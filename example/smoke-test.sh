#!/usr/bin/env bash
set -euo pipefail

failures=0
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1"; failures=$((failures + 1)); }

echo "=== nginx ==="
body=$(curl -sf http://localhost/)
[ "$body" = "hello from nixos" ] && pass "nginx serves response" || fail "nginx response: $body"

echo "=== postgresql ==="
psql -U runner -d runner -c "CREATE TABLE IF NOT EXISTS messages (id serial PRIMARY KEY, body text);"
psql -U runner -d runner -c "INSERT INTO messages (body) VALUES ('hello from the pipeline');"
result=$(psql -U runner -d runner -t -A -c "SELECT body FROM messages LIMIT 1;")
[ "$result" = "hello from the pipeline" ] && pass "postgres round-trip" || fail "postgres returned: $result"

echo "=== kafka ==="
echo "kafka-test-message" | kafka-console-producer.sh \
  --broker-list localhost:9092 \
  --topic test-topic 2>/dev/null

msg=$(timeout 10 kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 \
  --topic test-topic \
  --from-beginning \
  --max-messages 1 2>/dev/null)
[ "$msg" = "kafka-test-message" ] && pass "kafka round-trip" || fail "kafka returned: $msg"

echo ""
if [ "$failures" -eq 0 ]; then
  echo "All checks passed."
else
  echo "$failures check(s) failed."
  exit 1
fi
