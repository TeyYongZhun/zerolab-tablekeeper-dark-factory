# Tablekeeper Stage 2 Design

## Architecture

**Language**: Go 1.21+
**Framework**: Standard library (ServeMux)
**Database**: SQLite with WAL mode for concurrency
**Password hashing**: bcrypt

## Stage 2 New Features

### 1. Combined Tables
Restaurant fixture gains `combinable` field: array of pairs [[t1, t2], ...]
- Pair capacity = sum of individual capacities
- Only exactly 2 tables per pair (no 3+)
- Not transitive

### 2. Database Schema Changes
Add `combinable` to restaurants table:
```sql
ALTER TABLE restaurants ADD COLUMN combinable TEXT;
```
(Stored as JSON array)

### 3. API Changes

#### GET /availability
Returns `available_options`:
- Single tables (available_table_ids)
- Combined pairs with capacity >= party_size
- Order: singles first, then pairs in combinable order

#### POST /reservations
Accepts:
- `table_id` (string, single table - backward compatible)
- `table_ids` (array, one or two tables - NEW)

Validation:
- Cannot send both table_id and table_ids
- Max 2 tables in table_ids
- Pair must be in restaurant's combinable
- party_size <= combined capacity

#### POST /reservation-moves
Accepts `table_ids` per move

### 4. Browser UI
HTML routes: /, /signup, /login, /lookup

Data-testid attributes for all required elements.

Concurrent handling:
- 409 response → booking-error + refresh
- Lost response → booking-uncertain + retry with same key

### 5. Export/Import
- Accept stage-1 exports (no combinable field)
- Preserve tokens, references, idempotency keys
- Form state survives import

## Work Items

1. [ ] Update database for combinable field
2. [ ] Update GET /availability with available_options
3. [ ] Update POST /reservations to accept table_ids
4. [ ] Update PATCH /reservations
5. [ ] Update POST /reservation-moves
6. [ ] Add HTML templates for /, /signup, /login, /lookup
7. [ ] Implement UI concurrent handling (uncertain, error)
8. [ ] Update export/import for compatibility
9. [ ] Test locally
10. [ ] Build and verify