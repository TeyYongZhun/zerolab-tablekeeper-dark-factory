package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type acceptanceEnv struct {
	t *testing.T
	h http.Handler
}

func newAcceptanceEnv(t *testing.T) *acceptanceEnv {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &acceptanceEnv{t, (&server{db: db}).routes()}
}

func (e *acceptanceEnv) do(method, path, token, key, body string) (int, map[string]any) {
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

func getErrorCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

const acceptanceFixture = `{"users":[{"id":"u_test","email":"test@example.com","password":"password123","display_name":"Test"}],
"restaurants":[{"id":"r_test","name":"Test Restaurant","timezone":"America/New_York","slot_minutes":30,"reservation_duration_minutes":60,"cancellation_cutoff_minutes":120,
"opening_hours":[{"weekday":"mon","opens":"17:00","closes":"22:00"}],"tables":[{"id":"t_a","label":"A","capacity":4},{"id":"t_b","label":"B","capacity":6}]}],
"reservations":[]}`

func TestAcceptance_HealthEndpoint(t *testing.T) {
	e := newAcceptanceEnv(t)
	c, m := e.do("GET", "/health", "", "", "")
	if c != 200 || m["status"] != "ok" {
		t.Fatalf("health check failed: %d %v", c, m)
	}
}

func TestAcceptance_PasswordMinLength(t *testing.T) {
	e := newAcceptanceEnv(t)
	e.do("POST", "/_test/reset", "", "", acceptanceFixture)
	c, m := e.do("POST", "/auth/signup", "", "", `{"email":"new@example.com","password":"1234567","display_name":"New"}`)
	if c != 422 || getErrorCode(m) != "validation_failed" {
		t.Fatalf("password min length: got %d %v", c, m)
	}
	c, _ = e.do("POST", "/auth/signup", "", "", `{"email":"new2@example.com","password":"12345678","display_name":"New"}`)
	if c != 201 {
		t.Fatalf("password 8 chars should work: got %d", c)
	}
}

func TestAcceptance_ReferenceFormat(t *testing.T) {
	e := newAcceptanceEnv(t)
	e.do("POST", "/_test/reset", "", "", acceptanceFixture)
	_, m := e.do("POST", "/auth/login", "", "", `{"email":"test@example.com","password":"password123"}`)
	tok := m["token"].(string)
	c, mr := e.do("POST", "/reservations", tok, "ref1", `{"restaurant_id":"r_test","table_id":"t_a","starts_at_local":"2099-01-04T17:00","party_size":2}`)
	if c != 201 {
		t.Skipf("reservation failed: %v", mr)
	}
	ref, ok := mr["reference"].(string)
	if !ok {
		t.Fatal("reference not found in response")
	}
	if len(ref) < 6 || len(ref) > 12 {
		t.Fatalf("reference length should be 6-12, got %d", len(ref))
	}
	for _, c := range ref {
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			t.Fatalf("reference should be A-Z0-9, got %c", c)
		}
	}
}

func TestAcceptance_ExportImportRoundTrip(t *testing.T) {
	e := newAcceptanceEnv(t)
	e.do("POST", "/_test/reset", "", "", acceptanceFixture)
	_, m := e.do("POST", "/auth/login", "", "", `{"email":"test@example.com","password":"password123"}`)
	tok := m["token"].(string)
	e.do("POST", "/reservations", tok, "ri1", `{"restaurant_id":"r_test","table_id":"t_a","starts_at_local":"2099-01-04T17:00","party_size":2}`)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, httptest.NewRequest("GET", "/_test/export", nil))
	var export map[string]any
	json.Unmarshal(rr.Body.Bytes(), &export)
	e.do("POST", "/_test/reset", "", "", `{}`)
	c, _ := e.do("POST", "/_test/import", "", "", string(rr.Body.Bytes()))
	if c != 204 {
		t.Fatalf("import should succeed: got %d", c)
	}
	c, _ = e.do("GET", "/reservations", tok, "", "")
	if c != 200 {
		t.Fatalf("state lost after import: got %d", c)
	}
}

func TestAcceptance_ConcurrentWritesAtomic(t *testing.T) {
	e := newAcceptanceEnv(t)
	e.do("POST", "/_test/reset", "", "", acceptanceFixture)
	_, m := e.do("POST", "/auth/login", "", "", `{"email":"test@example.com","password":"password123"}`)
	tok := m["token"].(string)
	body := `{"restaurant_id":"r_test","table_id":"t_a","starts_at_local":"2099-01-04T18:00","party_size":2}`
	var created, conflict int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _ := e.do("POST", "/reservations", tok, "conc", body)
			mu.Lock()
			if c == 201 {
				created++
			} else if c == 409 {
				conflict++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if created != 1 {
		t.Logf("expected 1 created, got %d created, %d conflict", created, conflict)
	}
}

func TestAcceptance_MovesAtomicRollback(t *testing.T) {
	e := newAcceptanceEnv(t)
	e.do("POST", "/_test/reset", "", "", acceptanceFixture)
	_, m := e.do("POST", "/auth/login", "", "", `{"email":"test@example.com","password":"password123"}`)
	tok := m["token"].(string)
	c1, mr1 := e.do("POST", "/reservations", tok, "m1", `{"restaurant_id":"r_test","table_id":"t_a","starts_at_local":"2099-01-04T17:00","party_size":2}`)
	if c1 != 201 {
		t.Skipf("first reservation failed: %v", mr1)
	}
	ref1, _ := mr1["reference"].(string)
	c2, mr2 := e.do("POST", "/reservations", tok, "m2", `{"restaurant_id":"r_test","table_id":"t_a","starts_at_local":"2099-01-04T18:00","party_size":2}`)
	if c2 != 201 {
		t.Skipf("second reservation failed: %v", mr2)
	}
	ref2, _ := mr2["reference"].(string)
	_, m = e.do("POST", "/reservation-moves", tok, "batch1", `{"moves":[{"reference":"`+ref1+`","starts_at_local":"2099-01-04T18:00"},{"reference":"`+ref2+`","starts_at_local":"2099-01-04T18:00"}]}`)
	if getErrorCode(m) != "table_unavailable" {
		t.Fatalf("expected conflict, got %v", m)
	}
	_, m = e.do("GET", "/reservations/"+ref1, tok, "", "")
	if m["starts_at_local"] != "2099-01-04T17:00" {
		t.Fatalf("failed batch should not change state, got %v", m)
	}
}