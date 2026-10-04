# Tablekeeper Stage 2 - Full Specification

## Stage 1 Requirements Continue to Apply
All stage 1 functionality remains unchanged and must work.

## New in Stage 2

### 1. Browser UI Routes (HTML)
Return HTML for these routes, not JSON:
- `/` - Search and availability grid
- `/signup` - Signup form
- `/login` - Login form
- `/lookup` - Look up reservation by reference

### 2. Combined Tables
Restaurant fixture gains:
```json
{
  "id": "r_anker",
  "combinable": [["t_1", "t_2"], ["t_2", "t_3"]],
  ...
}
```
- Each entry is an unordered pair of table IDs
- Pairs only - never three or more
- Not transitive

### 3. API Changes

#### GET /availability
Returns new `available_options` field:
```json
{
  "slots": [
    {
      "starts_at_local": "2026-09-24T19:00",
      "starts_at": "2026-09-24T19:00:00+02:00",
      "available_table_ids": ["t_3"],
      "available_options": [
        { "table_ids": ["t_3"], "capacity": 4 },
        { "table_ids": ["t_1", "t_2"], "capacity": 6 }
      ]
    }
  ]
}
```
- `available_options`: every single table + every declared pair with capacity >= party_size and no overlap
- Singles first, then pairs in combinable order
- `table_ids` within pair in combinable order

#### POST /reservations
New body format accepts `table_ids` (array) instead of/alongside `table_id`:
```json
{ "restaurant_id": "r_anker", "table_ids": ["t_1", "t_2"],
  "starts_at_local": "2026-09-24T19:00", "party_size": 6 }
```
- `table_id` still works (single table)
- Sending both `table_id` and `table_ids` = 422 validation_failed
- Response includes both `table_ids` always, and `table_id` when single

**Error codes:**
- Pair not in combinable: 422 combination_not_allowed
- More than 2 tables: 422 combination_not_allowed
- Overlapping booking: 409 table_unavailable
- party_size > combined capacity: 422 party_exceeds_capacity
- Duplicate table in set: 422 validation_failed

#### PATCH /reservations/{reference}
Accepts `table_ids` same rules as POST.

#### POST /reservation-moves
Accepts `table_ids` per move, same validation as single-table booking.

### 4. Concurrent/Uncertain UI Handling

Required behaviors:
- If search A starts before B but finishes after B: show B's results
- 409 table_unavailable: show booking-error, refresh availability, preserve form inputs
- Lost response: show booking-uncertain (no error), retry with same idempotency key
- Successful retry: show original reference, remove uncertainty

### 5. Export/Import Compatibility
- Must accept stage-1 exports
- Browser sessions survive export/import
- References remain valid
- Retries with same key/body remain valid after import
- Form and pending retry must survive upgrade

### 6. data-testid Attributes

#### Auth
| data-testid | Element |
|-------------|---------|
| signup-email, signup-password, signup-display-name | Inputs |
| signup-submit | Button |
| login-email, login-password, login-submit | Inputs and button |
| auth-error | Error message |
| current-user | Display name when signed in |
| logout-button | Button |

#### Search/Availability
| data-testid | Element |
|-------------|---------|
| restaurant-select | Select with restaurant ids |
| date-input | YYYY-MM-DD |
| party-size-input | Number |
| search-button | Search trigger |
| availability-grid | Results container |
| slot-{table_id}-{HH:MM} | Single table cell |
| slot-{t_a}+{t_b}-{HH:MM} | Combined table cell |
| no-slots | When day has no slots |

Cell has data-available="true" or "false".

#### Booking
| data-testid | Element |
|-------------|---------|
| booking-form | Container |
| booking-summary | Table + time text |
| booking-party-size | Number input |
| booking-submit | Button |
| booking-error | Error message |
| booking-uncertain | Uncertainty message |

#### Confirmation
| data-testid | Element |
|-------------|---------|
| confirmation | Container |
| confirmation-reference | Reference only |
| confirmation-details | Restaurant, table, time |
| confirmation-tables | Table labels |

#### Lookup
| data-testid | Element |
|-------------|---------|
| lookup-reference-input | Input |
| lookup-submit | Button |
| reservation-detail | Container |
| reservation-status | confirmed/cancelled |
| reservation-cancel-button | Cancel button |
| reservation-error | Error message |
| reservation-tables | Table labels |

## Design Decisions

### Stack
- Continue with Go + SQLite
- Embed HTML templates in binary or serve from /static
- Use Go 1.22+ stdlib ServeMux

### UI Framework
- Server-side rendered HTML with embedded CSS/JS
- No external framework dependencies

### Implementation Order
1. Update database schema for combinable tables
2. Update API: GET /availability with available_options
3. Update API: POST /reservations with table_ids
4. Update API: PATCH and reservation-moves
5. Add HTML routes for /, /signup, /login, /lookup
6. Implement UI concurrent handling
7. Update export/import for compatibility
8. Test locally
9. Build and verify