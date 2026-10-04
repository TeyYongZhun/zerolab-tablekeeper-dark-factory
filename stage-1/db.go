package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
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
    cancellation_cutoff_minutes INTEGER NOT NULL
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
    table_id TEXT NOT NULL,
    party_size INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'confirmed',
    starts_at INTEGER NOT NULL,
    ends_at INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (restaurant_id, table_id) REFERENCES tables(restaurant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_reservations_table_time ON reservations(restaurant_id, table_id, starts_at, ends_at);
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
var allTables = []string{"idempotency_keys", "tokens", "reservations", "opening_hours", "tables", "restaurants", "users"}

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
}

func loadRestaurant(q querier, id string) (*restaurant, error) {
	r := &restaurant{}
	err := q.QueryRow(`SELECT id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes FROM restaurants WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Timezone, &r.Slot, &r.Duration, &r.Cutoff)
	if err != nil {
		return nil, err
	}
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
}

type fixtureReservation struct {
	ID           string `json:"id"`
	Reference    string `json:"reference"`
	UserID       string `json:"user_id"`
	RestaurantID string `json:"restaurant_id"`
	TableID      string `json:"table_id"`
	PartySize    int    `json:"party_size"`
	Status       string `json:"status"`
	StartsAtLoc  string `json:"starts_at_local"`
	StartsAt     string `json:"starts_at"`
	EndsAt       string `json:"ends_at"`
	CreatedAt    string `json:"created_at"`
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
		if _, err := tx.Exec(`INSERT INTO restaurants(id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes) VALUES(?,?,?,?,?,?)`,
			r.ID, r.Name, r.Timezone, r.Slot, r.Duration, r.Cutoff); err != nil {
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
	if tableOwner[rv.RestaurantID+"/"+rv.TableID] != rv.RestaurantID {
		return invalid("reservation %q table does not belong to restaurant", rv.ID)
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
	if _, err := tx.Exec(`INSERT INTO reservations(id,reference,user_id,restaurant_id,table_id,party_size,status,starts_at,ends_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		rv.ID, rv.Reference, rv.UserID, rv.RestaurantID, rv.TableID, rv.PartySize, rv.Status, start.Unix(), end.Unix(), rv.CreatedAt); err != nil {
		return invalid("reservation %q rejected: %v", rv.ID, err)
	}
	return nil
}

// ---- export / import ----

type stateDoc struct {
	Users []struct {
		ID           string `json:"id"`
		Email        string `json:"email"`
		PasswordHash string `json:"password_hash"`
		DisplayName  string `json:"display_name"`
	} `json:"users"`
	Restaurants []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Timezone string `json:"timezone"`
		Slot     int    `json:"slot_minutes"`
		Duration int    `json:"reservation_duration_minutes"`
		Cutoff   int    `json:"cancellation_cutoff_minutes"`
	} `json:"restaurants"`
	OpeningHours []struct {
		RestaurantID string `json:"restaurant_id"`
		Weekday      string `json:"weekday"`
		Opens        string `json:"opens"`
		Closes       string `json:"closes"`
	} `json:"opening_hours"`
	Tables []struct {
		ID           string `json:"id"`
		RestaurantID string `json:"restaurant_id"`
		Label        string `json:"label"`
		Capacity     int    `json:"capacity"`
	} `json:"tables"`
	Reservations []struct {
		ID           string `json:"id"`
		Reference    string `json:"reference"`
		UserID       string `json:"user_id"`
		RestaurantID string `json:"restaurant_id"`
		TableID      string `json:"table_id"`
		PartySize    int    `json:"party_size"`
		Status       string `json:"status"`
		StartsAt     int64  `json:"starts_at_unix"`
		EndsAt       int64  `json:"ends_at_unix"`
		CreatedAt    string `json:"created_at"`
	} `json:"reservations"`
	Tokens []struct {
		Token     string `json:"token"`
		UserID    string `json:"user_id"`
		CreatedAt string `json:"created_at"`
	} `json:"tokens"`
	IdempotencyKeys []struct {
		Key             string `json:"key"`
		UserID          string `json:"user_id"`
		RequestPath     string `json:"request_path"`
		RequestBodyHash string `json:"request_body_hash"`
		ResponseBody    string `json:"response_body"`
		CreatedAt       string `json:"created_at"`
	} `json:"idempotency_keys"`
}

func exportState(q querier) (*stateDoc, error) {
	s := &stateDoc{}
	type scanner func(rows *sql.Rows) error
	run := func(query string, fn scanner) error {
		rows, err := q.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := fn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := run(`SELECT id,email,password_hash,display_name FROM users ORDER BY rowid`, func(r *sql.Rows) error {
		var u struct {
			ID           string `json:"id"`
			Email        string `json:"email"`
			PasswordHash string `json:"password_hash"`
			DisplayName  string `json:"display_name"`
		}
		err := r.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName)
		s.Users = append(s.Users, u)
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(`SELECT id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes FROM restaurants ORDER BY rowid`, func(r *sql.Rows) error {
		var x struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Timezone string `json:"timezone"`
			Slot     int    `json:"slot_minutes"`
			Duration int    `json:"reservation_duration_minutes"`
			Cutoff   int    `json:"cancellation_cutoff_minutes"`
		}
		err := r.Scan(&x.ID, &x.Name, &x.Timezone, &x.Slot, &x.Duration, &x.Cutoff)
		s.Restaurants = append(s.Restaurants, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(`SELECT restaurant_id,weekday,opens,closes FROM opening_hours ORDER BY id`, func(r *sql.Rows) error {
		var x struct {
			RestaurantID string `json:"restaurant_id"`
			Weekday      string `json:"weekday"`
			Opens        string `json:"opens"`
			Closes       string `json:"closes"`
		}
		err := r.Scan(&x.RestaurantID, &x.Weekday, &x.Opens, &x.Closes)
		s.OpeningHours = append(s.OpeningHours, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(`SELECT id,restaurant_id,label,capacity FROM tables ORDER BY rowid`, func(r *sql.Rows) error {
		var x struct {
			ID           string `json:"id"`
			RestaurantID string `json:"restaurant_id"`
			Label        string `json:"label"`
			Capacity     int    `json:"capacity"`
		}
		err := r.Scan(&x.ID, &x.RestaurantID, &x.Label, &x.Capacity)
		s.Tables = append(s.Tables, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(`SELECT id,reference,user_id,restaurant_id,table_id,party_size,status,starts_at,ends_at,created_at FROM reservations ORDER BY rowid`, func(r *sql.Rows) error {
		var x struct {
			ID           string `json:"id"`
			Reference    string `json:"reference"`
			UserID       string `json:"user_id"`
			RestaurantID string `json:"restaurant_id"`
			TableID      string `json:"table_id"`
			PartySize    int    `json:"party_size"`
			Status       string `json:"status"`
			StartsAt     int64  `json:"starts_at_unix"`
			EndsAt       int64  `json:"ends_at_unix"`
			CreatedAt    string `json:"created_at"`
		}
		err := r.Scan(&x.ID, &x.Reference, &x.UserID, &x.RestaurantID, &x.TableID, &x.PartySize, &x.Status, &x.StartsAt, &x.EndsAt, &x.CreatedAt)
		s.Reservations = append(s.Reservations, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(`SELECT token,user_id,created_at FROM tokens ORDER BY rowid`, func(r *sql.Rows) error {
		var x struct {
			Token     string `json:"token"`
			UserID    string `json:"user_id"`
			CreatedAt string `json:"created_at"`
		}
		err := r.Scan(&x.Token, &x.UserID, &x.CreatedAt)
		s.Tokens = append(s.Tokens, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(`SELECT key,user_id,request_path,request_body_hash,response_body,created_at FROM idempotency_keys ORDER BY rowid`, func(r *sql.Rows) error {
		var x struct {
			Key             string `json:"key"`
			UserID          string `json:"user_id"`
			RequestPath     string `json:"request_path"`
			RequestBodyHash string `json:"request_body_hash"`
			ResponseBody    string `json:"response_body"`
			CreatedAt       string `json:"created_at"`
		}
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
		if _, err := tx.Exec(`INSERT INTO restaurants(id,name,timezone,slot_minutes,reservation_duration_minutes,cancellation_cutoff_minutes) VALUES(?,?,?,?,?,?)`,
			r.ID, r.Name, r.Timezone, r.Slot, r.Duration, r.Cutoff); err != nil {
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
		if _, err := tx.Exec(`INSERT INTO reservations(id,reference,user_id,restaurant_id,table_id,party_size,status,starts_at,ends_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Reference, r.UserID, r.RestaurantID, r.TableID, r.PartySize, r.Status, r.StartsAt, r.EndsAt, r.CreatedAt); err != nil {
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

var refRe = regexp.MustCompile(`^[A-Z0-9]{6,12}$`)
