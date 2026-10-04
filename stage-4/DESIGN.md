# Tablekeeper Stage 4 Design

## Architecture
Go 1.21+ with stdlib, SQLite with WAL, bcrypt

## New in Stage 4

### 1. Seating Replans
- POST /restaurants/{id}/replans - create seating plan
- POST /restaurants/{id}/replans/{plan_id}/apply - apply plan
- New closures table tracks applied closures
- Plans stored temporarily until applied or invalidated
- Restaurant revision increments on each successful plan apply

### 2. Series Amendments
- POST /series/{series_id}/amend - bulk amend recurring reservations
- Amend from specific index onward
- Check cutoff, validate against new date's policy

### 3. Database Schema Additions
- closures table: restaurant_id, table_id, from, to, plan_id
- plans table: plan_id, restaurant_id, closure JSON, assignments JSON, restaurant_revision, applied
- series revision tracking for amendments

## Work Items
1. [ ] Add replans endpoint
2. [ ] Add apply plan endpoint
3. [ ] Track closures in availability
4. [ ] Add series amend endpoint
5. [ ] Update history for reassignments
6. [ ] Handle export/import compatibility
7. [ ] Build and test