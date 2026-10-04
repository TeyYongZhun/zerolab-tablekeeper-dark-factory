package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *server) registerStage3(mux *http.ServeMux) {
	mux.HandleFunc("GET /reservations/{reference}/history", s.history)
	mux.HandleFunc("GET /reservations/{reference}/decision", s.decision)
	mux.HandleFunc("POST /restaurants/{id}/policies", s.publishPolicy)
	mux.HandleFunc("GET /restaurants/{id}/policies", s.listPolicies)
	mux.HandleFunc("POST /series", s.createSeries)
	mux.HandleFunc("GET /series/{id}", s.getSeries)
}

// ownedOr404 resolves the caller's own reservation; anything else (including a
// missing or bad token) is a plain 404 so existence never leaks.
func (s *server) ownedOr404(q querier, r *http.Request) (*reservation, *apiErr) {
	uid, ae := s.authUser(q, r)
	if ae != nil {
		return nil, errNotFound
	}
	return ownedReservation(q, uid, r.PathValue("reference"))
}

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ae := s.ownedOr404(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	type entry struct {
		Seq           int             `json:"seq"`
		At            string          `json:"at"`
		Event         string          `json:"event"`
		Revision      int             `json:"revision"`
		AcceptedTerms json.RawMessage `json:"accepted_terms"`
		Changes       json.RawMessage `json:"changes"`
		PlanID        string          `json:"plan_id,omitempty"`
	}
	entries := []entry{}
	err := scanAll(s.db, `SELECT seq,at,event,revision,terms,changes,plan_id FROM reservation_history WHERE reservation_id=? ORDER BY seq`, func(rows *sql.Rows) error {
		var e entry
		var tj, cj string
		if err := rows.Scan(&e.Seq, &e.At, &e.Event, &e.Revision, &tj, &cj, &e.PlanID); err != nil {
			return err
		}
		e.AcceptedTerms, e.Changes = json.RawMessage(tj), json.RawMessage(cj)
		entries = append(entries, e)
		return nil
	}, v.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"reference": v.Ref, "entries": entries})
}

func (s *server) decision(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ae := s.ownedOr404(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	writeJSON(w, 200, map[string]any{"reference": v.Ref, "revision": v.Rev, "accepted_terms": v.Terms})
}

// ---- policies ----

func intAtLeast(f map[string]json.RawMessage, name string, min int) (int, *apiErr) {
	raw, ok := f[name]
	if !ok {
		return 0, invalid("%s is required", name)
	}
	t := strings.TrimSpace(string(raw))
	if !intRe.MatchString(t) {
		return 0, invalid("%s must be an integer", name)
	}
	n, _ := strconv.Atoi(t)
	if n < min {
		return 0, invalid("%s must be at least %d", name, min)
	}
	return n, nil
}

func (s *server) publishPolicy(w http.ResponseWriter, r *http.Request) {
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
	manager := false
	for _, m := range rest.Managers {
		if m == uid {
			manager = true
		}
	}
	if !manager {
		writeErr(w, errf(403, "forbidden", "only restaurant managers can publish policies"))
		return
	}
	path := "/restaurants/" + rest.ID + "/policies"
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
	p, ae := parsePolicy(rest, f)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	maxV := 0
	for _, x := range rest.Policies {
		if x.Version > maxV {
			maxV = x.Version
		}
	}
	p.Version = maxV + 1
	if _, err := tx.Exec(`INSERT INTO policies(restaurant_id,policy_version,effective_from,body) VALUES(?,?,?,?)`,
		rest.ID, p.Version, p.EffectiveFrom, p.terms.json()); err != nil {
		fail(w, err)
		return
	}
	if !bumpOrFail(w, tx, rest.ID) {
		return
	}
	body, _ := json.Marshal(p)
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

func parsePolicy(rest *restaurant, f map[string]json.RawMessage) (*policy, *apiErr) {
	p := &policy{}
	eff, _, ae := fieldString(f, "effective_from", true)
	if ae != nil {
		return nil, ae
	}
	if !dateRe.MatchString(eff) {
		return nil, invalid("effective_from must be YYYY-MM-DD")
	}
	if _, err := time.Parse("2006-01-02", eff); err != nil {
		return nil, invalid("effective_from is not a valid date")
	}
	p.EffectiveFrom = eff
	var e *apiErr
	if p.Slot, e = intAtLeast(f, "slot_minutes", 1); e != nil {
		return nil, e
	}
	if p.Duration, e = intAtLeast(f, "reservation_duration_minutes", 1); e != nil {
		return nil, e
	}
	if p.Cutoff, e = intAtLeast(f, "cancellation_cutoff_minutes", 0); e != nil {
		return nil, e
	}
	rawHours, ok := f["opening_hours"]
	if !ok {
		return nil, invalid("opening_hours is required")
	}
	var hours []openHours
	if json.Unmarshal(rawHours, &hours) != nil {
		return nil, invalid("opening_hours must be an array of {weekday, opens, closes}")
	}
	if hours == nil {
		hours = []openHours{}
	}
	for _, h := range hours {
		o, ok1 := parseHHMM(h.Opens)
		c, ok2 := parseHHMM(h.Closes)
		if !validWeekday(h.Weekday) || !ok1 || !ok2 || c <= o {
			return nil, invalid("opening_hours contains an invalid window")
		}
	}
	p.Hours = hours
	rawCaps, ok := f["capacities"]
	if !ok {
		return nil, invalid("capacities is required")
	}
	var caps map[string]json.RawMessage
	if json.Unmarshal(rawCaps, &caps) != nil || caps == nil {
		return nil, invalid("capacities must be an object of table id to capacity")
	}
	merged := map[string]int{}
	for _, t := range rest.Tables {
		merged[t.ID] = t.Capacity
	}
	for id, raw := range caps {
		t := strings.TrimSpace(string(raw))
		if !intRe.MatchString(t) {
			return nil, invalid("capacities must be positive integers")
		}
		n, _ := strconv.Atoi(t)
		if n < 1 {
			return nil, invalid("capacities must be positive integers")
		}
		merged[id] = n
	}
	p.Capacities = merged
	return p, nil
}

func (s *server) listPolicies(w http.ResponseWriter, r *http.Request) {
	rest, err := loadRestaurant(s.db, r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	out := rest.Policies
	if out == nil {
		out = []policy{}
	}
	writeJSON(w, 200, map[string]any{"policies": out})
}

// ---- recurring series ----

func (s *server) createSeries(w http.ResponseWriter, r *http.Request) {
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
	ref, _, ae1 := fieldString(f, "anchor_reference", true)
	count, _, ae2 := fieldInt(f, "count", true)
	weeks, _, ae3 := fieldInt(f, "interval_weeks", true)
	for _, e := range []*apiErr{ae1, ae2, ae3} {
		if e != nil {
			writeErr(w, e)
			return
		}
	}
	if count < 2 || count > 12 {
		writeErr(w, invalid("count must be between 2 and 12"))
		return
	}
	if weeks < 1 || weeks > 4 {
		writeErr(w, invalid("interval_weeks must be between 1 and 4"))
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
	if resp, found, ae := idemLookup(tx, key, uid, "/series", hash); ae != nil {
		writeErr(w, ae)
		return
	} else if found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, resp)
		return
	}
	anchor, ae := ownedReservation(tx, uid, ref)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	if anchor.Status == "cancelled" {
		writeErr(w, errf(409, "reservation_cancelled", "the anchor reservation is cancelled"))
		return
	}
	rest, err := loadRestaurant(tx, anchor.RestID)
	if err != nil {
		fail(w, err)
		return
	}
	if cutoffPassed(rest, anchor) {
		writeErr(w, errf(409, "cutoff_passed", "the anchor reservation is within its cutoff window"))
		return
	}
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM series_occurrences WHERE reservation_id=?`, anchor.ID).Scan(&one); err == nil {
		writeErr(w, errf(409, "already_in_series", "the anchor reservation already belongs to a series"))
		return
	}
	sid := newID("ser_")
	if _, err := tx.Exec(`INSERT INTO series(id,user_id,interval_weeks,revision) VALUES(?,?,?,1)`, sid, uid, weeks); err != nil {
		fail(w, err)
		return
	}
	if _, err := tx.Exec(`INSERT INTO series_occurrences(series_id,idx,reservation_id,exception) VALUES(?,0,?,0)`, sid, anchor.ID); err != nil {
		fail(w, err)
		return
	}
	anchorLocal := fmtLocal(time.Unix(anchor.Start, 0), rest.Loc)
	anchorDay, _ := time.Parse("2006-01-02", anchorLocal[:10])
	for i := 1; i < count; i++ {
		day := anchorDay.AddDate(0, 0, i*weeks*7).Format("2006-01-02")
		startLocal := day + anchorLocal[10:]
		tm := rest.termsFor(day)
		ids, capacity, ae := resolveTables(rest, tm, anchor.TableIDs)
		if ae != nil {
			writeErr(w, ae)
			return
		}
		start, end, ae := checkBooking(rest, tm, capacity, bookingInput{ids, startLocal, anchor.Party})
		if ae != nil {
			writeErr(w, ae)
			return
		}
		busy, err := overlapExists(tx, rest.ID, ids, start.Unix(), end.Unix(), "")
		if err != nil {
			fail(w, err)
			return
		}
		if busy {
			writeErr(w, errf(409, "table_unavailable", "a table is already booked for occurrence "+strconv.Itoa(i)))
			return
		}
		nref, err := newReference(tx)
		if err != nil {
			fail(w, err)
			return
		}
		v := &reservation{ID: newID("res_"), Ref: nref, UserID: uid, RestID: rest.ID, TableIDs: ids, Party: anchor.Party,
			Status: "confirmed", Start: start.Unix(), End: end.Unix(), CreatedAt: nowStamp(), Rev: 1, Terms: tm}
		if err := insertReservation(tx, v); err != nil {
			fail(w, err)
			return
		}
		if err := recordHistory(tx, rest.Loc, v, "created", buildChanges(false, nil, "", 0, ids, startLocal, v.Party), ""); err != nil {
			fail(w, err)
			return
		}
		if _, err := tx.Exec(`INSERT INTO series_occurrences(series_id,idx,reservation_id,exception) VALUES(?,?,?,0)`, sid, i, v.ID); err != nil {
			fail(w, err)
			return
		}
	}
	body, ae := seriesJSON(tx, sid)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	if !bumpOrFail(w, tx, rest.ID) {
		return
	}
	bj, _ := json.Marshal(body)
	if _, err := tx.Exec(`INSERT INTO idempotency_keys(key,user_id,request_path,request_body_hash,response_body,created_at) VALUES(?,?,?,?,?,?)`,
		key, uid, "/series", hash, string(bj), nowStamp()); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, body)
}

func (s *server) getSeries(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, errNotFound)
		return
	}
	var owner string
	if err := s.db.QueryRow(`SELECT user_id FROM series WHERE id=?`, r.PathValue("id")).Scan(&owner); err != nil || owner != uid {
		writeErr(w, errNotFound)
		return
	}
	body, ae := seriesJSON(s.db, r.PathValue("id"))
	if ae != nil {
		writeErr(w, ae)
		return
	}
	writeJSON(w, 200, body)
}

func seriesJSON(q querier, id string) (map[string]any, *apiErr) {
	var weeks, rev int
	if err := q.QueryRow(`SELECT interval_weeks,revision FROM series WHERE id=?`, id).Scan(&weeks, &rev); err != nil {
		return nil, errNotFound
	}
	type occ struct {
		idx       int
		resID     string
		exception bool
	}
	var occs []occ
	err := scanAll(q, `SELECT idx,reservation_id,exception FROM series_occurrences WHERE series_id=? ORDER BY idx`, func(rows *sql.Rows) error {
		var o occ
		var ex int
		if err := rows.Scan(&o.idx, &o.resID, &ex); err != nil {
			return err
		}
		o.exception = ex != 0
		occs = append(occs, o)
		return nil
	}, id)
	if err != nil {
		return nil, errf(500, "internal_error", err.Error())
	}
	cache := locCache{}
	out := []map[string]any{}
	for _, o := range occs {
		v, err := scanRes(q.QueryRow(`SELECT `+resCols+` FROM reservations WHERE id=?`, o.resID))
		if err != nil {
			return nil, errf(500, "internal_error", err.Error())
		}
		if err := fillTables(q, v); err != nil {
			return nil, errf(500, "internal_error", err.Error())
		}
		j, err := (&server{}).resJSON(q, v, cache)
		if err != nil {
			return nil, errf(500, "internal_error", err.Error())
		}
		out = append(out, map[string]any{"index": o.idx, "reference": v.Ref, "exception": o.exception, "reservation": j})
	}
	return map[string]any{"series_id": id, "revision": rev, "interval_weeks": weeks, "count": len(out), "occurrences": out}, nil
}
