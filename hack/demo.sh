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

if [ "${E2E_PGVECTOR:-false}" = "true" ]; then
  echo "=== Deploying pgvector ==="
  kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: pg-init
data:
  init.sql: |
    CREATE EXTENSION IF NOT EXISTS vector;
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pgvector
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pgvector
  template:
    metadata:
      labels:
        app: pgvector
    spec:
      containers:
        - name: pgvector
          image: pgvector/pgvector:pg17
          env:
            - name: POSTGRES_PASSWORD
              value: kuberag
            - name: POSTGRES_DB
              value: kuberag
          ports:
            - containerPort: 5432
          volumeMounts:
            - name: init
              mountPath: /docker-entrypoint-initdb.d
      volumes:
        - name: init
          configMap:
            name: pg-init
---
apiVersion: v1
kind: Service
metadata:
  name: pgvector
spec:
  selector:
    app: pgvector
  ports:
    - port: 5432
EOF
  kubectl wait --for=condition=available deployment/pgvector --timeout=120s
  echo "pgvector deployed"

  echo "=== Creating pgvector KnowledgeBase ==="
  kubectl apply -f - <<'EOF'
apiVersion: rag.furkan.dev/v1alpha1
kind: KnowledgeBase
metadata:
  name: e2e-pgvector
  namespace: default
spec:
  sources:
    - name: readme
      type: github
      github:
        repo: sindresorhus/awesome
        ref: main
        includeGlobs:
          - "*.md"
  chunking:
    strategy: semantic
    maxTokens: 400
    overlap: 40
  embedding:
    model: bge-small
    provider: local
  vectorStore:
    type: pgvector
    endpoint: postgresql://postgres:kuberag@pgvector.default.svc.cluster.local:5432/kuberag
    collection: e2e-pgvector
    distance: cosine
  ingestion:
    mode: full
EOF
fi

kubectl apply -f "$ROOT/config/samples/e2e-test.yaml"

echo "==> waiting for ingestion to complete"
kubectl wait --for=jsonpath='{.status.phase}'=Ready kb/e2e-docs --timeout=300s

echo "==> verifying auto-tune recall"
RECALL=$(kubectl get kb e2e-docs -o jsonpath='{.status.evaluation.recallPercent}')
if [ -n "$RECALL" ] && [ "$RECALL" -gt 0 ]; then
  echo "PASS: auto-tune recall = $RECALL%"
else
  echo "WARNING: auto-tune recall not available (eval dataset may be empty)"
fi
ATTEMPTS=$(kubectl get kb e2e-docs -o jsonpath='{.status.autoTuneAttempts}')
echo "  auto-tune attempts: $ATTEMPTS"

kubectl get kb,vi,rtr

echo "==> querying the retriever"
kubectl rollout status deploy/e2e-docs-retriever --timeout=120s
kubectl port-forward svc/e2e-docs-retriever 8000:8000 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT
sleep 4
curl -s localhost:8000/query -H 'content-type: application/json' \
  -d '{"query":"machine learning resources","topK":3}' | (jq . 2>/dev/null || cat)

echo "=== Testing multi-source KnowledgeBase ==="
kubectl apply -f - <<'EOF'
apiVersion: rag.furkan.dev/v1alpha1
kind: KnowledgeBase
metadata:
  name: kuberag-multi
spec:
  sources:
    - name: github-docs
      type: github
      github:
        repo: kubernetes/website
        includeGlobs:
          - "content/en/docs/concepts/**/*.md"
    - name: web-docs
      type: web
      web:
        urls:
          - "https://kubernetes.io/docs/concepts/overview/"
  embedding:
    model: bge-small
    provider: local
  vectorStore:
    type: qdrant
    endpoint: http://qdrant:6333
EOF

for i in $(seq 1 60); do
  PHASE=$(kubectl get kb kuberag-multi -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
  if [ "$PHASE" = "Ready" ]; then
    SOURCES=$(kubectl get kb kuberag-multi -o jsonpath='{.status.sources[*].name}')
    echo "PASS: multi-source KB ready with sources: $SOURCES"
    break
  fi
  sleep 10
done

echo
echo "==> done. Clean up with:  k3d cluster delete $CLUSTER"
