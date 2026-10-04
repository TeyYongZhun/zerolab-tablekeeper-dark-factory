package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS restaurants (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    timezone TEXT NOT NULL,
    slot_minutes INTEGER NOT NULL,
    reservation_duration_minutes INTEGER NOT NULL,
    cancellation_cutoff_minutes INTEGER NOT NULL,
    combinable TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS opening_hours (
    id INTEGER PRIMARY KEY,
    restaurant_id TEXT NOT NULL REFERENCES restaurants(id),
    weekday TEXT NOT NULL,
    opens TEXT NOT NULL,
    closes TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tables (
    id TEXT NOT NULL,
    restaurant_id TEXT NOT NULL REFERENCES restaurants(id),
    label TEXT NOT NULL,
    capacity INTEGER NOT NULL,
    PRIMARY KEY (restaurant_id, id)
);
CREATE TABLE IF NOT EXISTS reservations (
    id TEXT PRIMARY KEY,
    reference TEXT UNIQUE NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id),
    restaurant_id TEXT NOT NULL REFERENCES restaurants(id),
    party_size INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'confirmed',
    starts_at INTEGER NOT NULL,
    ends_at INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reservation_tables (
    reservation_id TEXT NOT NULL REFERENCES reservations(id),
    restaurant_id TEXT NOT NULL,
    table_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    PRIMARY KEY (reservation_id, table_id),
    FOREIGN KEY (restaurant_id, table_id) REFERENCES tables(restaurant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_resv_tables_lookup ON reservation_tables(restaurant_id, table_id);
CREATE INDEX IF NOT EXISTS idx_reservations_user ON reservations(user_id);
CREATE TABLE IF NOT EXISTS tokens (
    token TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id),
    request_path TEXT NOT NULL,
    request_body_hash TEXT NOT NULL,
    response_body TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (key, user_id)
);
`

// delete order respects foreign keys.
var allTables = []string{"idempotency_keys", "tokens", "reservation_tables", "reservations", "opening_hours", "tables", "restaurants", "users"}

type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func openDB(path string) (*sql.DB, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises every statement; writers are further
	// serialised by the server mutex so a transaction never waits on itself.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return db, nil
}

func nowStamp() string { return time.Now().UTC().Format(rfcLayout) }

// hashPassword pre-hashes with SHA-256 so passwords longer than bcrypt's
// 72-byte limit are fully significant and never rejected.
func pwPrehash(p string) []byte {
	sum := sha256.Sum256([]byte(p))
	return []byte(base64.StdEncoding.EncodeToString(sum[:]))
}

func hashPassword(p string) (string, error) {
	h, err := bcrypt.GenerateFromPassword(pwPrehash(p), bcrypt.DefaultCost)
	return string(h), err
}

func checkPassword(hash, p string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), pwPrehash(p)) == nil
}

// ---- domain loaders ----

type openHours struct {
	Weekday string `json:"weekday"`
	Opens   string `json:"opens"`
	Closes  string `json:"closes"`
}

type tableRow struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Capacity int    `json:"capacity"`
}

type restaurant struct {
	ID       string
	Name     string
	Timezone string
	Slot     int
	Duration int
	Cutoff   int
	Loc      *time.Location
	Hours    []openHours
	Tables   []tableRow
	Combos   [][]string
}

func loadRestaurant(q querier, id string) (*restaurant, error) {
	r := &restaurant{}
	var combRaw string
	err := q.QueryRow(`SELECT id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes,combinable FROM restaurants WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Timezone, &r.Slot, &r.Duration, &r.Cutoff, &combRaw)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(combRaw), &r.Combos)
	r.Loc, err = time.LoadLocation(r.Timezone)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(`SELECT weekday,opens,closes FROM opening_hours WHERE restaurant_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h openHours
		if err := rows.Scan(&h.Weekday, &h.Opens, &h.Closes); err != nil {
			rows.Close()
			return nil, err
		}
		r.Hours = append(r.Hours, h)
	}
	rows.Close()
	rows, err = q.Query(`SELECT id,label,capacity FROM tables WHERE restaurant_id=? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t tableRow
		if err := rows.Scan(&t.ID, &t.Label, &t.Capacity); err != nil {
			rows.Close()
			return nil, err
		}
		r.Tables = append(r.Tables, t)
	}
	rows.Close()
	return r, nil
}

// ---- fixture / state replacement ----

type fixtureUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type fixtureRestaurant struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Timezone   string      `json:"timezone"`
	Slot       int         `json:"slot_minutes"`
	Duration   int         `json:"reservation_duration_minutes"`
	Cutoff     int         `json:"cancellation_cutoff_minutes"`
	OpeningHrs []openHours `json:"opening_hours"`
	Tables     []tableRow  `json:"tables"`
	Combinable [][]string  `json:"combinable"`
}

type fixtureReservation struct {
	ID           string   `json:"id"`
	Reference    string   `json:"reference"`
	UserID       string   `json:"user_id"`
	RestaurantID string   `json:"restaurant_id"`
	TableID      string   `json:"table_id"`
	TableIDs     []string `json:"table_ids"`
	PartySize    int      `json:"party_size"`
	Status       string   `json:"status"`
	StartsAtLoc  string   `json:"starts_at_local"`
	StartsAt     string   `json:"starts_at"`
	EndsAt       string   `json:"ends_at"`
	CreatedAt    string   `json:"created_at"`
}

type fixture struct {
	Users        []fixtureUser        `json:"users"`
	Restaurants  []fixtureRestaurant  `json:"restaurants"`
	Reservations []fixtureReservation `json:"reservations"`
}

func invalid(format string, a ...any) *apiErr {
	return &apiErr{http.StatusUnprocessableEntity, "validation_failed", fmt.Sprintf(format, a...)}
}

func clearAll(tx querier) error {
	for _, t := range allTables {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	return nil
}

func validID(s string) bool { return s != "" && len(s) <= 64 }

func applyFixture(tx querier, fx *fixture) error {
	if err := clearAll(tx); err != nil {
		return err
	}
	emails := map[string]bool{}
	for _, u := range fx.Users {
		if !validID(u.ID) {
			return invalid("user id must be 1-64 chars")
		}
		email := normEmail(u.Email)
		if email == "" || emails[email] {
			return invalid("user email missing or duplicated")
		}
		emails[email] = true
		h, err := hashPassword(u.Password)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO users(id,email,password_hash,display_name) VALUES(?,?,?,?)`, u.ID, email, h, u.DisplayName); err != nil {
			return invalid("user %q rejected: %v", u.ID, err)
		}
	}
	tableOwner := map[string]string{}
	for _, r := range fx.Restaurants {
		if !validID(r.ID) {
			return invalid("restaurant id must be 1-64 chars")
		}
		if _, err := time.LoadLocation(r.Timezone); err != nil || r.Timezone == "" {
			return invalid("restaurant %q has unknown timezone", r.ID)
		}
		if r.Slot < 1 || r.Duration < 1 || r.Cutoff < 0 {
			return invalid("restaurant %q has invalid minutes settings", r.ID)
		}
		combos, cerr := normalizeCombos(r.Tables, r.Combinable)
		if cerr != nil {
			return cerr
		}
		combJSON, _ := json.Marshal(combos)
		if _, err := tx.Exec(`INSERT INTO restaurants(id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes,combinable) VALUES(?,?,?,?,?,?,?)`,
			r.ID, r.Name, r.Timezone, r.Slot, r.Duration, r.Cutoff, string(combJSON)); err != nil {
			return invalid("restaurant %q rejected: %v", r.ID, err)
		}
		for _, h := range r.OpeningHrs {
			o, ok1 := parseHHMM(h.Opens)
			c, ok2 := parseHHMM(h.Closes)
			if !validWeekday(h.Weekday) || !ok1 || !ok2 || c <= o {
				return invalid("restaurant %q has invalid opening hours", r.ID)
			}
			if _, err := tx.Exec(`INSERT INTO opening_hours(restaurant_id,weekday,opens,closes) VALUES(?,?,?,?)`, r.ID, h.Weekday, h.Opens, h.Closes); err != nil {
				return err
			}
		}
		for _, t := range r.Tables {
			if !validID(t.ID) || t.Capacity < 1 {
				return invalid("restaurant %q has an invalid table", r.ID)
			}
			if _, err := tx.Exec(`INSERT INTO tables(id,restaurant_id,label,capacity) VALUES(?,?,?,?)`, t.ID, r.ID, t.Label, t.Capacity); err != nil {
				return invalid("table %q rejected: %v", t.ID, err)
			}
			tableOwner[r.ID+"/"+t.ID] = r.ID
		}
	}
	for _, rv := range fx.Reservations {
		if err := seedReservation(tx, rv, tableOwner); err != nil {
			return err
		}
	}
	return nil
}

func validWeekday(s string) bool {
	for _, w := range weekdayNames {
		if w == s {
			return true
		}
	}
	return false
}

func seedReservation(tx querier, rv fixtureReservation, tableOwner map[string]string) error {
	if rv.ID == "" {
		rv.ID = newID("res_")
	}
	if !validID(rv.ID) {
		return invalid("reservation id must be 1-64 chars")
	}
	if rv.Reference == "" {
		ref, err := newReference(tx)
		if err != nil {
			return err
		}
		rv.Reference = ref
	}
	if !refRe.MatchString(rv.Reference) {
		return invalid("reservation %q reference must be 6-12 characters of A-Z0-9", rv.ID)
	}
	if rv.Status == "" {
		rv.Status = "confirmed"
	}
	if rv.Status != "confirmed" && rv.Status != "cancelled" {
		return invalid("reservation %q has invalid status", rv.ID)
	}
	if rv.PartySize < 1 {
		return invalid("reservation %q has invalid party_size", rv.ID)
	}
	tableIDs := rv.TableIDs
	if len(tableIDs) == 0 && rv.TableID != "" {
		tableIDs = []string{rv.TableID}
	}
	if len(tableIDs) < 1 || len(tableIDs) > 2 || (len(tableIDs) == 2 && tableIDs[0] == tableIDs[1]) {
		return invalid("reservation %q must name one or two distinct tables", rv.ID)
	}
	for _, t := range tableIDs {
		if tableOwner[rv.RestaurantID+"/"+t] != rv.RestaurantID {
			return invalid("reservation %q table does not belong to restaurant", rv.ID)
		}
	}
	rest, err := loadRestaurant(tx, rv.RestaurantID)
	if err != nil {
		return invalid("reservation %q has unknown restaurant", rv.ID)
	}
	var start time.Time
	switch {
	case rv.StartsAtLoc != "":
		wall, ok := parseWall(rv.StartsAtLoc)
		if !ok {
			return invalid("reservation %q has invalid starts_at_local", rv.ID)
		}
		start, ok = resolveWall(rest.Loc, wall)
		if !ok {
			return invalid("reservation %q starts in a skipped local hour", rv.ID)
		}
	case rv.StartsAt != "":
		start, err = time.Parse(time.RFC3339, rv.StartsAt)
		if err != nil {
			return invalid("reservation %q has invalid starts_at", rv.ID)
		}
	default:
		return invalid("reservation %q has no start time", rv.ID)
	}
	end := start.Add(time.Duration(rest.Duration) * time.Minute)
	if rv.EndsAt != "" {
		end, err = time.Parse(time.RFC3339, rv.EndsAt)
		if err != nil || !end.After(start) {
			return invalid("reservation %q has invalid ends_at", rv.ID)
		}
	}
	if rv.CreatedAt == "" {
		rv.CreatedAt = nowStamp()
	}
	if len(tableIDs) == 2 && !rest.pairAllowed(tableIDs[0], tableIDs[1]) {
		return invalid("reservation %q uses a pair that is not combinable", rv.ID)
	}
	if err := insertReservation(tx, rv.ID, rv.Reference, rv.UserID, rv.RestaurantID, tableIDs, rv.PartySize, rv.Status, start.Unix(), end.Unix(), rv.CreatedAt); err != nil {
		return invalid("reservation %q rejected: %v", rv.ID, err)
	}
	return nil
}

// ---- export / import ----

type stateUser struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	PasswordHash string `json:"password_hash"`
	DisplayName  string `json:"display_name"`
}

type stateRestaurant struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Timezone   string     `json:"timezone"`
	Slot       int        `json:"slot_minutes"`
	Duration   int        `json:"reservation_duration_minutes"`
	Cutoff     int        `json:"cancellation_cutoff_minutes"`
	Combinable [][]string `json:"combinable"`
}

type stateHours struct {
	RestaurantID string `json:"restaurant_id"`
	Weekday      string `json:"weekday"`
	Opens        string `json:"opens"`
	Closes       string `json:"closes"`
}

type stateTable struct {
	ID           string `json:"id"`
	RestaurantID string `json:"restaurant_id"`
	Label        string `json:"label"`
	Capacity     int    `json:"capacity"`
}

// stateReservation keeps the stage-1 "table_id" field for compatibility;
// "table_ids" carries the full set and wins when present.
type stateReservation struct {
	ID           string   `json:"id"`
	Reference    string   `json:"reference"`
	UserID       string   `json:"user_id"`
	RestaurantID string   `json:"restaurant_id"`
	TableID      string   `json:"table_id"`
	TableIDs     []string `json:"table_ids"`
	PartySize    int      `json:"party_size"`
	Status       string   `json:"status"`
	StartsAt     int64    `json:"starts_at_unix"`
	EndsAt       int64    `json:"ends_at_unix"`
	CreatedAt    string   `json:"created_at"`
}

type stateToken struct {
	Token     string `json:"token"`
	UserID    string `json:"user_id"`
	CreatedAt string `json:"created_at"`
}

type stateIdem struct {
	Key             string `json:"key"`
	UserID          string `json:"user_id"`
	RequestPath     string `json:"request_path"`
	RequestBodyHash string `json:"request_body_hash"`
	ResponseBody    string `json:"response_body"`
	CreatedAt       string `json:"created_at"`
}

type stateDoc struct {
	Users           []stateUser        `json:"users"`
	Restaurants     []stateRestaurant  `json:"restaurants"`
	OpeningHours    []stateHours       `json:"opening_hours"`
	Tables          []stateTable       `json:"tables"`
	Reservations    []stateReservation `json:"reservations"`
	Tokens          []stateToken       `json:"tokens"`
	IdempotencyKeys []stateIdem        `json:"idempotency_keys"`
}

func scanAll(q querier, query string, each func(r *sql.Rows) error) error {
	rows, err := q.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportState(q querier) (*stateDoc, error) {
	s := &stateDoc{
		Users: []stateUser{}, Restaurants: []stateRestaurant{}, OpeningHours: []stateHours{}, Tables: []stateTable{},
		Reservations: []stateReservation{}, Tokens: []stateToken{}, IdempotencyKeys: []stateIdem{},
	}
	if err := scanAll(q, `SELECT id,email,password_hash,display_name FROM users ORDER BY rowid`, func(r *sql.Rows) error {
		var x stateUser
		err := r.Scan(&x.ID, &x.Email, &x.PasswordHash, &x.DisplayName)
		s.Users = append(s.Users, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := scanAll(q, `SELECT id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes,combinable FROM restaurants ORDER BY rowid`, func(r *sql.Rows) error {
		var x stateRestaurant
		var comb string
		err := r.Scan(&x.ID, &x.Name, &x.Timezone, &x.Slot, &x.Duration, &x.Cutoff, &comb)
		json.Unmarshal([]byte(comb), &x.Combinable)
		if x.Combinable == nil {
			x.Combinable = [][]string{}
		}
		s.Restaurants = append(s.Restaurants, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := scanAll(q, `SELECT restaurant_id,weekday,opens,closes FROM opening_hours ORDER BY id`, func(r *sql.Rows) error {
		var x stateHours
		err := r.Scan(&x.RestaurantID, &x.Weekday, &x.Opens, &x.Closes)
		s.OpeningHours = append(s.OpeningHours, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := scanAll(q, `SELECT id,restaurant_id,label,capacity FROM tables ORDER BY rowid`, func(r *sql.Rows) error {
		var x stateTable
		err := r.Scan(&x.ID, &x.RestaurantID, &x.Label, &x.Capacity)
		s.Tables = append(s.Tables, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := scanAll(q, `SELECT id,reference,user_id,restaurant_id,party_size,status,starts_at,ends_at,created_at FROM reservations ORDER BY rowid`, func(r *sql.Rows) error {
		var x stateReservation
		err := r.Scan(&x.ID, &x.Reference, &x.UserID, &x.RestaurantID, &x.PartySize, &x.Status, &x.StartsAt, &x.EndsAt, &x.CreatedAt)
		s.Reservations = append(s.Reservations, x)
		return err
	}); err != nil {
		return nil, err
	}
	for i := range s.Reservations {
		x := &s.Reservations[i]
		x.TableIDs = []string{}
		rows, err := q.Query(`SELECT table_id FROM reservation_tables WHERE reservation_id=? ORDER BY position`, x.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return nil, err
			}
			x.TableIDs = append(x.TableIDs, t)
		}
		rows.Close()
		if len(x.TableIDs) > 0 {
			x.TableID = x.TableIDs[0]
		}
	}
	if err := scanAll(q, `SELECT token,user_id,created_at FROM tokens ORDER BY rowid`, func(r *sql.Rows) error {
		var x stateToken
		err := r.Scan(&x.Token, &x.UserID, &x.CreatedAt)
		s.Tokens = append(s.Tokens, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := scanAll(q, `SELECT key,user_id,request_path,request_body_hash,response_body,created_at FROM idempotency_keys ORDER BY rowid`, func(r *sql.Rows) error {
		var x stateIdem
		err := r.Scan(&x.Key, &x.UserID, &x.RequestPath, &x.RequestBodyHash, &x.ResponseBody, &x.CreatedAt)
		s.IdempotencyKeys = append(s.IdempotencyKeys, x)
		return err
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func importState(tx querier, s *stateDoc) error {
	if err := clearAll(tx); err != nil {
		return err
	}
	bad := func(err error) error { return invalid("state rejected: %v", err) }
	for _, u := range s.Users {
		if _, err := tx.Exec(`INSERT INTO users(id,email,password_hash,display_name) VALUES(?,?,?,?)`, u.ID, u.Email, u.PasswordHash, u.DisplayName); err != nil {
			return bad(err)
		}
	}
	for _, r := range s.Restaurants {
		if _, err := time.LoadLocation(r.Timezone); err != nil || r.Timezone == "" || r.Slot < 1 || r.Duration < 1 {
			return invalid("state has an invalid restaurant")
		}
		comb := r.Combinable
		if comb == nil {
			comb = [][]string{}
		}
		combJSON, _ := json.Marshal(comb)
		if _, err := tx.Exec(`INSERT INTO restaurants(id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes,combinable) VALUES(?,?,?,?,?,?,?)`,
			r.ID, r.Name, r.Timezone, r.Slot, r.Duration, r.Cutoff, string(combJSON)); err != nil {
			return bad(err)
		}
	}
	for _, h := range s.OpeningHours {
		if _, err := tx.Exec(`INSERT INTO opening_hours(restaurant_id,weekday,opens,closes) VALUES(?,?,?,?)`, h.RestaurantID, h.Weekday, h.Opens, h.Closes); err != nil {
			return bad(err)
		}
	}
	for _, t := range s.Tables {
		if _, err := tx.Exec(`INSERT INTO tables(id,restaurant_id,label,capacity) VALUES(?,?,?,?)`, t.ID, t.RestaurantID, t.Label, t.Capacity); err != nil {
			return bad(err)
		}
	}
	for _, r := range s.Reservations {
		if r.Status != "confirmed" && r.Status != "cancelled" {
			return invalid("state has an invalid reservation status")
		}
		ids := r.TableIDs
		if len(ids) == 0 && r.TableID != "" {
			ids = []string{r.TableID}
		}
		if len(ids) < 1 || len(ids) > 2 {
			return invalid("state has a reservation without one or two tables")
		}
		if err := insertReservation(tx, r.ID, r.Reference, r.UserID, r.RestaurantID, ids, r.PartySize, r.Status, r.StartsAt, r.EndsAt, r.CreatedAt); err != nil {
			return bad(err)
		}
	}
	for _, t := range s.Tokens {
		if _, err := tx.Exec(`INSERT INTO tokens(token,user_id,created_at) VALUES(?,?,?)`, t.Token, t.UserID, t.CreatedAt); err != nil {
			return bad(err)
		}
	}
	for _, k := range s.IdempotencyKeys {
		if _, err := tx.Exec(`INSERT INTO idempotency_keys(key,user_id,request_path,request_body_hash,response_body,created_at) VALUES(?,?,?,?,?,?)`,
			k.Key, k.UserID, k.RequestPath, k.RequestBodyHash, k.ResponseBody, k.CreatedAt); err != nil {
			return bad(err)
		}
	}
	return nil
}

// pairAllowed reports whether two tables are a declared combinable pair.
func (r *restaurant) pairAllowed(a, b string) bool {
	for _, c := range r.Combos {
		if len(c) == 2 && ((c[0] == a && c[1] == b) || (c[0] == b && c[1] == a)) {
			return true
		}
	}
	return false
}

// normalizeCombos validates declared table pairs: exactly two distinct,
// existing table ids each; unordered duplicates are dropped, order is kept.
func normalizeCombos(tables []tableRow, combos [][]string) ([][]string, *apiErr) {
	known := map[string]bool{}
	for _, t := range tables {
		known[t.ID] = true
	}
	out := [][]string{}
	seen := map[string]bool{}
	for _, c := range combos {
		if len(c) != 2 || c[0] == c[1] || !known[c[0]] || !known[c[1]] {
			return nil, invalid("combinable entries must be pairs of two distinct tables of the restaurant")
		}
		k := c[0] + "\x00" + c[1]
		if c[1] < c[0] {
			k = c[1] + "\x00" + c[0]
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, []string{c[0], c[1]})
	}
	return out, nil
}

// insertReservation stores a reservation row and its table set.
func insertReservation(q querier, id, ref, uid, restID string, tableIDs []string, party int, status string, start, end int64, createdAt string) error {
	if _, err := q.Exec(`INSERT INTO reservations(id,reference,user_id,restaurant_id,party_size,status,starts_at,ends_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, ref, uid, restID, party, status, start, end, createdAt); err != nil {
		return err
	}
	return setReservationTables(q, id, restID, tableIDs)
}

func setReservationTables(q querier, id, restID string, tableIDs []string) error {
	if _, err := q.Exec(`DELETE FROM reservation_tables WHERE reservation_id=?`, id); err != nil {
		return err
	}
	for i, t := range tableIDs {
		if _, err := q.Exec(`INSERT INTO reservation_tables(reservation_id,restaurant_id,table_id,position) VALUES(?,?,?,?)`, id, restID, t, i); err != nil {
			return err
		}
	}
	return nil
}

var refRe = regexp.MustCompile(`^[A-Z0-9]{6,12}$`)
