#!/usr/bin/env bash
# End-to-end demo: spin up a throwaway k3d cluster, deploy kuberag from the
# published GHCR images, ingest a small GitHub repo into Qdrant, and query it.
#
# Usage:  ./hack/demo.sh
# Cleanup: k3d cluster delete kuberag-demo
set -euo pipefail

# Ensure required CLI dependencies are installed
type k3d >/dev/null 2>&1 || { echo "ERROR: k3d is required but not installed. Aborting." >&2; exit 1; }
type jq >/dev/null 2>&1 || { echo "ERROR: jq is required but not installed. Aborting." >&2; exit 1; }

CLUSTER="${CLUSTER:-kuberag-demo}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> creating k3d cluster '$CLUSTER'"
k3d cluster create "$CLUSTER" --wait

echo "==> installing CRDs, RBAC, and the operator (from GHCR images)"
kubectl apply -f "$ROOT/config/crd"
kubectl apply -f "$ROOT/config/rbac/role.yaml"
kubectl apply -f "$ROOT/config/rbac/priority-class.yaml"
kubectl apply -f "$ROOT/config/manager/manager.yaml"
kubectl apply -f "$ROOT/config/rbac/leader_election_role.yaml"
kubectl -n kuberag-system rollout status deploy/kuberag-controller-manager --timeout=120s

echo "==> deploying Qdrant + worker RBAC + a small KnowledgeBase + Retriever"
kubectl apply -f "$ROOT/config/rbac/worker_rbac.yaml" -n default
kubectl apply -f "$ROOT/config/samples/qdrant.yaml"
kubectl apply -f "$ROOT/config/samples/e2e-test.yaml"

echo "==> waiting for ingestion to complete"
kubectl wait --for=jsonpath='{.status.phase}'=Ready kb/e2e-docs --timeout=300s
kubectl get kb,vi,rtr

echo "==> querying the retriever"
kubectl rollout status deploy/e2e-docs-retriever --timeout=120s
kubectl port-forward svc/e2e-docs-retriever 8000:8000 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT
sleep 4
curl -s localhost:8000/query -H 'content-type: application/json' \
  -d '{"query":"machine learning resources","topK":3}' | (jq . 2>/dev/null || cat)

echo
echo "==> done. Clean up with:  k3d cluster delete $CLUSTER"
