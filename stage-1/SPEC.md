# Tablekeeper Stage 1 - Full Wire Contract

## Environment
- Build in Docker: `docker build -t tablekeeper .` from golang image
- Pure-Go SQLite (modernc.org/sqlite) with CGO off is acceptable
- Runtime: Linux in container, no network access at runtime

## Error Body Envelope
All 4xx and 5xx responses:
```json
{ "error": { "code": "table_unavailable", "message": "human readable" } }
```

## Auth Details

### Token Scheme
```
Authorization: Bearer <token>
```

### Signup
```
POST /auth/signup
Request: { "email": "a@example.com", "password": "correct horse", "display_name": "Ada" }
Response 201: { "user_id": "u_1", "display_name": "Ada", "token": "..." }
Response 409: { "error": { "code": "email_taken", ... } }
```

### Login
```
POST /auth/login
Request: { "email": "a@example.com", "password": "correct horse" }
Response 200: { "user_id": "u_1", "display_name": "Ada", "token": "..." }
Response 401: { "error": { "code": "unauthenticated", ... } }
```

### Validation Rules
- Password minimum 8 characters → 422 validation_failed
- Email must be form `local@domain` → 422 validation_failed
- Duplicate email → 409 email_taken
- Wrong password/unknown email → 401 unauthenticated

### Token
- Bearer token in Authorization header
- Tokens do not expire
- Multiple tokens per user allowed

## Public Endpoints

### GET /restaurants
```
Response 200:
{ "restaurants": [ { "id": "r_anker", "name": "Zum Anker", "timezone": "Europe/Berlin" } ] }
```

### GET /restaurants/{id}
```
Response 200:
{
  "id": "r_anker",
  "name": "Zum Anker",
  "timezone": "Europe/Berlin",
  "slot_minutes": 30,
  "reservation_duration_minutes": 90,
  "cancellation_cutoff_minutes": 120,
  "opening_hours": [
    { "weekday": "thu", "opens": "18:00", "closes": "23:00" },
    { "weekday": "fri", "opens": "18:00", "closes": "23:30" }
  ],
  "tables": [
    { "id": "t_1", "label": "1", "capacity": 2 },
    { "id": "t_2", "label": "2", "capacity": 4 }
  ]
}
```
404: { "error": { "code": "not_found", ... } }

### GET /availability
```
GET /availability?restaurant_id=r_anker&date=2026-09-24&party_size=4
```
Required query params: restaurant_id, date, party_size - all required, missing = 422 validation_failed
Integer params must be plain digits: `1e9`, `4.0`, `+4` = 422 validation_failed

```
Response 200:
{
  "restaurant_id": "r_anker",
  "date": "2026-09-24",
  "timezone": "Europe/Berlin",
  "slots": [
    {
      "starts_at_local": "2026-09-24T18:00",
      "starts_at": "2026-09-24T18:00:00+02:00",
      "available_table_ids": ["t_2"]
    }
  ]
}
```
- `starts_at_local` = YYYY-MM-DDTHH:MM (no offset, no Z)
- `starts_at` = RFC 3339 with offset (e.g. +02:00)
- Slot appears at every slot_minutes from opens where slot + reservation_duration <= closes
- available_table_ids: tables with capacity >= party_size AND no overlapping confirmed reservation, in fixture order, empty list if none
- Closed day: { "slots": [] }

## Protected Endpoints (require Authorization: Bearer <token>)

### POST /reservations
```
POST /reservations
Authorization: Bearer <token>
Idempotency-Key: <string 1-255 chars>
Request:
{
  "restaurant_id": "r_anker",
  "table_id": "t_2",
  "starts_at_local": "2026-09-24T19:00",
  "party_size": 4
}
```
- `starts_at_local` = wall-clock at restaurant, no offset, no Z
- `table_id` is CLIENT-CHOSEN (not auto-assigned)

Response 201:
```json
{
  "reservation_id": "res_7",
  "reference": "K3P7QW",
  "restaurant_id": "r_anker",
  "table_id": "t_2",
  "party_size": 4,
  "status": "confirmed",
  "starts_at_local": "2026-09-24T19:00",
  "starts_at": "2026-09-24T19:00:00+02:00",
  "ends_at": "2026-09-24T20:30:00+02:00",
  "created_at": "2026-09-21T11:04:03+00:00"
}
```

**Replay Behavior (Idempotency)**:
- First call: 201 with response above
- Replay (same key, same body): 200 with same JSON response
- Same key, different body: 409 idempotency_key_reuse
- Missing Idempotency-Key header: 400 missing_idempotency_key
- Empty key: 400 missing_idempotency_key
- Key length 1-255 chars, otherwise 422 validation_failed

**Error codes**:
- Overlapping confirmed reservation on table: 409 table_unavailable
- Not on slot grid: 422 not_on_slot_grid
- Outside opening hours / reservation ends after closes: 422 outside_opening_hours
- party_size > table capacity: 422 party_exceeds_capacity
- party_size < 1 or not integer: 422 validation_failed
- Invalid local time (spring forward skipped hour): 422 invalid_local_time
- Unknown restaurant/table: 404 not_found
- Table belongs to different restaurant: 404 not_found

### GET /reservations
```
Response 200:
{ "reservations": [ {...same as reservation response...} ] }
```
- Returns user's reservations (both confirmed and cancelled)
- Sorted by starts_at descending (most recent first)
- Empty list: { "reservations": [] }

### GET /reservations/{reference}
```
Response 200: { reservation object }
Response 404: { "error": { "code": "not_found", ... } } - MUST NOT LEAK existence of other user's reservations
```

### POST /reservations/{reference}/cancel
```
Response 200:
{
  "reference": "K3P7QW",
  "status": "cancelled",
  ...other fields...
}
```
- Already cancelled: 200 with current state (idempotent)
- Within cancellation_cutoff_minutes: 409 cutoff_passed
- Not user's reservation: 404 not_found

### PATCH /reservations/{reference}
```
Request body (any subset of fields):
{
  "table_id": "t_2",
  "starts_at_local": "2026-09-24T20:00",
  "party_size": 4
}
```
- Reference and reservation_id survive change
- Unknown fields ignored
- Validation same as POST /reservations
- Cutoff rule: measured against CURRENT start time → 409 cutoff_passed if within cutoff
- Cancelled reservation: 409 reservation_cancelled
- Success: releases old slot, reserves new atomically
- Failed amendment: original unchanged

### POST /reservation-moves (Atomic Batch)
```
POST /reservation-moves
Authorization: Bearer <token>
Idempotency-Key: <string>

Request:
{
  "moves": [
    { "reference": "BOOK01", "table_id": "t_2" },
    { "reference": "BOOK02", "starts_at_local": "2026-09-24T20:00" }
  ]
}
```
- 1-8 moves, distinct references
- Each move accepts: table_id, starts_at_local, party_size (omitted = retain current)
- Unknown fields ignored

**Errors**:
- Invalid shape/duplicates: 422 validation_failed
- Unknown reference / not user's: 404 not_found
- References from different restaurants: 422 validation_failed
- Cancelled booking: 409 reservation_cancelled
- Cutoff passed (for that booking): 409 cutoff_passed (takes precedence)
- Overlap among resulting bookings or with unlisted: 409 table_unavailable

**Success**:
- Response 201: { "reservations": [...objects in input order, including unchanged...] }
- Either every move commits or nothing changes

**Replay**:
- Same key, same body → 200 with original response (even after changes)

## Test Endpoints

### POST /_test/reset
```
Request: { fixture object }
Response 204: No Content
```
Replaces ALL service state with fixture.

### GET /_test/export
```
Response 200:
{
  "track": "tablekeeper",
  "format_version": 1,
  "state": { ...implementation-defined JSON... }
}
```

### POST /_test/import
```
Request: { "track": "tablekeeper", "format_version": 1, "state": {...} }
Response 204: No Content
```
- Atomic replacement
- Accept unchanged export
- Invalid: 422 validation_failed

## Time Rules

### "Now"
- Uses real system clock (no fixture override or test header)

### Slot Grid
- Slots start at every `slot_minutes` from opening time
- Example: slot_minutes=30, opens=18:00 → 18:00, 18:30, 19:00...

### Cutoff
- cancellation_cutoff_minutes measured from starts_at
- Cannot cancel/amend within cutoff window

### DST Handling
- **Spring forward**: local times in skipped hour don't exist → 422 invalid_local_time
- **Fall back**: resolve to first occurrence (before clocks change)
- Duration is absolute time: 90-min reservation = 90 real minutes

### Closing Boundary
- Slot must satisfy: slot + reservation_duration_minutes <= closes
- Example: opens=18:00, closes=23:00, duration=90 → last slot at 21:30

## Fixture Schema (POST /_test/reset)

```json
{
  "users": [
    { "id": "u_ada", "email": "ada@example.com", "password": "correct horse", "display_name": "Ada" }
  ],
  "restaurants": [
    {
      "id": "r_anker",
      "name": "Zum Anker",
      "timezone": "Europe/Berlin",
      "slot_minutes": 30,
      "reservation_duration_minutes": 90,
      "cancellation_cutoff_minutes": 120,
      "opening_hours": [
        { "weekday": "thu", "opens": "18:00", "closes": "23:00" },
        { "weekday": "fri", "opens": "18:00", "closes": "23:30" }
      ],
      "tables": [
        { "id": "t_1", "label": "1", "capacity": 2 },
        { "id": "t_2", "label": "2", "capacity": 4 }
      ]
    }
  ],
  "reservations": [
    {
      "id": "res_1",
      "reference": "ABC123",
      "user_id": "u_ada",
      "restaurant_id": "r_anker",
      "table_id": "t_1",
      "party_size": 2,
      "status": "confirmed",
      "starts_at_local": "2026-09-25T19:00",
      "created_at": "2026-09-21T11:04:03+00:00"
    }
  ]
}
```

- weekday: mon, tue, wed, thu, fri, sat, sun
- opens/closes: HH:MM, 24-hour, closes > opens on same day (no midnight crossing)
- Seeded users must login with given password immediately
- Reservations may seed confirmed bookings with all fields

## Reference Generation
- 6-12 characters from A-Z0-9
- Unique across ALL reservations (database constraint)
- Generate with crypto/rand

## Health Endpoint
```
GET /health
Response 200: { "status": "ok" }
```
Must return 200 within 60s of container start.