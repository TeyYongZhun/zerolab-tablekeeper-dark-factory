package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const comboFixture = `{"users":[{"id":"u_ada","email":"ada@example.com","password":"correct horse","display_name":"Ada"}],
"restaurants":[{"id":"r_anker","name":"Zum Anker","timezone":"Europe/Berlin","slot_minutes":30,"reservation_duration_minutes":90,"cancellation_cutoff_minutes":120,
"opening_hours":[{"weekday":"thu","opens":"18:00","closes":"23:00"}],
"tables":[{"id":"t_1","label":"1","capacity":2},{"id":"t_2","label":"2","capacity":4},{"id":"t_3","label":"3","capacity":4}],
"combinable":[["t_1","t_2"],["t_2","t_3"]]}],"reservations":[]}`

func TestCombinedTables(t *testing.T) {
	e := newEnv(t)
	if c, m := e.do("POST", "/_test/reset", "", "", comboFixture); c != 204 {
		t.Fatalf("reset %d %v", c, m)
	}
	_, m := e.do("POST", "/auth/login", "", "", `{"email":"ada@example.com","password":"correct horse"}`)
	tok := m["token"].(string)

	_, av := e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-24&party_size=5", "", "", "")
	opts := av["slots"].([]any)[0].(map[string]any)["available_options"].([]any)
	if len(opts) != 2 || opts[0].(map[string]any)["capacity"].(float64) != 6 || opts[1].(map[string]any)["capacity"].(float64) != 8 {
		t.Fatalf("party 5 options: %v", opts)
	}
	_, av = e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-24&party_size=2", "", "", "")
	slot := av["slots"].([]any)[0].(map[string]any)
	opts = slot["available_options"].([]any)
	if len(slot["available_table_ids"].([]any)) != 3 || len(opts) != 5 {
		t.Fatalf("party 2 options: %v", slot)
	}
	if first := opts[3].(map[string]any)["table_ids"].([]any); first[0] != "t_1" || first[1] != "t_2" {
		t.Fatalf("pairs must follow singles in combinable order: %v", opts)
	}

	body := func(ids string, party int) string {
		return `{"restaurant_id":"r_anker",` + ids + `,"starts_at_local":"2099-09-24T19:00","party_size":` + string(rune('0'+party)) + `}`
	}
	c, got := e.do("POST", "/reservations", tok, "p1", body(`"table_ids":["t_2","t_1"]`, 6))
	if c != 201 || got["table_id"] != nil {
		t.Fatalf("pair booking %d %v", c, got)
	}
	if ids := got["table_ids"].([]any); ids[0] != "t_1" || ids[1] != "t_2" {
		t.Fatalf("table_ids %v", ids)
	}
	for _, tc := range []struct {
		ids   string
		party int
		code  string
		http  int
	}{
		{`"table_id":"t_2"`, 2, "table_unavailable", 409},
		{`"table_ids":["t_3","t_2"]`, 2, "table_unavailable", 409},
		{`"table_ids":["t_1","t_3"]`, 2, "combination_not_allowed", 422},
		{`"table_ids":["t_1","t_2","t_3"]`, 2, "combination_not_allowed", 422},
		{`"table_ids":["t_1","t_1"]`, 2, "validation_failed", 422},
		{`"table_id":"t_3","table_ids":["t_3"]`, 2, "validation_failed", 422},
		{`"table_ids":[]`, 2, "validation_failed", 422},
		{`"table_ids":["t_9"]`, 2, "not_found", 404},
	} {
		if c, m := e.do("POST", "/reservations", tok, "x", body(tc.ids, tc.party)); c != tc.http || code(m) != tc.code {
			t.Fatalf("%s: got %d %v", tc.ids, c, m)
		}
	}
	// single via table_ids is plain single: reports table_id too, capacity checked
	if c, m := e.do("POST", "/reservations", tok, "s1", `{"restaurant_id":"r_anker","table_ids":["t_3"],"starts_at_local":"2099-09-24T19:00","party_size":5}`); c != 422 || code(m) != "party_exceeds_capacity" {
		t.Fatalf("capacity %d %v", c, m)
	}
	c, one := e.do("POST", "/reservations", tok, "s2", `{"restaurant_id":"r_anker","table_ids":["t_3"],"starts_at_local":"2099-09-24T19:00","party_size":4}`)
	if c != 201 || one["table_id"] != "t_3" || len(one["table_ids"].([]any)) != 1 {
		t.Fatalf("single via table_ids %d %v", c, one)
	}
	// patch: move the pair booking onto t_2+t_3? t_3 is taken -> 409; later slot works
	ref := got["reference"].(string)
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{"table_ids":["t_2","t_3"],"party_size":6}`); c != 409 {
		t.Fatalf("patch conflict %d %v", c, m)
	}
	c, pm := e.do("PATCH", "/reservations/"+ref, tok, "", `{"table_ids":["t_2","t_3"],"party_size":6,"starts_at_local":"2099-09-24T21:00"}`)
	if c != 200 || pm["table_ids"].([]any)[1] != "t_3" {
		t.Fatalf("patch %d %v", c, pm)
	}
	// t_1 is free again at 19:00
	if c, m := e.do("POST", "/reservations", tok, "s3", `{"restaurant_id":"r_anker","table_id":"t_1","starts_at_local":"2099-09-24T19:00","party_size":2}`); c != 201 {
		t.Fatalf("t_1 should be free %d %v", c, m)
	}
	// moves with table_ids
	c, mv := e.do("POST", "/reservation-moves", tok, "mv1", `{"moves":[{"reference":"`+ref+`","table_ids":["t_1","t_2"],"starts_at_local":"2099-09-24T21:00"}]}`)
	if c != 201 || mv["reservations"].([]any)[0].(map[string]any)["table_ids"].([]any)[0] != "t_1" {
		t.Fatalf("moves %d %v", c, mv)
	}
	// export/import keeps table sets
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, httptest.NewRequest("GET", "/_test/export", nil))
	exported := rr.Body.String()
	e.do("POST", "/_test/reset", "", "", `{}`)
	if c, m := e.do("POST", "/_test/import", "", "", exported); c != 204 {
		t.Fatalf("import %d %v", c, m)
	}
	_, g := e.do("GET", "/reservations/"+ref, tok, "", "")
	if ids := g["table_ids"].([]any); len(ids) != 2 {
		t.Fatalf("pair lost on import: %v", g)
	}
	_, rd := e.do("GET", "/restaurants/r_anker", "", "", "")
	if len(rd["combinable"].([]any)) != 2 {
		t.Fatalf("combinable lost: %v", rd)
	}
}

func TestStage1ExportImports(t *testing.T) {
	e := newEnv(t)
	stage1 := `{"track":"tablekeeper","format_version":1,"state":{
"users":[{"id":"u_ada","email":"ada@example.com","password_hash":"x","display_name":"Ada"}],
"restaurants":[{"id":"r_a","name":"A","timezone":"Europe/Berlin","slot_minutes":30,"reservation_duration_minutes":90,"cancellation_cutoff_minutes":120}],
"opening_hours":[{"restaurant_id":"r_a","weekday":"thu","opens":"18:00","closes":"23:00"}],
"tables":[{"id":"t_1","restaurant_id":"r_a","label":"1","capacity":2}],
"reservations":[{"id":"res_1","reference":"ABC123","user_id":"u_ada","restaurant_id":"r_a","table_id":"t_1","party_size":2,"status":"confirmed","starts_at_unix":4000000000,"ends_at_unix":4000005400,"created_at":"2026-09-21T11:04:03+00:00"}],
"tokens":[{"token":"tok123","user_id":"u_ada","created_at":"2026-09-21T11:04:03+00:00"}],
"idempotency_keys":[]}}`
	if c, m := e.do("POST", "/_test/import", "", "", stage1); c != 204 {
		t.Fatalf("stage-1 import %d %v", c, m)
	}
	c, g := e.do("GET", "/reservations/ABC123", "tok123", "", "")
	if c != 200 || g["table_id"] != "t_1" || len(g["table_ids"].([]any)) != 1 {
		t.Fatalf("session/reservation after import %d %v", c, g)
	}
}

func TestUIRoutes(t *testing.T) {
	e := newEnv(t)
	for path, ids := range map[string][]string{
		"/":       {"restaurant-select", "date-input", "party-size-input", "search-button", "booking-uncertain", "availability-grid", "no-slots", "confirmation-reference", "booking-error"},
		"/signup": {"signup-email", "signup-password", "signup-display-name", "signup-submit", "auth-error", "current-user", "logout-button"},
		"/login":  {"login-email", "login-password", "login-submit", "auth-error"},
		"/lookup": {"lookup-reference-input", "lookup-submit", "reservation-detail", "reservation-status", "reservation-cancel-button", "reservation-error", "reservation-tables"},
	} {
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Header().Get("Content-Type"))
		}
		for _, id := range ids {
			if !strings.Contains(rr.Body.String(), `data-testid="`+id+`"`) && !strings.Contains(rr.Body.String(), "'"+id+"'") {
				t.Errorf("%s missing data-testid %s", path, id)
			}
		}
	}
	if c, _ := e.do("GET", "/nope", "", "", ""); c != 404 {
		t.Fatalf("unknown path %d", c)
	}
}
