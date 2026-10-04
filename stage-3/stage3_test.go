package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const s3Fixture = `{"users":[
{"id":"u_ada","email":"ada@example.com","password":"correct horse","display_name":"Ada"},
{"id":"u_mgr","email":"mgr@example.com","password":"correct horse","display_name":"Manager"}],
"restaurants":[{"id":"r_anker","name":"Zum Anker","timezone":"Europe/Berlin","slot_minutes":30,"reservation_duration_minutes":90,"cancellation_cutoff_minutes":120,
"manager_user_ids":["u_mgr"],
"opening_hours":[{"weekday":"thu","opens":"18:00","closes":"23:00"}],
"tables":[{"id":"t_1","label":"1","capacity":2},{"id":"t_2","label":"2","capacity":4},{"id":"t_3","label":"3","capacity":4}],
"combinable":[["t_1","t_2"]]}],"reservations":[]}`

func login(t *testing.T, e *env, email string) string {
	_, m := e.do("POST", "/auth/login", "", "", `{"email":"`+email+`","password":"correct horse"}`)
	return m["token"].(string)
}

func TestExplainAndHistory(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/_test/reset", "", "", s3Fixture)
	tok := login(t, e, "ada@example.com")

	c, av := e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-24&party_size=3&explain=true", "", "", "")
	ex := av["slots"].([]any)[0].(map[string]any)["explain"].([]any)
	first := ex[0].(map[string]any)
	rules := first["rules"].([]any)
	if c != 200 || len(ex) != 3 || first["table_id"] != "t_1" || first["available"] != false ||
		rules[0].(map[string]any)["rule"] != "capacity" || rules[0].(map[string]any)["holds"] != false ||
		rules[1].(map[string]any)["rule"] != "no_overlap" || first["policy_version"].(float64) != 0 {
		t.Fatalf("explain %d %v", c, ex)
	}
	if c, _ := e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-24&party_size=3&explain=1", "", "", ""); c != 422 {
		t.Fatalf("explain=1 must be 422, got %d", c)
	}

	c, res := e.do("POST", "/reservations", tok, "k1", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":3}`)
	if c != 201 || res["revision"].(float64) != 1 || res["accepted_terms"].(map[string]any)["policy_version"].(float64) != 0 {
		t.Fatalf("create %d %v", c, res)
	}
	ref := res["reference"].(string)
	e.do("POST", "/reservations", tok, "k1", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":3}`) // replay
	// no-op amendment records nothing
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{"party_size":3,"expected_revision":1}`); c != 200 || m["revision"].(float64) != 1 {
		t.Fatalf("noop %d %v", c, m)
	}
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{"party_size":2,"expected_revision":5}`); c != 409 || code(m) != "stale_revision" {
		t.Fatalf("stale %d %v", c, m)
	}
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{"party_size":2,"expected_revision":0}`); c != 422 {
		t.Fatalf("expected_revision 0 %d %v", c, m)
	}
	if c, m := e.do("PATCH", "/reservations/"+ref, tok, "", `{"party_size":2,"expected_revision":1}`); c != 200 || m["revision"].(float64) != 2 {
		t.Fatalf("amend %d %v", c, m)
	}
	if c, m := e.do("POST", "/reservations/"+ref+"/cancel", tok, "", ""); c != 200 || m["revision"].(float64) != 3 {
		t.Fatalf("cancel %d %v", c, m)
	}
	e.do("POST", "/reservations/"+ref+"/cancel", tok, "", "") // already cancelled: nothing recorded

	c, h := e.do("GET", "/reservations/"+ref+"/history", tok, "", "")
	entries := h["entries"].([]any)
	if c != 200 || len(entries) != 3 {
		t.Fatalf("history %d %v", c, h)
	}
	for i, want := range []string{"created", "changed", "cancelled"} {
		en := entries[i].(map[string]any)
		if en["event"] != want || en["seq"].(float64) != float64(i+1) || en["revision"].(float64) != float64(i+1) {
			t.Fatalf("entry %d: %v", i, en)
		}
	}
	if ch := entries[1].(map[string]any)["changes"].([]any); len(ch) != 1 || ch[0].(map[string]any)["field"] != "party_size" {
		t.Fatalf("changed entry: %v", entries[1])
	}
	if ch := entries[0].(map[string]any)["changes"].([]any); len(ch) != 3 || ch[0].(map[string]any)["from"] != nil {
		t.Fatalf("created entry: %v", entries[0])
	}
	other := login(t, e, "mgr@example.com")
	for _, tk := range []string{"", other} {
		if c, _ := e.do("GET", "/reservations/"+ref+"/history", tk, "", ""); c != 404 {
			t.Fatalf("history leak %d", c)
		}
		if c, _ := e.do("GET", "/reservations/"+ref+"/decision", tk, "", ""); c != 404 {
			t.Fatalf("decision leak %d", c)
		}
	}
	if c, d := e.do("GET", "/reservations/"+ref+"/decision", tok, "", ""); c != 200 || d["revision"].(float64) != 3 {
		t.Fatalf("decision %d %v", c, d)
	}
}

func TestPolicies(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/_test/reset", "", "", s3Fixture)
	ada, mgr := login(t, e, "ada@example.com"), login(t, e, "mgr@example.com")
	pol := `{"effective_from":"2099-10-01","slot_minutes":30,"reservation_duration_minutes":120,"cancellation_cutoff_minutes":60,
"opening_hours":[{"weekday":"thu","opens":"18:00","closes":"23:00"}],"capacities":{"t_1":2,"t_2":6}}`
	if c, m := e.do("POST", "/restaurants/r_anker/policies", ada, "p1", pol); c != 403 || code(m) != "forbidden" {
		t.Fatalf("non-manager %d %v", c, m)
	}
	if c, m := e.do("POST", "/restaurants/r_anker/policies", mgr, "", pol); c != 400 {
		t.Fatalf("no key %d %v", c, m)
	}
	c, p := e.do("POST", "/restaurants/r_anker/policies", mgr, "p1", pol)
	if c != 201 || p["policy_version"].(float64) != 1 {
		t.Fatalf("publish %d %v", c, p)
	}
	if c, p2 := e.do("POST", "/restaurants/r_anker/policies", mgr, "p1", pol); c != 200 || p2["policy_version"].(float64) != 1 {
		t.Fatalf("replay %d %v", c, p2)
	}
	if c, m := e.do("POST", "/restaurants/r_anker/policies", mgr, "p2", strings.Replace(pol, `"slot_minutes":30`, `"slot_minutes":0`, 1)); c != 422 {
		t.Fatalf("invalid policy %d %v", c, m)
	}
	_, l := e.do("GET", "/restaurants/r_anker/policies", "", "", "")
	if len(l["policies"].([]any)) != 1 {
		t.Fatalf("list %v", l)
	}
	// 2099-10-01 uses policy 1: two-hour stay, t_2 holds 6
	c, r1 := e.do("POST", "/reservations", ada, "b1", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-10-01T19:00","party_size":6}`)
	if c != 201 || r1["ends_at"] != "2099-10-01T21:00:00+02:00" || r1["accepted_terms"].(map[string]any)["policy_version"].(float64) != 1 {
		t.Fatalf("policy booking %d %v", c, r1)
	}
	// the earlier date keeps policy 0
	if c, m := e.do("POST", "/reservations", ada, "b2", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":6}`); c != 422 || code(m) != "party_exceeds_capacity" {
		t.Fatalf("policy 0 capacity %d %v", c, m)
	}
	// amending to the earlier date swaps the accepted terms
	ref := r1["reference"].(string)
	c, m := e.do("PATCH", "/reservations/"+ref, ada, "", `{"starts_at_local":"2099-09-24T19:00","party_size":4}`)
	if c != 200 || m["accepted_terms"].(map[string]any)["policy_version"].(float64) != 0 || m["ends_at"] != "2099-09-24T20:30:00+02:00" || m["revision"].(float64) != 2 {
		t.Fatalf("amend across policies %d %v", c, m)
	}
}

func TestSeries(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/_test/reset", "", "", s3Fixture)
	tok := login(t, e, "ada@example.com")
	_, a := e.do("POST", "/reservations", tok, "a1", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":4}`)
	ref := a["reference"].(string)
	if c, _ := e.do("POST", "/series", tok, "", `{"anchor_reference":"`+ref+`","count":1,"interval_weeks":1}`); c != 422 {
		t.Fatalf("count 1: %d", c)
	}
	c, s := e.do("POST", "/series", tok, "", `{"anchor_reference":"`+ref+`","count":3,"interval_weeks":1}`)
	if c != 201 || s["revision"].(float64) != 1 {
		t.Fatalf("series %d %v", c, s)
	}
	occ := s["occurrences"].([]any)
	if len(occ) != 3 || occ[0].(map[string]any)["reference"] != ref {
		t.Fatalf("occurrences %v", occ)
	}
	if got := occ[2].(map[string]any)["reservation"].(map[string]any)["starts_at_local"]; got != "2099-10-08T19:00" {
		t.Fatalf("third occurrence %v", got)
	}
	if c, m := e.do("POST", "/series", tok, "", `{"anchor_reference":"`+ref+`","count":2,"interval_weeks":1}`); c != 409 || code(m) != "already_in_series" {
		t.Fatalf("again %d %v", c, m)
	}
	sid := s["series_id"].(string)
	ref1 := occ[1].(map[string]any)["reference"].(string)
	ref2 := occ[2].(map[string]any)["reference"].(string)
	e.do("PATCH", "/reservations/"+ref1, tok, "", `{"party_size":3}`)
	e.do("POST", "/reservations/"+ref2+"/cancel", tok, "", "")
	c, g := e.do("GET", "/series/"+sid, tok, "", "")
	o := g["occurrences"].([]any)
	if c != 200 || g["revision"].(float64) != 3 || o[1].(map[string]any)["exception"] != true || o[2].(map[string]any)["exception"] != false ||
		o[2].(map[string]any)["reservation"].(map[string]any)["status"] != "cancelled" || o[0].(map[string]any)["reservation"].(map[string]any)["status"] != "confirmed" {
		t.Fatalf("series after changes %d %v", c, g)
	}
	other := login(t, e, "mgr@example.com")
	if c, _ := e.do("GET", "/series/"+sid, other, "", ""); c != 404 {
		t.Fatalf("series leak %d", c)
	}
	// export/import keeps the series and history
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, httptest.NewRequest("GET", "/_test/export", nil))
	e.do("POST", "/_test/reset", "", "", `{}`)
	if c, m := e.do("POST", "/_test/import", "", "", rr.Body.String()); c != 204 {
		t.Fatalf("import %d %v", c, m)
	}
	if c, g := e.do("GET", "/series/"+sid, tok, "", ""); c != 200 || g["revision"].(float64) != 3 {
		t.Fatalf("series after import %d %v", c, g)
	}
	if _, h := e.do("GET", "/reservations/"+ref1+"/history", tok, "", ""); len(h["entries"].([]any)) != 2 {
		t.Fatalf("history after import %v", h)
	}
}
