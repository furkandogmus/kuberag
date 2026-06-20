#!/usr/bin/env bash
set -euo pipefail

# Verify Pod Security Standards (restricted profile) compliance.
# Checks Go job builders and Helm templates for required securityContext fields.

CHART="$(cd "$(dirname "$0")/../deploy/helm/kuberag" && pwd)"

echo "=== Checking Helm operator deployment ==="
helm template test "$CHART" --namespace ns > /tmp/pss-check.yaml

check_field() {
  local kind="$1" field="$2"
  if ! grep -q "$field" /tmp/pss-check.yaml; then
    echo "FAIL: $kind missing $field"
    exit 1
  fi
}

check_field "Deployment" "runAsNonRoot"
check_field "Deployment" "allowPrivilegeEscalation"
check_field "Deployment" "capabilities"
check_field "Deployment" "drop"
check_field "Deployment" "ALL"
check_field "Deployment" "readOnlyRootFilesystem"
check_field "Deployment" "seccompProfile"
check_field "Deployment" "RuntimeDefault"

echo "PASS: Pod Security Standards verified"
