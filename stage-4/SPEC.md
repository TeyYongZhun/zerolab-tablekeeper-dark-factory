# Tablekeeper Stage 3 - Full Specification

## Stages 1 & 2 Continue to Apply
All stage 1 and 2 functionality remains unchanged.

## New in Stage 3

### 1. Availability Explanations

#### GET /availability with explain=true
```http
GET /availability?restaurant_id=r_anker&date=2026-09-24&party_size=4&explain=true
```

Response adds `explain` to each slot:
```json
{
  "slots": [{
    "starts_at_local": "2026-09-24T18:00",
    "starts_at": "2026-09-24T18:00:00+02:00",
    "available_table_ids": ["t_2"],
    "explain": [
      { "table_id": "t_1", "policy_version": 0, "available": false,
        "rules": [
          { "rule": "capacity", "holds": false },
          { "rule": "no_overlap", "holds": true }
        ] },
      { "table_id": "t_2", "policy_version": 0, "available": true,
        "rules": [
          { "rule": "capacity", "holds": true },
          { "rule": "no_overlap", "holds": true }
        ] }
    ]
  }]
}
```

Rules order: capacity, then no_overlap. Every table appears once in fixture order.
`explain=true` must be exact; other values = 422 validation_failed.

### 2. Reservation History

#### GET /reservations/{reference}/history
```json
{
  "reference": "ABC12345",
  "entries": [
    { "seq": 1, "at": "2026-09-17T12:00:00+02:00", "event": "created",
      "revision": 1, "accepted_terms": {...},
      "changes": [...] },
    { "seq": 2, "at": "2026-09-17T12:05:00+02:00", "event": "changed",
      "revision": 2, "accepted_terms": {...},
      "changes": [...] },
    { "seq": 3, "at": "2026-09-17T12:09:00+02:00", "event": "cancelled",
      "revision": 3, "accepted_terms": {...},
      "changes": [] }
  ]
}
```

- Owner-only (404 if not owner)
- seq starts at 1, increments by 1
- created: all fields from null
- changed: only changed fields in order table_id, starts_at_local, party_size
- cancelled: empty changes, nothing follows
- Replays record nothing

#### GET /reservations/{reference}/decision
```json
{
  "reference": "ABC12345",
  "revision": 1,
  "accepted_terms": {...}
}
```
- Owner-only 404 rule (no auth = 404)

### 3. Policies

#### Restaurant fixture gains manager_user_ids
```json
{
  "id": "r_anker",
  "manager_user_ids": ["u_manager1", "u_manager2"],
  ...
}
```

#### POST /restaurants/{id}/policies (requires idempotency key)
```json
{
  "effective_from": "2026-09-28",
  "slot_minutes": 30,
  "reservation_duration_minutes": 120,
  "cancellation_cutoff_minutes": 60,
  "opening_hours": [{"weekday": "mon", "opens": "18:00", "closes": "23:00"}],
  "capacities": {"t_1": 2, "t_2": 4, "t_3": 6}
}
```

Returns 201 with policy plus policy_version (starts at 1, increments per restaurant).

#### GET /restaurants/{id}/policies (public)
```json
{"policies": [...]}
```
Publication order, omitting policy 0.

#### Policy Selection
For a booking's local start date: greatest effective_from <= start_date, ties = greatest policy_version.

### 4. Reservation Response Changes

All reservation responses gain:
```json
{
  "revision": 1,
  "accepted_terms": {
    "policy_version": 0,
    "slot_minutes": 30,
    "reservation_duration_minutes 90,
    "cancellation_cutoff_minutes": 120,
    "opening_hours": [...],
    "capacities": {"t_1": 2, "t_2": 4}
  }
}
```

### 5. PATCH Updates

- `expected_revision` optional: positive integer, mismatch = 409 stale_revision
- Check old accepted cutoff first
- Validate against resulting date's policy
- Real amendment: replace terms, end time, increment revision
- No-op: retain terms, revision, no history entry
- Cancel increments revision once

### 6. Recurring Reservations

#### POST /series
```json
{
  "anchor_reference": "ABC12345",
  "count": 8,
  "interval_weeks": 1
}
```

- Anchor: caller-owned, confirmed, not in cutoff
- count: 2-12 (includes anchor)
- interval_weeks: 1-4

Returns 201:
```json
{
  "series_id": "opaque",
  "revision": 1,
  "interval_weeks": 1,
  "occurrences": [
    {"index": 0, "reference": "ABC12345", "exception": false, "reservation": {...}},
    ...
  ]
}
```

Each occurrence:
- Index 0 = anchor
- Later occurrences: anchor date + i*interval_weeks*7 days
- Each selects policy for its date
- All have separate references, histories

#### GET /series/{series_id}
Same shape, with current states. Owner-only (404 otherwise).

#### Exception Handling
- PATCH occurrence: marks as exception=true, increments series revision
- Cancel: increments series revision, retains cancelled (not exception)
- Cancel anchor: does NOT cancel siblings

### 7. Export/Import Compatibility
- Must accept stage-1 and stage-2 exports
- Series must work on imported reservations

## Combined Table Changes
History for pair operations uses `table_ids` instead of `table_id`.

## data-testid Attributes
No new screens. Existing screens continue to work.

## Error Codes (new)
| Status | Code | When |
|--------|------|------|
| 403 | forbidden | Auth but not restaurant manager |
| 409 | stale_revision | expected_revision doesn't match |
| 409 | already_in_series | Anchor already adopted |
| 422 | invalid_local_time | Adoption creates non-existent DST time |