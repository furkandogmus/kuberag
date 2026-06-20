#!/usr/bin/env bash
set -euo pipefail

# kuberag Helm upgrade test
# Simulates upgrading from one chart version to another.

CHART_DIR="$(cd "$(dirname "$0")/../deploy/helm/kuberag" && pwd)"

echo "=== Helm lint ==="
helm lint "$CHART_DIR"

echo "=== Render v1 install ==="
helm template kuberag-test "$CHART_DIR" \
  --namespace kuberag-system \
  --set rbac.scope=cluster \
  > /tmp/kuberag-v1.yaml

echo "=== Render upgrade (same values) ==="
helm template kuberag-test "$CHART_DIR" \
  --namespace kuberag-system \
  --set rbac.scope=cluster \
  --set replicaCount=1 \
  > /tmp/kuberag-v2.yaml

echo "=== Verify CRDs present ==="
for crd in knowledgebases retrievers vectorindices ingestionruns backups restores; do
  if ! grep -q "name: ${crd}.rag.furkan.dev" "$CHART_DIR"/crds/rag.furkan.dev_${crd}.yaml; then
    echo "ERROR: CRD ${crd} not found or missing name annotation"
    exit 1
  fi
done

echo "=== Verify RBAC includes backup/restore ==="
for resource in backups restores; do
  if ! grep -q "$resource" /tmp/kuberag-v1.yaml; then
    echo "ERROR: RBAC missing $resource"
    exit 1
  fi
done

echo "=== Render namespace-scoped mode ==="
helm template kuberag-test "$CHART_DIR" \
  --namespace tenant-a \
  --set rbac.scope=namespace \
  --set rbac.watchNamespace=tenant-a \
  > /tmp/kuberag-ns.yaml

echo "=== Verify namespace-scoped has no ClusterRole ==="
if grep -q "kind: ClusterRole" /tmp/kuberag-ns.yaml; then
  echo "ERROR: namespace-scoped render should not contain ClusterRole"
  exit 1
fi

echo "=== Verify namespace-scoped has Role instead ==="
if ! grep -q "kind: Role" /tmp/kuberag-ns.yaml; then
  echo "ERROR: namespace-scoped render should contain Role"
  exit 1
fi

echo ""
echo "PASS: Helm upgrade test passed"
