# Tablekeeper Stage 4 - Full Specification

## Stages 1-3 Continue to Apply
All stage 1, 2, and 3 functionality remains unchanged.

## New in Stage 4

### 1. Seating Changes After Table Closure

#### POST /restaurants/{id}/replans (manager + idempotency key required)
```json
{
  "table_id": "t_2",
  "from": "2026-09-28T18:00:00+02:00",
  "to": "2026-09-28T23:00:00+02:00"
}
```

- Creates a seating plan to reassign bookings during table closure
- Closure interval is half-open [from, to)
- Considers all confirmed bookings overlapping the interval
- Each booking must retain: reference, owner, party size, start, end, accepted terms
- Assigns single table or declared pair with enough capacity per booking's accepted terms
- No conflicts with fixed bookings, other assignments, or closures

**Returns 201:**
```json
{
  "plan_id": "opaque",
  "restaurant_revision": 4,
  "closure": {"table_id": "t_2", "from": "...", "to": "..."},
  "assignments": [
    {"reference": "ABC12345", "table_ids": ["t_1"], "changed": true}
  ],
  "moved_count": 1,
  "unused_seats": 0
}
```

**Optimization (minimize in order):**
1. Number of bookings whose table set changes
2. Total unused seats (capacity - party_size)
3. Vector of option ranks (singles first, then pairs, ascending by reference)

**Errors:**
- Invalid interval: 422 validation_failed
- Unknown table: 404
- Planning limit exceeded (>6 tables, >4 pairs, >6 bookings): 422 planning_limit
- No feasible plan: 409 no_feasible_plan

#### POST /restaurants/{id}/replans/{plan_id}/apply (manager + idempotency key required)
Body: `{}`

- Applies the plan atomically
- Returns 201 with plan_id, restaurant_revision, reservations array

**Errors:**
- Unknown plan or wrong restaurant: 404
- Stale restaurant revision: 409 stale_plan
- Plan already applied with different key: 409 plan_already_applied
- Replay returns original response with 200

**On Apply:**
- Records closure and all assignments
- Each moved booking: increments revision, gains `reassigned` history entry with table_ids change and plan_id
- Restaurant revision increments once for whole plan
- Closures exclude tables from availability

### 2. Amend Recurring Reservations

#### POST /series/{series_id}/amend (owner-only, idempotent write)
```json
{
  "expected_revision": 3,
  "from_index": 2,
  "local_time": "20:00"
}
```

- expected_revision: positive integer
- from_index: 0 to count-1
- local_time: HH:MM in 00:00..23:59

**Process:**
1. Check stale_revision (409) before any cutoff/validation
2. For indices >= from_index (excluding cancelled and exceptions):
   - Change clock time, keep date
   - Check old accepted cutoff
   - Adopt policy for resulting date
3. Validate no conflicts

**On Success:**
- Return 201 with current series response
- Each changed occurrence gains one history entry and revision increment
- Series revision +1, restaurant revision +1 (if anything changed)
- No-op changes nothing

**Errors:**
- Invalid input: 422 validation_failed
- Stale series revision: 409 stale_revision
- Non-occupancy errors: in occurrence-index order
- Occupancy conflict: table_unavailable

### 3. Combined Table Changes
Seating repairs may move series occurrences, preserving:
- Exception flags
- Scheduled dates
- Identities
- Accepted terms

### 4. Export/Import Compatibility
- Must accept stages 1-3 exports
- Support series with moved/cancelled occurrences

## Error Codes (new)
| Status | Code | When |
|--------|------|------|
| 422 | planning_limit | Too many tables/pairs/bookings |
| 409 | no_feasible_plan | Cannot create feasible seating plan |
| 409 | stale_plan | Restaurant revision changed since plan creation |
| 409 | plan_already_applied | Plan applied with different key |