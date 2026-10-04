# Tablekeeper Stage 1 Design

## Architecture

**Language**: Go 1.21+
**Framework**: Standard library + gorilla/mux for routing
**Database**: SQLite with WAL mode for concurrency
**Password hashing**: bcrypt

### Why Go + SQLite
- Native concurrency support for handling 50 simultaneous requests
- Single static binary, no runtime dependencies
- SQLite with WAL mode handles concurrent reads/writes safely
- Industry-proven for HTTP services

## Data Model

```sql
-- Users table
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL
);

-- Restaurants table
CREATE TABLE restaurants (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    timezone TEXT NOT NULL,
    slot_minutes INTEGER NOT NULL,
    reservation_duration_minutes INTEGER NOT NULL,
    cancellation_cutoff_minutes INTEGER NOT NULL
);

-- Opening hours table
CREATE TABLE opening_hours (
    id INTEGER PRIMARY KEY,
    restaurant_id TEXT NOT NULL,
    weekday TEXT NOT NULL,
    opens TEXT NOT NULL,
    closes TEXT NOT NULL,
    FOREIGN KEY (restaurant_id) REFERENCES restaurants(id)
);

-- Tables table
CREATE TABLE tables (
    id TEXT PRIMARY KEY,
    restaurant_id TEXT NOT NULL,
    label TEXT NOT NULL,
    capacity INTEGER NOT NULL,
    FOREIGN KEY (restaurant_id) REFERENCES restaurants(id)
);

-- Reservations table
CREATE TABLE reservations (
    id TEXT PRIMARY KEY,
    reference TEXT UNIQUE NOT NULL,
    user_id TEXT NOT NULL,
    restaurant_id TEXT NOT NULL,
    table_id TEXT NOT NULL,
    party_size INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'confirmed',
    starts_at TEXT NOT NULL,
    ends_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id),
    FOREIGN KEY (restaurant_id) REFERENCES restaurants(id),
    FOREIGN KEY (table_id) REFERENCES tables(id)
);

-- Index for overlapping reservation check
CREATE INDEX idx_reservations_table_time ON reservations(table_id, starts_at, ends_at);

-- Tokens table
CREATE TABLE tokens (
    token TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id)
);

-- Idempotency keys table
CREATE TABLE idempotency_keys (
    key TEXT NOT NULL,
    user_id TEXT NOT NULL,
    request_path TEXT NOT NULL,
    request_body_hash TEXT NOT NULL,
    response_body TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (key, user_id),
    FOREIGN KEY (user_id) REFERENCES users(id)
);
```

## Concurrency Model

### Preventing Double Bookings
1. Use SQLite transactions with `BEGIN IMMEDIATE` to acquire write lock early
2. For availability check + book: single transaction
3. For overlapping check: query existing reservations where:
   - `table_id = ? AND status = 'confirmed'`
   - `NOT (ends_at <= new_starts_at OR starts_at >= new_ends_at)`

### Idempotency
1. On POST with Idempotency-Key, check if key exists for user
2. If exists with same body hash: return stored response (200)
3. If exists with different body: return 409 idempotency_key_reuse
4. If new: process request, store key+response on success
5. For concurrent identical requests: database unique constraint ensures serialized

## Timezone Handling

### DST Rules (Section 9)
- Use `github.com/tkuchiki/go-timezone` or standard `time.LoadLocation()`
- Spring forward: reject times in skipped hour (422 invalid_local_time)
- Fall back: resolve to first occurrence (use time before DST transition)
- Duration is absolute: 90 minutes = 90 minutes real time

### Slot Generation
1. Parse opens/closes as local times
2. Generate slots at `slot_minutes` intervals from opens
3. Filter slots where `slot + reservation_duration <= closes`
4. For each slot, check if any confirmed reservation overlaps

## API Endpoints

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| GET | /health | No | |
| POST | /_test/reset | No | Seed data |
| GET | /_test/export | No | State export |
| POST | /_test/import | No | State import |
| POST | /auth/signup | No | |
| POST | /auth/login | No | |
| GET | /restaurants | No | List all |
| GET | /restaurants/{id} | No | Detail + tables |
| GET | /availability | No | Query params required |
| POST | /reservations | Yes | Idempotency key required |
| GET | /reservations | Yes | User's reservations |
| GET | /reservations/{reference} | Yes | Own reservation only |
| POST | /reservations/{reference}/cancel | Yes | |
| PATCH | /reservations/{reference} | Yes | Amend |
| POST | /reservation-moves | Yes | Atomic batch move |

## Error Codes

| Status | Code | When |
|--------|------|------|
| 400 | malformed_request | Unparseable body |
| 400 | missing_idempotency_key | Header absent/empty |
| 401 | unauthenticated | Invalid/missing token |
| 403 | forbidden | Auth but not owner |
| 404 | not_found | Unknown resource |
| 409 | idempotency_key_reuse | Same key, different body |
| 409 | table_unavailable | Overlapping booking |
| 409 | cutoff_passed | Within cancellation window |
| 409 | reservation_cancelled | On cancelled reservation |
| 422 | validation_failed | Invalid field value |
| 422 | not_on_slot_grid | Not on slot boundary |
| 422 | outside_opening_hours | Outside restaurant hours |
| 422 | party_exceeds_capacity | Party > table capacity |
| 422 | invalid_local_time | Skipped DST hour |

## Reference Generation

- 6-12 characters from A-Z0-9
- Use cryptographically secure random
- Must be unique (database constraint)

## Acceptance Criteria

1. Service starts and responds to /health within 60s
2. POST /_test/reset seeds data correctly
3. Authentication flow works (signup/login)
4. Public endpoints return data without auth
5. Protected endpoints reject unauthenticated requests
6. Reservations prevent double-booking under concurrency
7. Idempotency prevents duplicate reservations
8. Cancellation releases table immediately
9. Amendments validate correctly
10. DST transitions handled per spec
11. Export/import preserves all state
12. Atomic reservation moves work as specified

## Work Items

1. [ ] Initialize Go project with dependencies
2. [ ] Set up SQLite schema and connection
3. [ ] Implement health endpoint
4. [ ] Implement reset/seed endpoint
5. [ ] Implement auth (signup/login)
6. [ ] Implement tokens
7. [ ] Implement GET /restaurants
8. [ ] Implement GET /restaurants/{id}
9. [ ] Implement GET /availability
10. [ ] Implement POST /reservations with idempotency
11. [ ] Implement GET /reservations
12. [ ] Implement GET /reservations/{reference}
13. [ ] Implement cancel endpoint
14. [ ] Implement PATCH endpoint
15. [ ] Implement reservation-moves
16. [ ] Implement export/import
17. [ ] Write Dockerfile
18. [ ] Write RUN.md
19. [ ] Test locally