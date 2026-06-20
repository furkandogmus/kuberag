# Contributing to kuberag

Thanks for your interest! kuberag is an early-stage (`v1alpha1`) project; issues,
ideas, and PRs are welcome.

## Prerequisites

- **Go** — see `go.mod` for the exact version (pinned by `go run`).
- **Python 3.12** — for the worker/retriever data plane and tests.
- **Docker** — for building images and running the local kind/k3d cluster.
- **`kubectl`** — for manual testing.
- **`make`** — wraps the common build/test/lint commands.
- **`controller-gen`** and **`golangci-lint`** — installed on demand via
  `go run` by the Makefile; no global install needed.

## Layout

| Path | What |
|------|------|
| `api/v1alpha1/` | CRD Go types + generated DeepCopy |
| `internal/controller/` | reconcilers, job/scheduling/metrics helpers, unit + envtest tests |
| `cmd/main.go` | manager entrypoint |
| `worker/rag_worker/` | Python data plane: sources, stores, chunking, embeddings, ingest/eval/cleanup |
| `worker/retriever/` | FastAPI retrieval + generation server |
| `worker/tests/` | Python unit + real-store integration tests |
| `config/` | generated CRDs/RBAC, manager manifest, runnable samples |
| `deploy/helm/kuberag/` | Helm chart (mirrors `config/`) |
| `hack/` | demo and e2e scripts |
| `docs/` | ARCHITECTURE, API, PROVIDERS, TUNING, OBSERVABILITY, ROADMAP, CHANGELOG, RUNBOOK |

The API group is `rag.furkan.dev` and is intentionally independent of the repo
name. The `kuberag` binary, image, and Helm chart names mirror each other;
`rag.furkan.dev` survives a future repo move.

## Code style

### Go

- `gofmt` (enforced in CI; pre-commit hook recommended).
- `go vet ./...` (enforced in CI).
- `golangci-lint run ./...` with the ruleset in `.golangci.yml` (enforced in CI).
- Imports grouped: stdlib / third-party / internal (`github.com/furkandogmus/...`).
- Error wrapping with `%w`: `fmt.Errorf("doing X: %w", err)`.
- No init-side-effects; dependency wiring happens in `SetupWithManager` or the
  constructor.
- For CRD types: every new field needs `// +optional` (or whatever kubebuilder
  requires), an inline godoc, and a regenerable comment that controller-gen
  emits into the CRD manifest. CI fails if `make manifests generate` produces
  a diff.
- Prefer pure functions and put them in their own file (see
  `internal/controller/decision_helpers.go`) so they're unit-testable without
  spinning up envtest.

### Python

- `pytest` style discovery (test files are also `unittest`-discoverable).
- One module per concern (`sources.py`, `stores.py`, `chunking.py`,
  `embeddings.py`, `ingest.py`, `evaluate.py`, `cleanup.py`).
- Lazy imports of optional heavy deps (`qdrant_client`, `psycopg`,
  `pymilvus`) inside the class body or `__init__` so the worker image
  can omit the ones the user doesn't need.
- No docstring style mandate; just stay consistent within a file. Public
  functions get a one-line summary at minimum.
- f-strings everywhere. No `%` or `.format()`.
- Compose dynamic SQL with `psycopg.sql.Identifier` / `psycopg.sql.SQL`;
  f-string table names are a SQL-injection vector even when
  "sanitized".

### YAML

- Two-space indent.
- `kubectl apply --dry-run=client -f` before committing CRD/sample
  changes; it catches syntax errors that linters miss.
- Helm templates: use `_helpers.tpl` for label/annotation helpers;
  never duplicate `app.kubernetes.io/managed-by: kuberag`.

## Common tasks

```bash
make build              # generate + fmt + vet + build the manager
make test               # Go unit tests
make test-integration   # envtest integration tests (downloads kube-apiserver/etcd)
make test-py            # Python worker tests (mocked)
make manifests generate # regenerate CRDs/RBAC + DeepCopy after changing api/ types
make lint               # golangci-lint (via `go run`; no global install)
```

CI mirrors these and additionally fails if generated artifacts are stale, so
run `make manifests generate` and commit the result whenever you touch `api/`
types or kubebuilder `+rbac` markers.

### Running the real-store integration tests locally

The integration tests need a running Qdrant and pgvector:

```bash
docker run -d --rm -p 6333:6333 qdrant/qdrant
docker run -d --rm -p 5432:5432 -e POSTGRES_PASSWORD=postgres pgvector/pgvector:pg17

cd worker
QDRANT_ENDPOINT=http://localhost:6333 \
PGVECTOR_DSN=postgresql://postgres:postgres@localhost:5432/postgres \
  python -m unittest tests.test_stores_integration -v
```

## Local end-to-end

```bash
k3d cluster create kuberag-dev
make install                 # CRDs
make run &                   # operator against your kubeconfig (or `make deploy`)
make sample                  # Qdrant + worker RBAC + example KnowledgeBase + Retriever
kubectl get kb,vi,rtr -w
```

For a scripted demo see [`hack/demo.sh`](hack/demo.sh).

## Pull request process

1. **Open an issue first** for non-trivial changes. The maintainer triages
   weekly; a "drive-by" PR without a linked issue is likely to stall.
2. **Branch from `main`.** PRs target `main` directly; we don't use
   long-lived feature branches.
3. **One logical change per PR.** Multi-feature PRs are hard to review and
   hard to bisect when something breaks.
4. **Fill out the PR template** (`.github/PULL_REQUEST_TEMPLATE.md`).
   Include:
   - The motivation (which user problem this fixes).
   - A note for each file category (controller, CRD, worker, retriever,
     docs, tests, CI).
   - The release-note blurb (one sentence, fits in the changelog).
5. **CI must be green.** Lint, build, generated-artifacts-check, unit,
   integration, Python tests, container scan, e2e (the last runs only
   on push to `main`).
6. **Self-review your diff** before requesting review. Run
   `git diff main...HEAD` and read it as if you were the reviewer.
7. **At least one approval** from a maintainer. For API or security
   changes, two.
8. **Squash-merge** with a Conventional Commits message (`feat:`,
   `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, `build:`, `ci:`).
9. **Delete the branch** after merge.

### What gets reviewed harder

- Anything that touches `api/v1alpha1/`. Once a field is in the CRD we
  can't remove it without a deprecation cycle.
- Anything that touches the worker / retriever security boundary (DNS
  resolution, file paths, secret material).
- Anything that adds a new image (we now publish 3; each one is a
  release artifact with a CVSS surface).
- Anything that touches `internal/controller/knowledgebase_controller.go`
  (the core reconcile loop). The auto-tune state machine is delicate
  and changes have caused silent re-index loops in the past.

## How to add a new component

### New source type (e.g. Confluence, Notion)

1. Add a Go type for the source block in `api/v1alpha1/knowledgebase_types.go`,
   following the pattern of `GitHubSource` / `S3Source` / `WebSource`. Use
   the existing CEL rules as templates.
2. Run `make manifests generate` and commit the updated CRD manifest.
3. Add the source handler in `worker/rag_worker/sources.py`. The contract:
   a `fetch(src: dict) -> SourceDocs` function that returns
   `(revision, [(doc_path, text)])`. The revision is a stable string used
   for incremental-skip comparisons (git SHA, S3 ETag hash, etc.).
4. If the source needs new credentials, add a `tokenSecretRef` /
   `credentialsSecretRef` and extend `internal/controller/secrets.go`'s
   `appendSecretHash` to include it. Otherwise secret rotation won't
   trigger credential refresh.
5. Add a `make sample-NAME` target and a sample manifest under
   `config/samples/NAME-test.yaml`.
6. Add tests in `worker/tests/test_sources.py`.
7. Document in `docs/PROVIDERS.md` and the `API.md` reference.

### New vector store (e.g. Weaviate)

1. Implement the `VectorStore` ABC in `worker/rag_worker/stores.py`. Every
   method is documented in the class; failing to implement `swap_collection`
   (atomic promotion) is a correctness issue for incremental ingest.
2. Add the `make_store` factory branch.
3. Add a Go enum constant for the store type in
   `api/v1alpha1/knowledgebase_types.go` (`VectorStoreType`).
4. Run `make manifests generate`.
5. Add a sample manifest at `config/samples/NAME.yaml` and a probe path
   in `internal/controller/vectorindex_controller.go`'s `probeStore`.
6. Add real-store integration tests in `worker/tests/test_stores_integration.py`
   (Qdrant-style; only added to the nightly job if it's heavy).
7. Document in `docs/PROVIDERS.md`.

### New provider (e.g. Cohere embeddings, Anthropic generation)

1. Add the base URL and known dimensions to `worker/rag_worker/embeddings.py`
   or `worker/retriever/server.py` constants.
2. If the SDK isn't `openai`-compatible, add a dedicated backend; otherwise
   the existing OpenAI SDK is enough.
3. Document in `docs/PROVIDERS.md`.

## Release checklist

For each release:

1. Cut a release branch from `main`: `git checkout -b release/v0.X.Y`.
2. Bump image tags in `Makefile` and Helm chart's `Chart.yaml`.
3. Update `CHANGELOG.md` with the `Added / Changed / Deprecated / Removed / Fixed / Security` sections. PRs merged since the last release provide
   the material; `git log --oneline v0.(X-1).0..HEAD` is the starting
   point.
4. Tag: `git tag -s v0.X.Y -m "v0.X.Y"`. (Requires a GPG key configured in
   GitHub.)
5. Push the tag: `git push origin v0.X.Y`. The release workflow
   builds and pushes the three images, signs the artifacts, and
   creates a GitHub release with auto-generated notes.
6. Verify on a fresh cluster: `make sample && kubectl get kb,vi,rtr -w`.
7. Announce on the project discussion / mailing list.

## Reporting issues

- **Bugs** — use the bug-report template. Include `kubectl get
  knowledgebase -o yaml` output and the operator logs.
- **Feature requests** — use the feature-request template. The
  maintainer triages against `ROADMAP.md`.
- **Security** — see [`SECURITY.md`](SECURITY.md). Do not open a
  public issue.

## Code of conduct

Be kind. Critique the code, not the author. Assume good intent. We're
all volunteers working on infrastructure that nobody pays us to
maintain.
