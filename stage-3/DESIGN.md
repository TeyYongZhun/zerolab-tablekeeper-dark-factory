# Tablekeeper Stage 3 Design

## Architecture
Go 1.21+ with stdlib, SQLite with WAL, bcrypt

## New in Stage 3

### 1. Availability Explanations
- GET /availability with explain=true returns rule analysis
- Rules: capacity, no_overlap
- Every table appears once in fixture order

### 2. Reservation History
- New endpoint: GET /reservations/{reference}/history
- New endpoint: GET /reservations/{reference}/decision
- Track changes with seq, at, event, revision, accepted_terms

### 3. Booking Policies
- Restaurants have manager_user_ids
- POST /restaurants/{id}/policies to publish (requires idempotency)
- GET /restaurants/{id}/policies to list
- Policy selection by effective_from and policy_version

### 4. Reservation Terms
- All reservation responses include revision and accepted_terms
- PATCH accepts expected_revision for optimistic locking
- Cancel checks cutoff against current start

### 5. Recurring Reservations
- POST /series to create recurring bookings
- GET /series/{series_id} to view
- Exception handling for individual occurrences

### 6. Database Schema Additions
- policies table: restaurant_id, effective_from, slot_minutes, duration, cutoff, opening_hours, capacities, policy_version
- reservation_history: reference, seq, at, event, revision, accepted_terms JSON
- series table: series_id, anchor_reference, count, interval_weeks, revision
- series_occurrences: series_id, index, reference, exception

## Work Items
1. [ ] Update availability with explain parameter
2. [ ] Add history endpoint
3. [ ] Add decision endpoint
4. [ ] Add manager_user_ids to restaurants
5. [ ] Add policies endpoint (POST/GET)
6. [ ] Update reservation responses with revision/accepted_terms
7. [ ] Add expected_revision to PATCH
8. [ ] Add recurring reservations (POST/GET series)
9. [ ] Update combined table history
10. [ ] Test export/import compatibility
11. [ ] Test locally