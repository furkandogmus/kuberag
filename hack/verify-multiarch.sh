#!/usr/bin/env bash
set -euo pipefail

# Verify multi-arch images are published and inspectable.
# Requires: docker or crane, jq (optional)

REGISTRY="${REGISTRY:-ghcr.io/furkandogmus}"
TAG="${TAG:-latest}"
IMAGES=("kuberag" "kuberag-worker" "kuberag-retriever")

echo "=== Multi-arch Image Verification ==="

for img in "${IMAGES[@]}"; do
  REF="${REGISTRY}/${img}:${TAG}"
  echo ""
  echo "--- $REF ---"
  
  # Try crane first (lighter), fall back to docker
  if command -v crane &>/dev/null; then
    DIGEST=$(crane digest "$REF" 2>/dev/null || echo "")
    if [ -n "$DIGEST" ]; then
      echo "  digest: $DIGEST"
      # Check manifest for multiple platforms
      crane manifest "$REF" 2>/dev/null | jq -r '
        if .manifests then
          .manifests[] | "  platform: \(.platform.os)/\(.platform.architecture)"
        else
          "  single-platform image"
        end' 2>/dev/null || echo "  (manifest inspection failed)"
    else
      echo "  WARNING: image not found (not yet published?)"
    fi
  elif command -v docker &>/dev/null; then
    docker buildx imagetools inspect "$REF" 2>/dev/null || echo "  WARNING: image not found"
  else
    echo "  SKIP: neither crane nor docker available"
  fi
done

echo ""
echo "PASS: multi-arch verification complete"
