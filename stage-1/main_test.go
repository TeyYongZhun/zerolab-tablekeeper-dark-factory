package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveWallDST(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	// spring forward 2026-03-29: 02:30 does not exist
	if _, ok := resolveWall(loc, time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC)); ok {
		t.Fatal("02:30 on spring-forward day must be invalid")
	}
	if _, ok := resolveWall(loc, time.Date(2026, 3, 29, 3, 0, 0, 0, time.UTC)); !ok {
		t.Fatal("03:00 must be valid")
	}
	// fall back 2026-10-25: 02:30 occurs twice, first is +02:00
	got, ok := resolveWall(loc, time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC))
	if !ok || fmtRFC(got, loc) != "2026-10-25T02:30:00+02:00" {
		t.Fatalf("fall back first occurrence wrong: %v %v", got, ok)
	}
	got, _ = resolveWall(loc, time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC))
	if fmtRFC(got, loc) != "2026-09-24T18:00:00+02:00" {
		t.Fatalf("summer offset wrong: %v", fmtRFC(got, loc))
	}
}

type env struct {
	t *testing.T
	h http.Handler
}

func newEnv(t *testing.T) *env {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &env{t, (&server{db: db}).routes()}
}

func (e *env) do(method, path, token, key, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func code(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

// dates far in the future so the cutoff never interferes; 2099-09-24 is a Thursday.
const fixtureJSON = `{"users":[{"id":"u_ada","email":"ada@example.com","password":"correct horse","display_name":"Ada"}],
"restaurants":[{"id":"r_anker","name":"Zum Anker","timezone":"Europe/Berlin","slot_minutes":30,"reservation_duration_minutes":90,"cancellation_cutoff_minutes":120,
"opening_hours":[{"weekday":"thu","opens":"18:00","closes":"23:00"},{"weekday":"fri","opens":"18:00","closes":"23:30"}],
"tables":[{"id":"t_1","label":"1","capacity":2},{"id":"t_2","label":"2","capacity":4}]}],
"reservations":[{"id":"res_1","reference":"ABC123","user_id":"u_ada","restaurant_id":"r_anker","table_id":"t_1","party_size":2,"status":"confirmed","starts_at_local":"2099-09-25T19:00","created_at":"2026-09-21T11:04:03+00:00"}]}`

func TestFlow(t *testing.T) {
	e := newEnv(t)
	if c, _ := e.do("POST", "/_test/reset", "", "", fixtureJSON); c != 204 {
		t.Fatalf("reset %d", c)
	}
	c, m := e.do("POST", "/auth/login", "", "", `{"email":"ada@example.com","password":"correct horse"}`)
	if c != 200 {
		t.Fatalf("login %d %v", c, m)
	}
	tok := m["token"].(string)
	if c, m := e.do("POST", "/auth/login", "", "", `{"email":"ada@example.com","password":"nope nope"}`); c != 401 || code(m) != "unauthenticated" {
		t.Fatalf("bad login %d", c)
	}
	if c, m := e.do("POST", "/auth/signup", "", "", `{"email":"ada@example.com","password":"password123","display_name":"x"}`); c != 409 || code(m) != "email_taken" {
		t.Fatalf("dup signup %d %v", c, m)
	}
	if c, m := e.do("POST", "/auth/signup", "", "", `{"email":"bad","password":"password123","display_name":"x"}`); c != 422 {
		t.Fatalf("bad email %d %v", c, m)
	}
	if c, _ := e.do("POST", "/auth/signup", "", "", `{"email":"b@example.com","password":"password123","display_name":"B"}`); c != 201 {
		t.Fatalf("signup %d", c)
	}

	// availability
	c, m = e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-24&party_size=4", "", "", "")
	slots := m["slots"].([]any)
	if c != 200 || len(slots) != 8 { // 18:00..21:30 every 30 min
		t.Fatalf("availability %d slots=%d", c, len(slots))
	}
	if c, _ := e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-24&party_size=4.0", "", "", ""); c != 422 {
		t.Fatalf("float party %d", c)
	}
	if _, m = e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-26&party_size=1", "", "", ""); len(m["slots"].([]any)) != 0 {
		t.Fatal("closed day should have no slots")
	}

	// auth + idempotency
	body := `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":4}`
	if c, _ := e.do("POST", "/reservations", "", "k1", body); c != 401 {
		t.Fatalf("unauth %d", c)
	}
	if c, m := e.do("POST", "/reservations", tok, "", body); c != 400 || code(m) != "missing_idempotency_key" {
		t.Fatalf("no key %d", c)
	}
	c, first := e.do("POST", "/reservations", tok, "k1", body)
	if c != 201 || first["ends_at"] != "2099-09-24T20:30:00+02:00" || first["status"] != "confirmed" {
		t.Fatalf("create %d %v", c, first)
	}
	c, replay := e.do("POST", "/reservations", tok, "k1", body)
	if c != 200 || replay["reference"] != first["reference"] {
		t.Fatalf("replay %d %v", c, replay)
	}
	if c, m := e.do("POST", "/reservations", tok, "k1", strings.Replace(body, "19:00", "20:00", 1)); c != 409 || code(m) != "idempotency_key_reuse" {
		t.Fatalf("reuse %d %v", c, m)
	}
	if c, m := e.do("POST", "/reservations", tok, "k2", body); c != 409 || code(m) != "table_unavailable" {
		t.Fatalf("overlap %d %v", c, m)
	}
	for _, tc := range []struct{ body, code string }{
		{`{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:15","party_size":2}`, "not_on_slot_grid"},
		{`{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T22:00","party_size":2}`, "outside_opening_hours"},
		{`{"restaurant_id":"r_anker","table_id":"t_1","starts_at_local":"2099-09-24T21:00","party_size":3}`, "party_exceeds_capacity"},
		{`{"restaurant_id":"r_anker","table_id":"t_1","starts_at_local":"2099-09-24T21:00","party_size":0}`, "validation_failed"},
		{`{"restaurant_id":"nope","table_id":"t_1","starts_at_local":"2099-09-24T21:00","party_size":2}`, "not_found"},
		{`{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2026-03-29T02:30","party_size":2}`, "invalid_local_time"},
	} {
		if c, m := e.do("POST", "/reservations", tok, "kx", tc.body); code(m) != tc.code {
			t.Fatalf("%s: got %d %v", tc.code, c, m)
		}
	}

	// get/list/other user
	ref := first["reference"].(string)
	if c, _ := e.do("GET", "/reservations/"+ref, tok, "", ""); c != 200 {
		t.Fatalf("get %d", c)
	}
	_, su := e.do("POST", "/auth/signup", "", "", `{"email":"c@example.com","password":"password123","display_name":"C"}`)
	other := su["token"].(string)
	if c, _ := e.do("GET", "/reservations/"+ref, other, "", ""); c != 404 {
		t.Fatalf("leak %d", c)
	}
	if c, _ := e.do("POST", "/reservations/"+ref+"/cancel", other, "", ""); c != 404 {
		t.Fatalf("cancel other %d", c)
	}
	_, l := e.do("GET", "/reservations", tok, "", "")
	if lst := l["reservations"].([]any); len(lst) != 2 || lst[0].(map[string]any)["reference"] != "ABC123" {
		t.Fatalf("list order %v", l)
	}

	// patch
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{"starts_at_local":"2099-09-24T20:00","bogus":1}`); c != 200 || m["starts_at_local"] != "2099-09-24T20:00" || m["reference"] != ref {
		t.Fatalf("patch %d %v", c, m)
	}
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{"table_id":"t_1"}`); c != 422 || code(m) != "party_exceeds_capacity" {
		t.Fatalf("patch cap %d %v", c, m)
	}

	// moves
	c, m = e.do("POST", "/reservation-moves", tok, "m2", `{"moves":[{"reference":"`+ref+`","starts_at_local":"2099-09-24T21:00"},{"reference":"ABC123","party_size":1}]}`)
	if c != 201 {
		t.Fatalf("moves %d %v", c, m)
	}
	if c, _ := e.do("POST", "/reservation-moves", tok, "m2", `{"moves":[{"reference":"`+ref+`","starts_at_local":"2099-09-24T21:00"},{"reference":"ABC123","party_size":1}]}`); c != 200 {
		t.Fatalf("moves replay %d", c)
	}
	if c, m := e.do("POST", "/reservation-moves", tok, "m3", `{"moves":[{"reference":"`+ref+`","starts_at_local":"2099-09-25T19:00","table_id":"t_1","party_size":2},{"reference":"ABC123","party_size":1}]}`); c != 409 || code(m) != "table_unavailable" {
		t.Fatalf("moves conflict %d %v", c, m)
	}
	_, g := e.do("GET", "/reservations/"+ref, tok, "", "")
	if g["starts_at_local"] != "2099-09-24T21:00" {
		t.Fatalf("failed batch must not change state: %v", g)
	}

	// cancel releases the table
	if c, m := e.do("POST", "/reservations/"+ref+"/cancel", tok, "", ""); c != 200 || m["status"] != "cancelled" {
		t.Fatalf("cancel %d %v", c, m)
	}
	if c, m := e.do("POST", "/reservations/"+ref+"/cancel", tok, "", ""); c != 200 || m["status"] != "cancelled" {
		t.Fatalf("recancel %d", c)
	}
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{}`); c != 409 || code(m) != "reservation_cancelled" {
		t.Fatalf("patch cancelled %d %v", c, m)
	}

	// export / import round trip
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, httptest.NewRequest("GET", "/_test/export", nil))
	exported := rr.Body.Bytes()
	e.do("POST", "/_test/reset", "", "", `{}`)
	if c, _ := e.do("POST", "/_test/import", "", "", string(exported)); c != 204 {
		t.Fatalf("import %d", c)
	}
	if c, _ := e.do("GET", "/reservations/"+ref, tok, "", ""); c != 200 {
		t.Fatalf("state lost after import %d", c)
	}
	if c, m := e.do("POST", "/_test/import", "", "", `{"track":"x","format_version":1,"state":{}}`); c != 422 {
		t.Fatalf("bad import %d %v", c, m)
	}
}

func TestNoDoubleBookingConcurrent(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/_test/reset", "", "", fixtureJSON)
	_, m := e.do("POST", "/auth/login", "", "", `{"email":"ada@example.com","password":"correct horse"}`)
	tok := m["token"].(string)
	body := `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":4}`
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _ := e.do("POST", "/reservations", tok, "key"+string(rune('a'+i)), body)
			mu.Lock()
			codes[c]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if codes[201] != 1 || codes[409] != 19 {
		t.Fatalf("expected exactly one booking, got %v", codes)
	}
}

func TestResetRegressions(t *testing.T) {
	e := newEnv(t)
	two := `{"restaurants":[
{"id":"r_a","name":"A","timezone":"Europe/Berlin","slot_minutes":30,"reservation_duration_minutes":90,"cancellation_cutoff_minutes":0,"opening_hours":[{"weekday":"thu","opens":"18:00","closes":"23:00"}],"tables":[{"id":"t_1","label":"1","capacity":2}]},
{"id":"r_b","name":"B","timezone":"America/New_York","slot_minutes":30,"reservation_duration_minutes":90,"cancellation_cutoff_minutes":0,"opening_hours":[{"weekday":"thu","opens":"18:00","closes":"23:00"}],"tables":[{"id":"t_1","label":"1","capacity":2}]}]}`
	for i := 0; i < 2; i++ {
		if c, m := e.do("POST", "/_test/reset", "", "", two); c != 204 {
			t.Fatalf("same table id in two restaurants: %d %v", c, m)
		}
	}
	for _, ref := range []string{"x/lower01", "TOO-LONG-WITH-DASH", "abc123", "SHORT"} {
		fx := strings.Replace(fixtureJSON, `"ABC123"`, `"`+ref+`"`, 1)
		if c, m := e.do("POST", "/_test/reset", "", "", fx); c != 422 || code(m) != "validation_failed" {
			t.Fatalf("reference %q: %d %v", ref, c, m)
		}
	}
	if c, m := e.do("POST", "/auth/signup", "", "", `{"email":5,"password":"password123","display_name":"x"}`); c != 400 || code(m) != "malformed_request" {
		t.Fatalf("wrong type email: %d %v", c, m)
	}
}
