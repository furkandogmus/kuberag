# Contributing to kuberag

Thanks for your interest! kuberag is an early-stage (`v1alpha1`) project; issues,
ideas, and PRs are welcome.

## Prerequisites

- Go (see `go.mod` for the version), Python 3.12
- Docker, `kubectl`
- A cluster for manual testing — [k3d](https://k3d.io) or kind works well
- `make`

## Layout

| Path | What |
|------|------|
| `api/v1alpha1/` | CRD Go types (+ generated DeepCopy) |
| `internal/controller/` | reconcilers, job/scheduling/metrics helpers, unit + envtest tests |
| `cmd/main.go` | manager entrypoint |
| `worker/rag_worker/` | Python data plane (sources, stores, chunking, embeddings, ingest/eval/cleanup) |
| `worker/retriever/` | FastAPI retrieval + generation server |
| `config/` | generated CRDs/RBAC, manager manifest, runnable samples |

The API group is `rag.furkan.dev` and is intentionally independent of the repo name.

## Common tasks

```bash
make build              # generate + fmt + vet + build the manager
make test               # Go unit tests
make test-integration   # envtest integration tests (downloads kube-apiserver/etcd)
make test-py            # Python worker tests
make manifests generate # regenerate CRDs/RBAC + DeepCopy after changing api/ types
```

CI mirrors these and additionally fails if generated artifacts are stale, so run
`make manifests generate` and commit the result whenever you touch `api/` types
or kubebuilder `+rbac` markers.

## Local end-to-end

```bash
k3d cluster create kuberag-dev
make install                 # CRDs
make run &                   # operator against your kubeconfig (or `make deploy`)
make sample                  # Qdrant + worker RBAC + example KnowledgeBase + Retriever
kubectl get kb,vi,rtr -w
```

For a scripted demo see [`hack/demo.sh`](hack/demo.sh).

## Conventions

- Keep the control plane (Go) and data plane (Python) responsibilities separated:
  the operator decides *when*; workers do the *work*.
- Match surrounding code style; `gofmt` must be clean.
- Add/extend tests with behavior changes (unit for pure logic, envtest for reconcile flows).
