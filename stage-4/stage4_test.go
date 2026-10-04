package main

import (
	"net/http/httptest"
	"testing"
)

const s4Fixture = `{"users":[
{"id":"u_ada","email":"ada@example.com","password":"correct horse","display_name":"Ada"},
{"id":"u_mgr","email":"mgr@example.com","password":"correct horse","display_name":"Manager"}],
"restaurants":[{"id":"r_anker","name":"Zum Anker","timezone":"Europe/Berlin","slot_minutes":30,"reservation_duration_minutes":90,"cancellation_cutoff_minutes":120,
"manager_user_ids":["u_mgr"],
"opening_hours":[{"weekday":"thu","opens":"18:00","closes":"23:00"}],
"tables":[{"id":"t_1","label":"1","capacity":2},{"id":"t_2","label":"2","capacity":4},{"id":"t_3","label":"3","capacity":4}],
"combinable":[["t_1","t_3"]]}],"reservations":[]}`

const closureBody = `{"table_id":"t_2","from":"2099-09-24T18:00:00+02:00","to":"2099-09-24T23:00:00+02:00"}`

func TestReplanApply(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/_test/reset", "", "", s4Fixture)
	ada, mgr := login(t, e, "ada@example.com"), login(t, e, "mgr@example.com")
	_, a := e.do("POST", "/reservations", ada, "a", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":3}`)
	ref := a["reference"].(string)

	if c, m := e.do("POST", "/restaurants/r_anker/replans", ada, "p0", closureBody); c != 403 {
		t.Fatalf("non-manager %d %v", c, m)
	}
	if c, m := e.do("POST", "/restaurants/r_anker/replans", mgr, "p0", `{"table_id":"t_2","from":"2099-09-24T23:00:00+02:00","to":"2099-09-24T18:00:00+02:00"}`); c != 422 {
		t.Fatalf("bad interval %d %v", c, m)
	}
	if c, m := e.do("POST", "/restaurants/r_anker/replans", mgr, "p0", `{"table_id":"t_9","from":"2099-09-24T18:00:00+02:00","to":"2099-09-24T23:00:00+02:00"}`); c != 404 {
		t.Fatalf("unknown table %d %v", c, m)
	}
	c, plan := e.do("POST", "/restaurants/r_anker/replans", mgr, "p1", closureBody)
	if c != 201 {
		t.Fatalf("plan %d %v", c, plan)
	}
	as := plan["assignments"].([]any)
	first := as[0].(map[string]any)
	if len(as) != 1 || first["reference"] != ref || first["changed"] != true || first["table_ids"].([]any)[0] != "t_3" ||
		plan["moved_count"].(float64) != 1 || plan["unused_seats"].(float64) != 1 {
		t.Fatalf("plan content %v", plan)
	}
	if c, again := e.do("POST", "/restaurants/r_anker/replans", mgr, "p1", closureBody); c != 200 || again["plan_id"] != plan["plan_id"] {
		t.Fatalf("plan replay %d %v", c, again)
	}
	pid := plan["plan_id"].(string)
	_, plan2 := e.do("POST", "/restaurants/r_anker/replans", mgr, "p2", closureBody)
	applyPath := "/restaurants/r_anker/replans/" + pid + "/apply"
	c, ap := e.do("POST", applyPath, mgr, "ap1", `{}`)
	if c != 201 || ap["restaurant_revision"].(float64) != plan["restaurant_revision"].(float64)+1 {
		t.Fatalf("apply %d %v", c, ap)
	}
	if c, rp := e.do("POST", applyPath, mgr, "ap1", `{}`); c != 200 || rp["plan_id"] != pid {
		t.Fatalf("apply replay %d %v", c, rp)
	}
	if c, m := e.do("POST", applyPath, mgr, "ap2", `{}`); c != 409 || code(m) != "plan_already_applied" {
		t.Fatalf("applied twice %d %v", c, m)
	}
	if c, m := e.do("POST", "/restaurants/r_anker/replans/"+plan2["plan_id"].(string)+"/apply", mgr, "ap3", `{}`); c != 409 || code(m) != "stale_plan" {
		t.Fatalf("stale %d %v", c, m)
	}
	if c, m := e.do("POST", "/restaurants/r_anker/replans/nope/apply", mgr, "ap4", `{}`); c != 404 {
		t.Fatalf("unknown plan %d %v", c, m)
	}
	_, g := e.do("GET", "/reservations/"+ref, ada, "", "")
	if g["table_ids"].([]any)[0] != "t_3" || g["revision"].(float64) != 2 {
		t.Fatalf("moved booking %v", g)
	}
	_, h := e.do("GET", "/reservations/"+ref+"/history", ada, "", "")
	last := h["entries"].([]any)[1].(map[string]any)
	if last["event"] != "reassigned" || last["plan_id"] != pid || last["changes"].([]any)[0].(map[string]any)["field"] != "table_ids" {
		t.Fatalf("history %v", h)
	}
	_, av := e.do("GET", "/availability?restaurant_id=r_anker&date=2099-09-24&party_size=2&explain=true", "", "", "")
	ids := av["slots"].([]any)[0].(map[string]any)["available_table_ids"].([]any)
	for _, id := range ids {
		if id == "t_2" {
			t.Fatalf("closed table still available: %v", ids)
		}
	}
	if c, m := e.do("POST", "/reservations", ada, "z", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T21:00","party_size":2}`); c != 409 || code(m) != "table_unavailable" {
		t.Fatalf("booking a closed table %d %v", c, m)
	}
}

func TestReplanInfeasible(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/_test/reset", "", "", s4Fixture)
	ada, mgr := login(t, e, "ada@example.com"), login(t, e, "mgr@example.com")
	e.do("POST", "/reservations", ada, "a", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":3}`)
	e.do("POST", "/reservations", ada, "b", `{"restaurant_id":"r_anker","table_id":"t_3","starts_at_local":"2099-09-24T19:00","party_size":4}`)
	if c, m := e.do("POST", "/restaurants/r_anker/replans", mgr, "p1", closureBody); c != 409 || code(m) != "no_feasible_plan" {
		t.Fatalf("infeasible %d %v", c, m)
	}
}

func TestSeriesAmend(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/_test/reset", "", "", s4Fixture)
	ada, mgr := login(t, e, "ada@example.com"), login(t, e, "mgr@example.com")
	_, a := e.do("POST", "/reservations", ada, "a", `{"restaurant_id":"r_anker","table_id":"t_2","starts_at_local":"2099-09-24T19:00","party_size":4}`)
	_, s := e.do("POST", "/series", ada, "s", `{"anchor_reference":"`+a["reference"].(string)+`","count":4,"interval_weeks":1}`)
	sid := s["series_id"].(string)
	occ := s["occurrences"].([]any)
	// make occurrence 3 an exception and cancel occurrence 2's sibling is untouched
	e.do("PATCH", "/reservations/"+occ[3].(map[string]any)["reference"].(string), ada, "", `{"party_size":3}`)
	path := "/series/" + sid + "/amend"
	body := func(rev int, idx int) string {
		return `{"expected_revision":` + string(rune('0'+rev)) + `,"from_index":` + string(rune('0'+idx)) + `,"local_time":"20:00"}`
	}
	if c, m := e.do("POST", path, ada, "m1", body(1, 1)); c != 409 || code(m) != "stale_revision" {
		t.Fatalf("stale %d %v", c, m)
	}
	if c, m := e.do("POST", path, ada, "m1", `{"expected_revision":2,"from_index":1,"local_time":"25:00"}`); c != 422 {
		t.Fatalf("bad time %d %v", c, m)
	}
	if c, _ := e.do("POST", path, mgr, "m1", body(2, 1)); c != 404 {
		t.Fatalf("non-owner %d", c)
	}
	if c, _ := e.do("POST", path, ada, "", body(2, 1)); c != 400 {
		t.Fatalf("no key %d", c)
	}
	c, out := e.do("POST", path, ada, "m1", body(2, 1))
	if c != 201 || out["revision"].(float64) != 3 {
		t.Fatalf("amend %d %v", c, out)
	}
	o := out["occurrences"].([]any)
	times := []string{"2099-09-24T19:00", "2099-10-01T20:00", "2099-10-08T20:00", "2099-10-15T19:00"}
	for i, want := range times {
		r := o[i].(map[string]any)["reservation"].(map[string]any)
		if i == 3 {
			// the exception keeps its time
			if r["starts_at_local"] != "2099-10-15T19:00" || o[i].(map[string]any)["exception"] != true {
				t.Fatalf("exception occurrence changed: %v", o[i])
			}
			continue
		}
		if r["starts_at_local"] != want || (i > 0 && r["revision"].(float64) != 2) || o[i].(map[string]any)["exception"] != false {
			t.Fatalf("occurrence %d: %v", i, o[i])
		}
	}
	if c, rp := e.do("POST", path, ada, "m1", body(2, 1)); c != 200 || rp["revision"].(float64) != 3 {
		t.Fatalf("replay %d %v", c, rp)
	}
	// no-op: same time again changes nothing
	c, noop := e.do("POST", path, ada, "m2", body(3, 1))
	if c != 201 || noop["revision"].(float64) != 3 {
		t.Fatalf("noop %d %v", c, noop)
	}
	// export/import keeps closures, plans and series
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, httptest.NewRequest("GET", "/_test/export", nil))
	e.do("POST", "/_test/reset", "", "", `{}`)
	if c, m := e.do("POST", "/_test/import", "", "", rr.Body.String()); c != 204 {
		t.Fatalf("import %d %v", c, m)
	}
	if c, g := e.do("GET", "/series/"+sid, ada, "", ""); c != 200 || g["revision"].(float64) != 3 {
		t.Fatalf("series after import %d %v", c, g)
	}
}
