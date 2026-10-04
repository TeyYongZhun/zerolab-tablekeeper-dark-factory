package main

import (
	"encoding/json"
	"sort"
	"time"
)

// buildChanges lists changed fields in the order table, start, party size.
// For a created reservation (hasOld false) every field changes from null.
// A pair of tables is reported as table_ids, a single table as table_id.
func buildChanges(hasOld bool, oldTables []string, oldStart string, oldParty int, newTables []string, newStart string, newParty int) []map[string]any {
	out := []map[string]any{}
	if !hasOld || !sameSet(oldTables, newTables) {
		if len(oldTables) > 1 || len(newTables) > 1 {
			var from any
			if hasOld {
				from = oldTables
			}
			out = append(out, map[string]any{"field": "table_ids", "from": from, "to": newTables})
		} else {
			var from any
			if hasOld {
				from = oldTables[0]
			}
			out = append(out, map[string]any{"field": "table_id", "from": from, "to": newTables[0]})
		}
	}
	if !hasOld || oldStart != newStart {
		var from any
		if hasOld {
			from = oldStart
		}
		out = append(out, map[string]any{"field": "starts_at_local", "from": from, "to": newStart})
	}
	if !hasOld || oldParty != newParty {
		var from any
		if hasOld {
			from = oldParty
		}
		out = append(out, map[string]any{"field": "party_size", "from": from, "to": newParty})
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string{}, a...)
	y := append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// recordHistory appends a history entry for v at its current revision and
// terms. at is an RFC 3339 instant; empty means now.
func recordHistory(q querier, loc *time.Location, v *reservation, event string, changes []map[string]any, at string) error {
	return recordHistoryPlan(q, loc, v, event, changes, at, "")
}

func recordHistoryPlan(q querier, loc *time.Location, v *reservation, event string, changes []map[string]any, at, planID string) error {
	t := time.Now()
	if at != "" {
		if p, err := time.Parse(time.RFC3339, at); err == nil {
			t = p
		}
	}
	var seq int
	if err := q.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM reservation_history WHERE reservation_id=?`, v.ID).Scan(&seq); err != nil {
		return err
	}
	cj, _ := json.Marshal(changes)
	_, err := q.Exec(`INSERT INTO reservation_history(reservation_id,seq,at,event,revision,terms,changes,plan_id) VALUES(?,?,?,?,?,?,?,?)`,
		v.ID, seq, fmtRFC(t, loc), event, v.Rev, v.Terms.json(), string(cj), planID)
	return err
}

// touchSeries bumps the revision of the series a reservation belongs to and
// optionally marks the occurrence as an exception.
func touchSeries(q querier, reservationID string, markException bool) error {
	var sid string
	err := q.QueryRow(`SELECT series_id FROM series_occurrences WHERE reservation_id=?`, reservationID).Scan(&sid)
	if err != nil {
		return nil // not part of a series
	}
	if _, err := q.Exec(`UPDATE series SET revision=revision+1 WHERE id=?`, sid); err != nil {
		return err
	}
	if markException {
		_, err = q.Exec(`UPDATE series_occurrences SET exception=1 WHERE reservation_id=?`, reservationID)
	}
	return err
}
