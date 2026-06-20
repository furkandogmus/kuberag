#!/usr/bin/env bash
set -euo pipefail

# kuberag retriever load benchmark
# Requires: kubectl, curl, jq
# Usage: ./hack/benchmark.sh [namespace] [retriever-name] [concurrency] [duration_sec]

NAMESPACE="${1:-default}"
RETRIEVER="${2:-kuberag-retriever}"
CONCURRENCY="${3:-4}"
DURATION="${4:-30}"

echo "=== kuberag Benchmark ==="
echo "Target: $RETRIEVER.$NAMESPACE"
echo "Concurrency: $CONCURRENCY, Duration: ${DURATION}s"

# Port-forward
kubectl port-forward -n "$NAMESPACE" "svc/$RETRIEVER" 8080:8080 &
PF_PID=$!
sleep 3
trap "kill $PF_PID 2>/dev/null" EXIT

BASE="http://localhost:8080"

# Warmup
echo "=== Warmup ==="
curl -s "$BASE/healthz" || { echo "FAIL: retriever not reachable"; exit 1; }

# Benchmark using parallel curl
echo "=== Benchmark ==="
start=$(date +%s)
success=0
fail=0
latencies=()

run_query() {
  local query="What is ${RANDOM}?"
  local t1=$(python3 -c "import time; print(int(time.time()*1000))")
  local code=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$BASE/query" \
    -H "Content-Type: application/json" \
    -d "{\"query\":\"$query\",\"topK\":5}" 2>/dev/null || echo "000")
  local t2=$(python3 -c "import time; print(int(time.time()*1000))")
  local lat=$((t2 - t1))
  echo "$lat $code"
}

export -f run_query
export BASE DURATION

end=$((start + DURATION))
while [ "$(date +%s)" -lt "$end" ]; do
  for i in $(seq 1 $CONCURRENCY); do
    run_query &
  done
  wait
  sleep 0.5
done

# Stats via /metrics
echo "=== Metrics Snapshot ==="
curl -s "$BASE/metrics" 2>/dev/null | grep -E "rag_" | head -10 || echo "(no metrics endpoint)"

echo ""
echo "=== Benchmark Complete ==="
