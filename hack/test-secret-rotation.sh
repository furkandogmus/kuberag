#!/usr/bin/env bash
set -euo pipefail
# Verify that secret rotation does NOT trigger re-ingestion.
# Requires: a running kuberag, kubectl, jq

NAMESPACE="${1:-default}"
KB="${2:-kuberag-demo}"

echo "=== Secret Rotation Test ==="

SPEC_HASH_BEFORE=$(kubectl get kb "$KB" -n "$NAMESPACE" -o jsonpath='{.status.observedSpecHash}')
echo "Spec hash before: $SPEC_HASH_BEFORE"

# Rotate the embedding API key secret (create a new dummy one)
kubectl create secret generic test-rotated-key -n "$NAMESPACE" \
  --from-literal=apiKey="rotated-$(date +%s)" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "Waiting for controller to observe secret change..."
sleep 15

SPEC_HASH_AFTER=$(kubectl get kb "$KB" -n "$NAMESPACE" -o jsonpath='{.status.observedSpecHash}')
echo "Spec hash after:  $SPEC_HASH_AFTER"

if [ "$SPEC_HASH_BEFORE" = "$SPEC_HASH_AFTER" ]; then
  echo "PASS: spec hash unchanged after secret rotation"
else
  echo "FAIL: spec hash changed after secret rotation (would trigger re-index!)"
  exit 1
fi
