#!/usr/bin/env bash
# Populate a running Tablekeeper with demo data for screenshots and the video.
# Usage:  bash assets/seed-demo.sh [base-url]      (default http://localhost:8080)
#
# Uses the service's own POST /_test/reset fixture endpoint — the same hook the
# event harness uses. No application code is touched.
set -u
BASE="${1:-http://localhost:8080}"
DIR="$(cd "$(dirname "$0")" && pwd)"

# Strip CRLF and any UTF-8 BOM: the fixture is edited on Windows and Go's
# json.Unmarshal rejects a BOM before the opening brace.
tr -d '\r' < "$DIR/demo-seed.json" | sed '1s/^\xEF\xBB\xBF//' > /tmp/tk-seed.json

code=$(curl -s -o /tmp/tk-seed-out.txt -w '%{http_code}' \
  -X POST "$BASE/_test/reset" \
  -H 'Content-Type: application/json' \
  --data-binary @/tmp/tk-seed.json)

if [ "$code" != "204" ]; then
  echo "reset failed: HTTP $code"
  cat /tmp/tk-seed-out.txt
  exit 1
fi

echo "seeded $BASE"
curl -s "$BASE/restaurants"; echo
echo
echo "  login:      tey@zerolab.dev / zerolab123"
echo "  reference:  AURORA01"
echo "  try:        Aurora Bistro, 2026-10-08, party size 2"
