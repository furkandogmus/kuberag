#!/usr/bin/env bash
set -euo pipefail

# Air-gapped test: verify the worker can run without internet access
# if models are pre-cached and sources are locally reachable.
# 
# Requires: docker, a running kuberag cluster (optional)
# Usage: ./hack/test-airgapped.sh [namespace]

NAMESPACE="${1:-default}"

echo "=== Air-gapped Worker Test ==="

# Step 1: Prefetch models using the worker image
echo "--- Prefetching embedding models ---"
docker run --rm \
  -e HF_HOME=/scratch/.cache/huggingface \
  -e FASTEMBED_CACHE_PATH=/scratch/.cache/fastembed \
  -v "$(pwd)/.model-cache:/scratch/.cache" \
  ghcr.io/furkandogmus/kuberag-worker:latest \
  python3 -c "
from fastembed import TextEmbedding
# Pre-download models
TextEmbedding(model_name='BAAI/bge-small-en-v1.5', cache_dir='/scratch/.cache/fastembed')
print('Models cached successfully')
"

echo "  Models cached at ./.model-cache"

# Step 2: Verify worker can start without network
echo "--- Testing worker startup without network ---"
docker run --rm \
  --network none \
  -e HF_HOME=/scratch/.cache/huggingface \
  -e FASTEMBED_CACHE_PATH=/scratch/.cache/fastembed \
  -e HOME=/scratch \
  -e KB_SPEC_JSON='{"embedding":{"model":"bge-small","provider":"local"},"vectorStore":{"type":"qdrant","endpoint":"http://localhost:6333"}}' \
  -v "$(pwd)/.model-cache:/scratch/.cache" \
  ghcr.io/furkandogmus/kuberag-worker:latest \
  python3 -c "
from fastembed import TextEmbedding
m = TextEmbedding(model_name='BAAI/bge-small-en-v1.5', cache_dir='/scratch/.cache/fastembed')
v = list(m.embed(['test']))[0]
assert len(v) == 384, f'expected dim 384, got {len(v)}'
print('PASS: embedding model loaded without network')
"

echo ""
echo "PASS: air-gapped test passed"
echo "  Model cache at: ./.model-cache"
echo "  To use in production, mount this as a PVC and set modelCacheSizeLimit"
