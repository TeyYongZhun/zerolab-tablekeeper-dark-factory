package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (s *server) registerStage4(mux *http.ServeMux) {
	// /replans is the documented path; /replots is accepted as an alias.
	for _, base := range []string{"replans", "replots"} {
		mux.HandleFunc("POST /restaurants/{id}/"+base, s.createPlan)
		mux.HandleFunc("POST /restaurants/{id}/"+base+"/{plan_id}/apply", s.applyPlan)
	}
	mux.HandleFunc("POST /series/{id}/amend", s.amendSeries)
}

// bumpRevision increments the restaurant revision. Plans are only valid for
// the revision they were computed at, so every change to bookings, policies
// or seating moves it.
func bumpRevision(q querier, restID string) error {
	_, err := q.Exec(`UPDATE restaurants SET revision=revision+1 WHERE id=?`, restID)
	return err
}

// ---- seating replans ----

// planOption is a table set a booking may be assigned to.
type planOption struct {
	ids  []string
	rank int
}

type planBooking struct {
	v       *reservation
	options []planOption // feasible, in rank order
}

type planAssignment struct {
	Reference string   `json:"reference"`
	TableIDs  []string `json:"table_ids"`
	Changed   bool     `json:"changed"`
}

type planClosure struct {
	TableID string `json:"table_id"`
	From    string `json:"from"`
	To      string `json:"to"`
}

func sharesTable(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func requireManager(q querier, rest *restaurant, uid string) *apiErr {
	for _, m := range rest.Managers {
		if m == uid {
			return nil
		}
	}
	return errf(403, "forbidden", "only restaurant managers can change seating")
}

func (s *server) createPlan(w http.ResponseWriter, r *http.Request) {
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	key, ae := idemKey(r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	f, hash, ae := readObject(r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	rest, err := loadRestaurant(tx, r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if ae := requireManager(tx, rest, uid); ae != nil {
		writeErr(w, ae)
		return
	}
	path := "/restaurants/" + rest.ID + "/replans"
	resp, found, ae := idemLookup(tx, key, uid, path, hash)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	if found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, resp)
		return
	}
	tableID, _, e1 := fieldString(f, "table_id", true)
	fromS, _, e2 := fieldString(f, "from", true)
	toS, _, e3 := fieldString(f, "to", true)
	for _, e := range []*apiErr{e1, e2, e3} {
		if e != nil {
			writeErr(w, e)
			return
		}
	}
	from, err1 := time.Parse(time.RFC3339, fromS)
	to, err2 := time.Parse(time.RFC3339, toS)
	if err1 != nil || err2 != nil || !from.Before(to) {
		writeErr(w, invalid("from and to must be RFC 3339 instants with from before to"))
		return
	}
	if findTable(rest, tableID) == nil {
		writeErr(w, errNotFound)
		return
	}
	if len(rest.Tables) > 6 || len(rest.Combos) > 4 {
		writeErr(w, errf(422, "planning_limit", "too many tables or combinable pairs to plan"))
		return
	}
	bookings, ae := loadPlanBookings(tx, rest, tableID, from.Unix(), to.Unix())
	if ae != nil {
		writeErr(w, ae)
		return
	}
	if len(bookings) > 6 {
		writeErr(w, errf(422, "planning_limit", "too many bookings to plan"))
		return
	}
	best, moved, unused, ok, err := solvePlan(tx, rest, tableID, bookings)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		writeErr(w, errf(409, "no_feasible_plan", "the affected bookings cannot all be seated"))
		return
	}
	assignments := []planAssignment{}
	for i, b := range bookings {
		assignments = append(assignments, planAssignment{Reference: b.v.Ref, TableIDs: best[i].ids, Changed: !sameSet(best[i].ids, b.v.TableIDs)})
	}
	pid := newID("plan_")
	closure := planClosure{TableID: tableID, From: fromS, To: toS}
	out := map[string]any{
		"plan_id": pid, "restaurant_revision": rest.Revision, "closure": closure,
		"assignments": assignments, "moved_count": moved, "unused_seats": unused,
	}
	body, _ := json.Marshal(out)
	cj, _ := json.Marshal(closure)
	aj, _ := json.Marshal(assignments)
	if _, err := tx.Exec(`INSERT INTO plans(id,restaurant_id,revision,closure,assignments,moved_count,unused_seats) VALUES(?,?,?,?,?,?,?)`,
		pid, rest.ID, rest.Revision, string(cj), string(aj), moved, unused); err != nil {
		fail(w, err)
		return
	}
	if _, err := tx.Exec(`INSERT INTO idempotency_keys(key,user_id,request_path,request_body_hash,response_body,created_at) VALUES(?,?,?,?,?,?)`,
		key, uid, path, hash, string(body), nowStamp()); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	w.Write(append(body, '\n'))
}

// loadPlanBookings returns the confirmed bookings overlapping [from,to),
// ordered by reference, with the options each one could be seated at.
func loadPlanBookings(q querier, rest *restaurant, closedTable string, from, to int64) ([]*planBooking, *apiErr) {
	var list []*reservation
	err := scanAll(q, `SELECT `+resCols+` FROM reservations WHERE restaurant_id=? AND status='confirmed' AND starts_at<? AND ends_at>? ORDER BY reference`,
		func(rows *sql.Rows) error {
			v, err := scanRes(rows)
			list = append(list, v)
			return err
		}, rest.ID, to, from)
	if err != nil {
		return nil, errf(500, "internal_error", err.Error())
	}
	if err := fillTables(q, list...); err != nil {
		return nil, errf(500, "internal_error", err.Error())
	}
	out := []*planBooking{}
	for _, v := range list {
		pb := &planBooking{v: v}
		rank := 0
		for _, t := range rest.Tables {
			if t.ID != closedTable && v.Terms.capacity(t.ID) >= v.Party {
				pb.options = append(pb.options, planOption{ids: []string{t.ID}, rank: rank})
			}
			rank++
		}
		for _, c := range rest.Combos {
			if c[0] != closedTable && c[1] != closedTable && v.Terms.capacity(c[0])+v.Terms.capacity(c[1]) >= v.Party {
				pb.options = append(pb.options, planOption{ids: []string{c[0], c[1]}, rank: rank})
			}
			rank++
		}
		out = append(out, pb)
	}
	return out, nil
}

func optionCapacity(v *reservation, ids []string) int {
	c := 0
	for _, id := range ids {
		c += v.Terms.capacity(id)
	}
	return c
}

// solvePlan finds the assignment minimising (moved bookings, unused seats,
// rank vector by reference). Bookings are few (<=6) so it searches exhaustively.
func solvePlan(q querier, rest *restaurant, closedTable string, bs []*planBooking) ([]planOption, int, int, bool, error) {
	// options blocked by fixed bookings (confirmed bookings not being replanned) or earlier closures
	inPlan := map[string]bool{}
	for _, b := range bs {
		inPlan[b.v.ID] = true
	}
	for _, b := range bs {
		kept := b.options[:0:0]
		for _, o := range b.options {
			busy, err := occupiedByOthers(q, rest.ID, o.ids, b.v.Start, b.v.End, inPlan)
			if err != nil {
				return nil, 0, 0, false, err
			}
			if !busy {
				kept = append(kept, o)
			}
		}
		b.options = kept
	}
	n := len(bs)
	cur := make([]planOption, n)
	var best []planOption
	bestMoved, bestUnused := 0, 0
	var bestRanks []int
	better := func(moved, unused int, ranks []int) bool {
		if best == nil {
			return true
		}
		if moved != bestMoved {
			return moved < bestMoved
		}
		if unused != bestUnused {
			return unused < bestUnused
		}
		for i := range ranks {
			if ranks[i] != bestRanks[i] {
				return ranks[i] < bestRanks[i]
			}
		}
		return false
	}
	var dfs func(i int)
	dfs = func(i int) {
		if i == n {
			moved, unused := 0, 0
			ranks := make([]int, n)
			for j, b := range bs {
				if !sameSet(cur[j].ids, b.v.TableIDs) {
					moved++
				}
				unused += optionCapacity(b.v, cur[j].ids) - b.v.Party
				ranks[j] = cur[j].rank
			}
			if better(moved, unused, ranks) {
				best = append([]planOption{}, cur...)
				bestMoved, bestUnused, bestRanks = moved, unused, ranks
			}
			return
		}
		for _, o := range bs[i].options {
			clash := false
			for j := 0; j < i; j++ {
				if bs[j].v.Start < bs[i].v.End && bs[i].v.Start < bs[j].v.End && sharesTable(cur[j].ids, o.ids) {
					clash = true
					break
				}
			}
			if clash {
				continue
			}
			cur[i] = o
			dfs(i + 1)
		}
	}
	dfs(0)
	if best == nil {
		return nil, 0, 0, false, nil
	}
	return best, bestMoved, bestUnused, true, nil
}

// occupiedByOthers reports whether the tables are used, in [start,end), by a
// confirmed booking outside the plan, or are covered by an existing closure.
func occupiedByOthers(q querier, restID string, tableIDs []string, start, end int64, inPlan map[string]bool) (bool, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(tableIDs)), ",")
	args := []any{restID, end, start}
	for _, t := range tableIDs {
		args = append(args, t)
	}
	var ids []string
	err := scanAll(q, `SELECT DISTINCT r.id FROM reservations r JOIN reservation_tables rt ON rt.reservation_id=r.id
		WHERE r.restaurant_id=? AND r.status='confirmed' AND r.starts_at<? AND r.ends_at>? AND rt.table_id IN (`+ph+`)`,
		func(rows *sql.Rows) error {
			var id string
			err := rows.Scan(&id)
			ids = append(ids, id)
			return err
		}, args...)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if !inPlan[id] {
			return true, nil
		}
	}
	var one int
	cargs := []any{restID, end, start}
	for _, t := range tableIDs {
		cargs = append(cargs, t)
	}
	err = q.QueryRow(`SELECT 1 FROM closures WHERE restaurant_id=? AND from_unix<? AND to_unix>? AND table_id IN (`+ph+`) LIMIT 1`, cargs...).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *server) applyPlan(w http.ResponseWriter, r *http.Request) {
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	key, ae := idemKey(r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	_, hash, ae := readObject(r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	rest, err := loadRestaurant(tx, r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if ae := requireManager(tx, rest, uid); ae != nil {
		writeErr(w, ae)
		return
	}
	pid := r.PathValue("plan_id")
	path := "/restaurants/" + rest.ID + "/replans/" + pid + "/apply"
	resp, found, ae := idemLookup(tx, key, uid, path, hash)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	if found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, resp)
		return
	}
	var planRev, applied int
	var cj, aj string
	err = tx.QueryRow(`SELECT revision,closure,assignments,applied FROM plans WHERE id=? AND restaurant_id=?`, pid, rest.ID).Scan(&planRev, &cj, &aj, &applied)
	if err == sql.ErrNoRows {
		writeErr(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if applied != 0 {
		writeErr(w, errf(409, "plan_already_applied", "this plan has already been applied"))
		return
	}
	if planRev != rest.Revision {
		writeErr(w, errf(409, "stale_plan", "the restaurant has changed since this plan was made"))
		return
	}
	var closure planClosure
	var assignments []planAssignment
	json.Unmarshal([]byte(cj), &closure)
	json.Unmarshal([]byte(aj), &assignments)
	from, _ := time.Parse(time.RFC3339, closure.From)
	to, _ := time.Parse(time.RFC3339, closure.To)
	if _, err := tx.Exec(`INSERT INTO closures(restaurant_id,table_id,from_unix,to_unix,from_text,to_text,plan_id) VALUES(?,?,?,?,?,?,?)`,
		rest.ID, closure.TableID, from.Unix(), to.Unix(), closure.From, closure.To, pid); err != nil {
		fail(w, err)
		return
	}
	// move every changed booking first, then verify no overlaps remain
	var all []*reservation
	for _, a := range assignments {
		v, ae := ownedByRef(tx, a.Reference)
		if ae != nil {
			fail(w, ae)
			return
		}
		all = append(all, v)
		if !a.Changed {
			continue
		}
		old := v.TableIDs
		v.Rev++
		if _, err := tx.Exec(`UPDATE reservations SET revision=? WHERE id=?`, v.Rev, v.ID); err != nil {
			fail(w, err)
			return
		}
		if err := setReservationTables(tx, v.ID, v.RestID, a.TableIDs); err != nil {
			fail(w, err)
			return
		}
		v.TableIDs = a.TableIDs
		changes := []map[string]any{{"field": "table_ids", "from": old, "to": a.TableIDs}}
		if err := recordHistoryPlan(tx, rest.Loc, v, "reassigned", changes, "", pid); err != nil {
			fail(w, err)
			return
		}
	}
	for _, v := range all {
		busy, err := overlapExists(tx, rest.ID, v.TableIDs, v.Start, v.End, v.ID)
		if err != nil {
			fail(w, err)
			return
		}
		if busy {
			writeErr(w, errf(409, "stale_plan", "the plan no longer fits the current bookings"))
			return
		}
	}
	if err := bumpRevision(tx, rest.ID); err != nil {
		fail(w, err)
		return
	}
	newRev := rest.Revision + 1
	out := []map[string]any{}
	for _, v := range all {
		out = append(out, v.json(rest.Loc))
	}
	body, _ := json.Marshal(map[string]any{"plan_id": pid, "restaurant_revision": newRev, "reservations": out})
	if _, err := tx.Exec(`UPDATE plans SET applied=1, apply_key=?, apply_response=? WHERE id=?`, key, string(body), pid); err != nil {
		fail(w, err)
		return
	}
	if _, err := tx.Exec(`INSERT INTO idempotency_keys(key,user_id,request_path,request_body_hash,response_body,created_at) VALUES(?,?,?,?,?,?)`,
		key, uid, path, hash, string(body), nowStamp()); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	w.Write(append(body, '\n'))
}

func ownedByRef(q querier, ref string) (*reservation, error) {
	v, err := scanRes(q.QueryRow(`SELECT `+resCols+` FROM reservations WHERE reference=?`, ref))
	if err != nil {
		return nil, err
	}
	return v, fillTables(q, v)
}

// ---- amending a recurring series ----

var localTimeRe = regexp.MustCompile(`^\d{2}:\d{2}$`)

func (s *server) amendSeries(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, errNotFound)
		return
	}
	key, ae := idemKey(r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	f, hash, ae := readObject(r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	expRev, _, e1 := fieldInt(f, "expected_revision", true)
	fromIdx, e2 := intAtLeast(f, "from_index", 0)
	lt, _, e3 := fieldString(f, "local_time", true)
	for _, e := range []*apiErr{e1, e2, e3} {
		if e != nil {
			writeErr(w, e)
			return
		}
	}
	if _, ok := parseHHMM(lt); !ok || !localTimeRe.MatchString(lt) {
		writeErr(w, invalid("local_time must be HH:MM between 00:00 and 23:59"))
		return
	}
	sid := r.PathValue("id")
	tx, err := s.begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	var owner string
	var serRev int
	if err := tx.QueryRow(`SELECT user_id,revision FROM series WHERE id=?`, sid).Scan(&owner, &serRev); err != nil || owner != uid {
		writeErr(w, errNotFound)
		return
	}
	path := "/series/" + sid + "/amend"
	resp, found, ae := idemLookup(tx, key, uid, path, hash)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	if found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, resp)
		return
	}
	type occ struct {
		idx       int
		resID     string
		exception bool
	}
	var occs []occ
	err = scanAll(tx, `SELECT idx,reservation_id,exception FROM series_occurrences WHERE series_id=? ORDER BY idx`, func(rows *sql.Rows) error {
		var o occ
		var ex int
		err := rows.Scan(&o.idx, &o.resID, &ex)
		o.exception = ex != 0
		occs = append(occs, o)
		return err
	}, sid)
	if err != nil {
		fail(w, err)
		return
	}
	if fromIdx > len(occs)-1 {
		writeErr(w, invalid("from_index must be between 0 and %d", len(occs)-1))
		return
	}
	if expRev != serRev {
		writeErr(w, errf(409, "stale_revision", "the series has changed since that revision"))
		return
	}
	var restID string
	type change struct {
		v  *reservation
		am *amended
	}
	var changes []change
	var rest *restaurant
	// pass 1: everything except occupancy, in occurrence order
	for _, o := range occs {
		v, err := scanRes(tx.QueryRow(`SELECT `+resCols+` FROM reservations WHERE id=?`, o.resID))
		if err != nil {
			fail(w, err)
			return
		}
		if err := fillTables(tx, v); err != nil {
			fail(w, err)
			return
		}
		if rest == nil {
			restID = v.RestID
			if rest, err = loadRestaurant(tx, restID); err != nil {
				fail(w, err)
				return
			}
		}
		if o.idx < fromIdx || o.exception || v.Status == "cancelled" {
			continue
		}
		cur := fmtLocal(time.Unix(v.Start, 0), rest.Loc)
		newLocal := cur[:11] + lt
		if newLocal == cur {
			continue
		}
		if cutoffPassed(rest, v) {
			writeErr(w, errf(409, "cutoff_passed", "occurrence "+strconv.Itoa(o.idx)+" is within its cutoff window"))
			return
		}
		am, ae := resolveAmend(rest, v, &amendment{StartLocal: newLocal, hasStart: true})
		if ae != nil {
			writeErr(w, ae)
			return
		}
		changes = append(changes, change{v, am})
	}
	// pass 2: apply, then check occupancy
	for _, c := range changes {
		if err := commitAmendSeries(tx, rest, c.v, c.am); err != nil {
			fail(w, err)
			return
		}
	}
	for _, c := range changes {
		busy, err := overlapExists(tx, c.v.RestID, c.v.TableIDs, c.v.Start, c.v.End, c.v.ID)
		if err != nil {
			fail(w, err)
			return
		}
		if busy {
			writeErr(w, errf(409, "table_unavailable", "an amended occurrence collides with another booking"))
			return
		}
	}
	if len(changes) > 0 {
		if _, err := tx.Exec(`UPDATE series SET revision=revision+1 WHERE id=?`, sid); err != nil {
			fail(w, err)
			return
		}
		if err := bumpRevision(tx, restID); err != nil {
			fail(w, err)
			return
		}
	}
	body, ae := seriesJSON(tx, sid)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	bj, _ := json.Marshal(body)
	if _, err := tx.Exec(`INSERT INTO idempotency_keys(key,user_id,request_path,request_body_hash,response_body,created_at) VALUES(?,?,?,?,?,?)`,
		key, uid, path, hash, string(bj), nowStamp()); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, body)
}

// bumpOrFail bumps the restaurant revision, writing a 500 on failure.
func bumpOrFail(w http.ResponseWriter, q querier, restID string) bool {
	if err := bumpRevision(q, restID); err != nil {
		fail(w, err)
		return false
	}
	return true
}
