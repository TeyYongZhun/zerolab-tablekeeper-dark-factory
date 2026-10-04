package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

type apiErr struct {
	Status int
	Code   string
	Msg    string
}

func (e *apiErr) Error() string { return e.Code + ": " + e.Msg }

func errf(status int, code, msg string) *apiErr { return &apiErr{status, code, msg} }

var (
	errUnauth    = errf(401, "unauthenticated", "missing or invalid credentials")
	errNotFound  = errf(404, "not_found", "resource not found")
	errMalformed = errf(400, "malformed_request", "request body must be a JSON object")
)

type server struct {
	db *sql.DB
	mu sync.Mutex // serialises all mutating requests
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *apiErr) {
	writeJSON(w, e.Status, map[string]any{"error": map[string]string{"code": e.Code, "message": e.Msg}})
}

// fail maps any error to a response; unknown errors become 500.
func fail(w http.ResponseWriter, err error) {
	var ae *apiErr
	if errors.As(err, &ae) {
		writeErr(w, ae)
		return
	}
	log.Printf("internal error: %v", err)
	writeErr(w, errf(500, "internal_error", "internal error"))
}

func newID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

const refAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func newReference(q querier) (string, error) {
	for i := 0; i < 20; i++ {
		b := make([]byte, 8)
		for j := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(refAlphabet))))
			if err != nil {
				return "", err
			}
			b[j] = refAlphabet[n.Int64()]
		}
		ref := string(b)
		var one int
		err := q.QueryRow(`SELECT 1 FROM reservations WHERE reference=?`, ref).Scan(&one)
		if err == sql.ErrNoRows {
			return ref, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not generate reference")
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// ---- request body helpers ----

// readObject reads a JSON object body. It returns the raw fields and a
// canonical hash of the body (insensitive to whitespace and key order).
func readObject(r *http.Request) (map[string]json.RawMessage, string, *apiErr) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, "", errMalformed
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, "", errMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var generic any
	dec.Decode(&generic)
	canon, _ := json.Marshal(generic)
	sum := sha256.Sum256(canon)
	return fields, hex.EncodeToString(sum[:]), nil
}

func fieldString(f map[string]json.RawMessage, name string, required bool) (string, bool, *apiErr) {
	raw, ok := f[name]
	if !ok {
		if required {
			return "", false, invalid("%s is required", name)
		}
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", false, invalid("%s must be a non-empty string", name)
	}
	return s, true, nil
}

var intRe = regexp.MustCompile(`^-?[0-9]{1,9}$`)

func fieldInt(f map[string]json.RawMessage, name string, required bool) (int, bool, *apiErr) {
	raw, ok := f[name]
	if !ok {
		if required {
			return 0, false, invalid("%s is required", name)
		}
		return 0, false, nil
	}
	t := strings.TrimSpace(string(raw))
	if !intRe.MatchString(t) {
		return 0, false, invalid("%s must be an integer", name)
	}
	n, _ := strconv.Atoi(t)
	if n < 1 {
		return 0, false, invalid("%s must be at least 1", name)
	}
	return n, true, nil
}

// ---- auth ----

func (s *server) authUser(q querier, r *http.Request) (string, *apiErr) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", errUnauth
	}
	tok := strings.TrimSpace(h[len("Bearer "):])
	if tok == "" {
		return "", errUnauth
	}
	var uid string
	if err := q.QueryRow(`SELECT user_id FROM tokens WHERE token=?`, tok).Scan(&uid); err != nil {
		return "", errUnauth
	}
	return uid, nil
}

func newToken(q querier, uid string) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	_, err := q.Exec(`INSERT INTO tokens(token,user_id,created_at) VALUES(?,?,?)`, tok, uid, nowStamp())
	return tok, err
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)

func (s *server) signup(w http.ResponseWriter, r *http.Request) {
	f, _, ae := readObject(r)
	if ae == nil {
		ae = requireStrings(f, "email", "password", "display_name")
	}
	if ae != nil {
		writeErr(w, ae)
		return
	}
	email, _, ae := fieldString(f, "email", true)
	if ae == nil {
		_, _, ae = fieldString(f, "password", true)
	}
	if ae == nil {
		_, _, ae = fieldString(f, "display_name", true)
	}
	if ae != nil {
		writeErr(w, ae)
		return
	}
	password, _, _ := fieldString(f, "password", true)
	display, _, _ := fieldString(f, "display_name", true)
	email = normEmail(email)
	if !emailRe.MatchString(email) {
		writeErr(w, invalid("email must be of the form local@domain"))
		return
	}
	if len([]rune(password)) < 8 {
		writeErr(w, invalid("password must be at least 8 characters"))
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		fail(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE email=?`, email).Scan(&one); err == nil {
		writeErr(w, errf(409, "email_taken", "email already registered"))
		return
	}
	uid := newID("u_")
	if _, err := tx.Exec(`INSERT INTO users(id,email,password_hash,display_name) VALUES(?,?,?,?)`, uid, email, hash, display); err != nil {
		fail(w, err)
		return
	}
	tok, err := newToken(tx, uid)
	if err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"user_id": uid, "display_name": display, "token": tok})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	f, _, ae := readObject(r)
	if ae == nil {
		ae = requireStrings(f, "email", "password")
	}
	if ae != nil {
		writeErr(w, ae)
		return
	}
	email, _, ae1 := fieldString(f, "email", true)
	password, _, ae2 := fieldString(f, "password", true)
	if ae1 != nil || ae2 != nil {
		writeErr(w, errUnauth)
		return
	}
	var uid, hash, display string
	err := s.db.QueryRow(`SELECT id,password_hash,display_name FROM users WHERE email=?`, normEmail(email)).Scan(&uid, &hash, &display)
	if err != nil || !checkPassword(hash, password) {
		writeErr(w, errUnauth)
		return
	}
	s.mu.Lock()
	tok, err := newToken(s.db, uid)
	s.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"user_id": uid, "display_name": display, "token": tok})
}

// ---- public ----

func (s *server) listRestaurants(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id,name,timezone FROM restaurants ORDER BY rowid`)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, tz string
		if err := rows.Scan(&id, &name, &tz); err != nil {
			fail(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "name": name, "timezone": tz})
	}
	writeJSON(w, 200, map[string]any{"restaurants": out})
}

func (s *server) getRestaurant(w http.ResponseWriter, r *http.Request) {
	rest, err := loadRestaurant(s.db, r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	hours := rest.Hours
	if hours == nil {
		hours = []openHours{}
	}
	tables := rest.Tables
	if tables == nil {
		tables = []tableRow{}
	}
	writeJSON(w, 200, map[string]any{
		"id": rest.ID, "name": rest.Name, "timezone": rest.Timezone,
		"slot_minutes": rest.Slot, "reservation_duration_minutes": rest.Duration,
		"cancellation_cutoff_minutes": rest.Cutoff,
		"opening_hours":               hours, "tables": tables,
	})
}

var digitsRe = regexp.MustCompile(`^[0-9]{1,9}$`)

// window returns the opening window for a local date in minutes after midnight.
func (rest *restaurant) window(wall time.Time) (int, int, bool) {
	wd := weekdayNames[wall.Weekday()]
	for _, h := range rest.Hours {
		if h.Weekday == wd {
			o, _ := parseHHMM(h.Opens)
			c, _ := parseHHMM(h.Closes)
			return o, c, true
		}
	}
	return 0, 0, false
}

// closesAt returns the instant of the closing time on the date of wall.
func (rest *restaurant) closesAt(wall time.Time, closeMin int) time.Time {
	cw := time.Date(wall.Year(), wall.Month(), wall.Day(), closeMin/60, closeMin%60, 0, 0, time.UTC)
	if t, ok := resolveWall(rest.Loc, cw); ok {
		return t
	}
	return time.Date(wall.Year(), wall.Month(), wall.Day(), closeMin/60, closeMin%60, 0, 0, rest.Loc)
}

func (rest *restaurant) duration() time.Duration { return time.Duration(rest.Duration) * time.Minute }

func overlapExists(q querier, restID, tableID string, start, end int64, excludeID string) (bool, error) {
	var one int
	err := q.QueryRow(`SELECT 1 FROM reservations WHERE restaurant_id=? AND table_id=? AND status='confirmed' AND id<>? AND starts_at<? AND ends_at>? LIMIT 1`,
		restID, tableID, excludeID, end, start).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *server) availability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rid, date, ps := q.Get("restaurant_id"), q.Get("date"), q.Get("party_size")
	if rid == "" || date == "" || ps == "" {
		writeErr(w, invalid("restaurant_id, date and party_size are required"))
		return
	}
	if !digitsRe.MatchString(ps) {
		writeErr(w, invalid("party_size must be plain digits"))
		return
	}
	party, _ := strconv.Atoi(ps)
	if party < 1 {
		writeErr(w, invalid("party_size must be at least 1"))
		return
	}
	if !dateRe.MatchString(date) {
		writeErr(w, invalid("date must be YYYY-MM-DD"))
		return
	}
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		writeErr(w, invalid("date is not a valid calendar date"))
		return
	}
	rest, err := loadRestaurant(s.db, rid)
	if err == sql.ErrNoRows {
		writeErr(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	slots := []map[string]any{}
	if open, closeMin, ok := rest.window(day); ok {
		closeAt := rest.closesAt(day, closeMin)
		for m := open; m < closeMin; m += rest.Slot {
			wall := time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, time.UTC)
			start, ok := resolveWall(rest.Loc, wall)
			if !ok {
				continue
			}
			end := start.Add(rest.duration())
			if end.After(closeAt) {
				continue
			}
			ids := []string{}
			for _, t := range rest.Tables {
				if t.Capacity < party {
					continue
				}
				busy, err := overlapExists(s.db, rest.ID, t.ID, start.Unix(), end.Unix(), "")
				if err != nil {
					fail(w, err)
					return
				}
				if !busy {
					ids = append(ids, t.ID)
				}
			}
			slots = append(slots, map[string]any{
				"starts_at_local": fmtLocal(start, rest.Loc), "starts_at": fmtRFC(start, rest.Loc),
				"available_table_ids": ids,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"restaurant_id": rest.ID, "date": date, "timezone": rest.Timezone, "slots": slots})
}

// ---- reservations ----

type reservation struct {
	ID, Ref, UserID, RestID, TableID, Status, CreatedAt string
	Party                                               int
	Start, End                                          int64
}

const resCols = `id,reference,user_id,restaurant_id,table_id,party_size,status,starts_at,ends_at,created_at`

func scanRes(sc interface{ Scan(...any) error }) (*reservation, error) {
	v := &reservation{}
	err := sc.Scan(&v.ID, &v.Ref, &v.UserID, &v.RestID, &v.TableID, &v.Party, &v.Status, &v.Start, &v.End, &v.CreatedAt)
	return v, err
}

func (v *reservation) json(loc *time.Location) map[string]any {
	st := time.Unix(v.Start, 0)
	return map[string]any{
		"reservation_id": v.ID, "reference": v.Ref, "restaurant_id": v.RestID, "table_id": v.TableID,
		"party_size": v.Party, "status": v.Status,
		"starts_at_local": fmtLocal(st, loc), "starts_at": fmtRFC(st, loc),
		"ends_at": fmtRFC(time.Unix(v.End, 0), loc), "created_at": v.CreatedAt,
	}
}

type locCache map[string]*restaurant

func (c locCache) get(q querier, id string) (*restaurant, error) {
	if r, ok := c[id]; ok {
		return r, nil
	}
	r, err := loadRestaurant(q, id)
	if err == nil {
		c[id] = r
	}
	return r, err
}

func (s *server) resJSON(q querier, v *reservation, c locCache) (map[string]any, error) {
	rest, err := c.get(q, v.RestID)
	if err != nil {
		return nil, err
	}
	return v.json(rest.Loc), nil
}

// bookingInput holds the (possibly merged) values to validate.
type bookingInput struct {
	TableID    string
	StartLocal string
	Party      int
}

// checkBooking validates a booking against the restaurant rules and returns the
// absolute start/end times. The table must already belong to rest.
func checkBooking(rest *restaurant, tbl *tableRow, in bookingInput) (time.Time, time.Time, *apiErr) {
	wall, ok := parseWall(in.StartLocal)
	if !ok {
		return time.Time{}, time.Time{}, invalid("starts_at_local must be YYYY-MM-DDTHH:MM")
	}
	start, ok := resolveWall(rest.Loc, wall)
	if !ok {
		return time.Time{}, time.Time{}, errf(422, "invalid_local_time", "local time does not exist (DST gap)")
	}
	open, closeMin, ok := rest.window(wall)
	outside := errf(422, "outside_opening_hours", "outside opening hours")
	if !ok {
		return time.Time{}, time.Time{}, outside
	}
	m := wall.Hour()*60 + wall.Minute()
	if m < open || m >= closeMin {
		return time.Time{}, time.Time{}, outside
	}
	if (m-open)%rest.Slot != 0 {
		return time.Time{}, time.Time{}, errf(422, "not_on_slot_grid", "start time is not on the slot grid")
	}
	end := start.Add(rest.duration())
	if end.After(rest.closesAt(wall, closeMin)) {
		return time.Time{}, time.Time{}, outside
	}
	if in.Party > tbl.Capacity {
		return time.Time{}, time.Time{}, errf(422, "party_exceeds_capacity", "party size exceeds table capacity")
	}
	return start, end, nil
}

func findTable(rest *restaurant, id string) *tableRow {
	for i := range rest.Tables {
		if rest.Tables[i].ID == id {
			return &rest.Tables[i]
		}
	}
	return nil
}

// idempotency: returns stored response when the key was seen.
func idemLookup(q querier, key, uid, path, hash string) (string, bool, *apiErr) {
	var p, h, resp string
	err := q.QueryRow(`SELECT request_path,request_body_hash,response_body FROM idempotency_keys WHERE key=? AND user_id=?`, key, uid).Scan(&p, &h, &resp)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, errf(500, "internal_error", err.Error())
	}
	if p != path || h != hash {
		return "", false, errf(409, "idempotency_key_reuse", "idempotency key reused with a different request")
	}
	return resp, true, nil
}

func idemKey(r *http.Request) (string, *apiErr) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return "", errf(400, "missing_idempotency_key", "Idempotency-Key header is required")
	}
	if len(k) > 255 {
		return "", invalid("Idempotency-Key must be 1-255 characters")
	}
	return k, nil
}

func (s *server) begin() (*sql.Tx, error) { return s.db.Begin() }

func (s *server) createReservation(w http.ResponseWriter, r *http.Request) {
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
	resp, found, ae := idemLookup(tx, key, uid, "/reservations", hash)
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
	rid, _, ae1 := fieldString(f, "restaurant_id", true)
	tid, _, ae2 := fieldString(f, "table_id", true)
	startLocal, _, ae3 := fieldString(f, "starts_at_local", true)
	party, _, ae4 := fieldInt(f, "party_size", true)
	for _, e := range []*apiErr{ae1, ae2, ae3, ae4} {
		if e != nil {
			writeErr(w, e)
			return
		}
	}
	if _, ok := parseWall(startLocal); !ok {
		writeErr(w, invalid("starts_at_local must be YYYY-MM-DDTHH:MM"))
		return
	}
	rest, err := loadRestaurant(tx, rid)
	if err == sql.ErrNoRows {
		writeErr(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	tbl := findTable(rest, tid)
	if tbl == nil {
		writeErr(w, errNotFound)
		return
	}
	start, end, ae := checkBooking(rest, tbl, bookingInput{tid, startLocal, party})
	if ae != nil {
		writeErr(w, ae)
		return
	}
	busy, err := overlapExists(tx, rid, tid, start.Unix(), end.Unix(), "")
	if err != nil {
		fail(w, err)
		return
	}
	if busy {
		writeErr(w, errf(409, "table_unavailable", "table is already booked for that time"))
		return
	}
	ref, err := newReference(tx)
	if err != nil {
		fail(w, err)
		return
	}
	v := &reservation{ID: newID("res_"), Ref: ref, UserID: uid, RestID: rid, TableID: tid, Party: party,
		Status: "confirmed", Start: start.Unix(), End: end.Unix(), CreatedAt: nowStamp()}
	if _, err := tx.Exec(`INSERT INTO reservations(`+resCols+`) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.Ref, v.UserID, v.RestID, v.TableID, v.Party, v.Status, v.Start, v.End, v.CreatedAt); err != nil {
		fail(w, err)
		return
	}
	body, _ := json.Marshal(v.json(rest.Loc))
	if _, err := tx.Exec(`INSERT INTO idempotency_keys(key,user_id,request_path,request_body_hash,response_body,created_at) VALUES(?,?,?,?,?,?)`,
		key, uid, "/reservations", hash, string(body), nowStamp()); err != nil {
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

func (s *server) listReservations(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock() // consistent snapshot with the single connection
	defer s.mu.Unlock()
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	rows, err := s.db.Query(`SELECT `+resCols+` FROM reservations WHERE user_id=? ORDER BY starts_at DESC, rowid DESC`, uid)
	if err != nil {
		fail(w, err)
		return
	}
	var list []*reservation
	for rows.Next() {
		v, err := scanRes(rows)
		if err != nil {
			rows.Close()
			fail(w, err)
			return
		}
		list = append(list, v)
	}
	rows.Close()
	c := locCache{}
	out := []map[string]any{}
	for _, v := range list {
		j, err := s.resJSON(s.db, v, c)
		if err != nil {
			fail(w, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, 200, map[string]any{"reservations": out})
}

func ownedReservation(q querier, uid, ref string) (*reservation, *apiErr) {
	v, err := scanRes(q.QueryRow(`SELECT `+resCols+` FROM reservations WHERE reference=? AND user_id=?`, ref, uid))
	if err == sql.ErrNoRows {
		return nil, errNotFound
	}
	if err != nil {
		return nil, errf(500, "internal_error", err.Error())
	}
	return v, nil
}

func (s *server) getReservation(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	v, ae := ownedReservation(s.db, uid, r.PathValue("reference"))
	if ae != nil {
		writeErr(w, ae)
		return
	}
	j, err := s.resJSON(s.db, v, locCache{})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, j)
}

func cutoffPassed(rest *restaurant, v *reservation) bool {
	deadline := time.Unix(v.Start, 0).Add(-time.Duration(rest.Cutoff) * time.Minute)
	return time.Now().After(deadline)
}

func (s *server) cancelReservation(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	tx, err := s.begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	v, ae := ownedReservation(tx, uid, r.PathValue("reference"))
	if ae != nil {
		writeErr(w, ae)
		return
	}
	rest, err := loadRestaurant(tx, v.RestID)
	if err != nil {
		fail(w, err)
		return
	}
	if v.Status != "cancelled" {
		if cutoffPassed(rest, v) {
			writeErr(w, errf(409, "cutoff_passed", "within the cancellation cutoff window"))
			return
		}
		if _, err := tx.Exec(`UPDATE reservations SET status='cancelled' WHERE id=?`, v.ID); err != nil {
			fail(w, err)
			return
		}
		v.Status = "cancelled"
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v.json(rest.Loc))
}

// amendFields parses the optional amendment fields shared by PATCH and moves.
type amendment struct {
	TableID                      string
	StartLocal                   string
	Party                        int
	hasTable, hasStart, hasParty bool
}

func parseAmendment(f map[string]json.RawMessage) (*amendment, *apiErr) {
	a := &amendment{}
	var ae *apiErr
	if a.TableID, a.hasTable, ae = fieldString(f, "table_id", false); ae != nil {
		return nil, ae
	}
	if a.StartLocal, a.hasStart, ae = fieldString(f, "starts_at_local", false); ae != nil {
		return nil, ae
	}
	if a.hasStart {
		if _, ok := parseWall(a.StartLocal); !ok {
			return nil, invalid("starts_at_local must be YYYY-MM-DDTHH:MM")
		}
	}
	if a.Party, a.hasParty, ae = fieldInt(f, "party_size", false); ae != nil {
		return nil, ae
	}
	return a, nil
}

// resolveAmend merges an amendment into a reservation and validates it,
// returning the new values.
func resolveAmend(q querier, rest *restaurant, v *reservation, a *amendment) (bookingInput, time.Time, time.Time, *apiErr) {
	in := bookingInput{TableID: v.TableID, StartLocal: fmtLocal(time.Unix(v.Start, 0), rest.Loc), Party: v.Party}
	if a.hasTable {
		in.TableID = a.TableID
	}
	if a.hasStart {
		in.StartLocal = a.StartLocal
	}
	if a.hasParty {
		in.Party = a.Party
	}
	tbl := findTable(rest, in.TableID)
	if tbl == nil {
		return in, time.Time{}, time.Time{}, errNotFound
	}
	start, end, ae := checkBooking(rest, tbl, in)
	return in, start, end, ae
}

func (s *server) patchReservation(w http.ResponseWriter, r *http.Request) {
	uid, ae := s.authUser(s.db, r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	f, _, ae := readObject(r)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	a, ae := parseAmendment(f)
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
	v, ae := ownedReservation(tx, uid, r.PathValue("reference"))
	if ae != nil {
		writeErr(w, ae)
		return
	}
	rest, err := loadRestaurant(tx, v.RestID)
	if err != nil {
		fail(w, err)
		return
	}
	if v.Status == "cancelled" {
		writeErr(w, errf(409, "reservation_cancelled", "reservation is cancelled"))
		return
	}
	if cutoffPassed(rest, v) {
		writeErr(w, errf(409, "cutoff_passed", "within the amendment cutoff window"))
		return
	}
	in, start, end, ae := resolveAmend(tx, rest, v, a)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	busy, err := overlapExists(tx, v.RestID, in.TableID, start.Unix(), end.Unix(), v.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if busy {
		writeErr(w, errf(409, "table_unavailable", "table is already booked for that time"))
		return
	}
	if _, err := tx.Exec(`UPDATE reservations SET table_id=?,party_size=?,starts_at=?,ends_at=? WHERE id=?`,
		in.TableID, in.Party, start.Unix(), end.Unix(), v.ID); err != nil {
		fail(w, err)
		return
	}
	v.TableID, v.Party, v.Start, v.End = in.TableID, in.Party, start.Unix(), end.Unix()
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v.json(rest.Loc))
}

func (s *server) moves(w http.ResponseWriter, r *http.Request) {
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
	resp, found, ae := idemLookup(tx, key, uid, "/reservation-moves", hash)
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
	var rawMoves []json.RawMessage
	if err := json.Unmarshal(f["moves"], &rawMoves); err != nil || len(rawMoves) < 1 || len(rawMoves) > 8 {
		writeErr(w, invalid("moves must be an array of 1-8 items"))
		return
	}
	type move struct {
		ref string
		a   *amendment
	}
	var mv []move
	seen := map[string]bool{}
	for _, rm := range rawMoves {
		var mf map[string]json.RawMessage
		if err := json.Unmarshal(rm, &mf); err != nil || mf == nil {
			writeErr(w, invalid("each move must be an object"))
			return
		}
		ref, _, ae := fieldString(mf, "reference", true)
		if ae != nil {
			writeErr(w, ae)
			return
		}
		if seen[ref] {
			writeErr(w, invalid("duplicate reference %s", ref))
			return
		}
		seen[ref] = true
		a, ae := parseAmendment(mf)
		if ae != nil {
			writeErr(w, ae)
			return
		}
		mv = append(mv, move{ref, a})
	}
	vs := make([]*reservation, len(mv))
	for i, m := range mv {
		v, ae := ownedReservation(tx, uid, m.ref)
		if ae != nil {
			writeErr(w, ae)
			return
		}
		vs[i] = v
		if v.RestID != vs[0].RestID {
			writeErr(w, invalid("all moves must belong to one restaurant"))
			return
		}
	}
	rest, err := loadRestaurant(tx, vs[0].RestID)
	if err != nil {
		fail(w, err)
		return
	}
	for _, v := range vs {
		if v.Status == "cancelled" {
			writeErr(w, errf(409, "reservation_cancelled", "reservation "+v.Ref+" is cancelled"))
			return
		}
	}
	for _, v := range vs {
		if cutoffPassed(rest, v) {
			writeErr(w, errf(409, "cutoff_passed", "reservation "+v.Ref+" is within the cutoff window"))
			return
		}
	}
	type result struct {
		in         bookingInput
		start, end time.Time
	}
	res := make([]result, len(mv))
	for i, m := range mv {
		in, st, en, ae := resolveAmend(tx, rest, vs[i], m.a)
		if ae != nil {
			writeErr(w, ae)
			return
		}
		res[i] = result{in, st, en}
	}
	for i, v := range vs {
		if _, err := tx.Exec(`UPDATE reservations SET table_id=?,party_size=?,starts_at=?,ends_at=? WHERE id=?`,
			res[i].in.TableID, res[i].in.Party, res[i].start.Unix(), res[i].end.Unix(), v.ID); err != nil {
			fail(w, err)
			return
		}
		v.TableID, v.Party, v.Start, v.End = res[i].in.TableID, res[i].in.Party, res[i].start.Unix(), res[i].end.Unix()
	}
	for i, v := range vs {
		busy, err := overlapExists(tx, v.RestID, res[i].in.TableID, v.Start, v.End, v.ID)
		if err != nil {
			fail(w, err)
			return
		}
		if busy {
			writeErr(w, errf(409, "table_unavailable", "resulting booking overlaps another booking"))
			return
		}
	}
	out := []map[string]any{}
	for _, v := range vs {
		out = append(out, v.json(rest.Loc))
	}
	body, _ := json.Marshal(map[string]any{"reservations": out})
	if _, err := tx.Exec(`INSERT INTO idempotency_keys(key,user_id,request_path,request_body_hash,response_body,created_at) VALUES(?,?,?,?,?,?)`,
		key, uid, "/reservation-moves", hash, string(body), nowStamp()); err != nil {
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

// ---- test endpoints ----

func (s *server) reset(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	var fx fixture
	if err := json.Unmarshal(data, &fx); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			writeErr(w, invalid("fixture has a wrongly typed field: %v", err))
		} else {
			writeErr(w, errMalformed)
		}
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
	if err := applyFixture(tx, &fx); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *server) export(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := exportState(s.db)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"track": "tablekeeper", "format_version": 1, "state": st})
}

func (s *server) importHandler(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	var doc struct {
		Track         *string         `json:"track"`
		FormatVersion *json.Number    `json:"format_version"`
		State         json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			writeErr(w, invalid("import document has a wrongly typed field"))
		} else {
			writeErr(w, errMalformed)
		}
		return
	}
	if doc.Track == nil || *doc.Track != "tablekeeper" || doc.FormatVersion == nil || doc.FormatVersion.String() != "1" ||
		len(doc.State) == 0 || string(bytes.TrimSpace(doc.State)) == "null" {
		writeErr(w, invalid("unsupported track, format_version or missing state"))
		return
	}
	var st stateDoc
	if err := json.Unmarshal(doc.State, &st); err != nil {
		writeErr(w, invalid("state is malformed: %v", err))
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
	if err := importState(tx, &st); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /_test/reset", s.reset)
	mux.HandleFunc("GET /_test/export", s.export)
	mux.HandleFunc("POST /_test/import", s.importHandler)
	mux.HandleFunc("POST /auth/signup", s.signup)
	mux.HandleFunc("POST /auth/login", s.login)
	mux.HandleFunc("GET /restaurants", s.listRestaurants)
	mux.HandleFunc("GET /restaurants/{id}", s.getRestaurant)
	mux.HandleFunc("GET /availability", s.availability)
	mux.HandleFunc("POST /reservations", s.createReservation)
	mux.HandleFunc("GET /reservations", s.listReservations)
	mux.HandleFunc("GET /reservations/{reference}", s.getReservation)
	mux.HandleFunc("POST /reservations/{reference}/cancel", s.cancelReservation)
	mux.HandleFunc("PATCH /reservations/{reference}", s.patchReservation)
	mux.HandleFunc("POST /reservation-moves", s.moves)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, errNotFound)
	})
	return mux
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "/tmp/tablekeeper.db"
	}
	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	srv := &server{db: db}
	hs := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
	log.Printf("listening on :%s", port)
	log.Fatal(hs.ListenAndServe())
}

// requireStrings returns 400 malformed_request when a present field is not a
// JSON string (a wrong type is a malformed request; missing/empty is a
// validation failure handled by the caller).
func requireStrings(f map[string]json.RawMessage, names ...string) *apiErr {
	for _, n := range names {
		if raw, ok := f[n]; ok {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return errf(400, "malformed_request", n+" must be a string")
			}
		}
	}
	return nil
}
