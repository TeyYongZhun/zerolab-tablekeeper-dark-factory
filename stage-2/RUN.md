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
