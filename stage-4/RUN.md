# Tablekeeper stage 1 — build and run

From a clean checkout:

```bash
cd stage-1
docker build -t tablekeeper .
docker run --rm -e PORT=8080 -p 8080:8080 tablekeeper
```

The build downloads all Go dependencies, runs `go vet` and the unit tests, and
compiles a static binary (pure-Go SQLite, no CGO). The running container needs
no network access. It listens on `$PORT` (default 8080); state lives in a
SQLite (WAL) file under `/tmp` that is recreated at every start.

Check it:

```bash
curl http://localhost:8080/health
```

Run the unit tests without Docker (needs Go 1.22+): `go test ./...`

## Stage 2 additions

- Browser UI (server-rendered shells, inline JS, no external assets): `/`, `/signup`, `/login`, `/lookup`.
- Combined tables: restaurant fixtures accept `combinable` pairs; bookings accept `table_ids`.
- `go test ./...` also runs the stage-2 tests (combined tables, stage-1 export import, UI routes).

## Stage 3 additions

- `GET /availability?...&explain=true`, reservation `revision` / `accepted_terms`, `GET /reservations/{ref}/history` and `/decision`.
- Restaurant `manager_user_ids`, `POST|GET /restaurants/{id}/policies` (policy 0 is the base configuration).
- `PATCH` accepts `expected_revision`; `POST /series`, `GET /series/{id}`.
- Exports now carry policies, history and series; stage-1 and stage-2 exports import (history is synthesised for them).

## Stage 4 additions

- Seating replans: `POST /restaurants/{id}/replans` (alias `/replots`) and `.../{plan_id}/apply`; applied closures remove tables from availability and booking.
- `POST /series/{id}/amend` shifts the clock time of later occurrences.
- Restaurant revision counts every booking, policy or seating change; plans are only valid at the revision they were made.
