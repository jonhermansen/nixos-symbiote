#!/usr/bin/env bash
set -euo pipefail

failures=0
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1"; failures=$((failures + 1)); }

echo "=== nixos ==="
[ -L /run/current-system ] && pass "/run/current-system exists" || fail "no /run/current-system"
nixos-version 2>/dev/null && pass "nixos-version works" || fail "nixos-version missing"

echo "=== nginx ==="
body=$(curl -sf http://localhost/)
[ "$body" = "hello from nixos" ] && pass "nginx serves response" || fail "nginx response: $body"

echo "=== postgresql ==="
psql -U runner -d runner -c "CREATE TABLE IF NOT EXISTS messages (id serial PRIMARY KEY, body text);"
psql -U runner -d runner -c "INSERT INTO messages (body) VALUES ('hello from the pipeline');"
result=$(psql -U runner -d runner -t -A -c "SELECT body FROM messages LIMIT 1;")
[ "$result" = "hello from the pipeline" ] && pass "postgres round-trip" || fail "postgres returned: $result"

echo "=== kafka ==="
kafka-topics.sh --bootstrap-server localhost:9092 --create --topic test-topic --partitions 1 --replication-factor 1
echo "hello from kafka" | kafka-console-producer.sh --bootstrap-server localhost:9092 --topic test-topic
result=$(timeout 10 kafka-console-consumer.sh --bootstrap-server localhost:9092 --topic test-topic --from-beginning --max-messages 1)
[ "$result" = "hello from kafka" ] && pass "kafka round-trip" || fail "kafka returned: $result"

echo "=== docker ==="
docker info >/dev/null 2>&1 && pass "docker daemon running" || fail "docker not available"

echo "=== k3s ==="
echo "Waiting for k3s to be ready..."
for i in $(seq 1 60); do
  if kubectl get nodes --no-headers 2>/dev/null | grep -q " Ready"; then
    break
  fi
  sleep 2
done
kubectl get nodes && pass "k3s node ready" || fail "k3s node not ready"
kubectl get pods -A && pass "k3s pods listing" || fail "k3s pods failed"

echo ""
if [ "$failures" -eq 0 ]; then
  echo "All checks passed."
else
  echo "$failures check(s) failed."
  exit 1
fi
