IMG ?= ghcr.io/furkandogmus/kuberag:latest
WORKER_IMG ?= ghcr.io/furkandogmus/kuberag-worker:latest
RETRIEVER_IMG ?= ghcr.io/furkandogmus/kuberag-retriever:latest
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

.PHONY: all
all: build

.PHONY: generate
generate: ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths="./api/..."

.PHONY: manifests
manifests: ## Generate CRD + RBAC manifests.
	$(CONTROLLER_GEN) crd rbac:roleName=kuberag-role paths="./..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac

ENVTEST_VERSION ?= release-0.19
ENVTEST_K8S_VERSION ?= 1.31.0
PYTHON ?= python3
PYTEST_VENV ?= .venv-test
PYTEST_PYTHON := $(PYTEST_VENV)/bin/python

.PHONY: fmt vet build test test-integration test-coverage
fmt:
	go fmt ./...
vet:
	go vet ./...
test: generate fmt vet ## Run Go unit tests.
	go test ./...
test-integration: generate ## Run envtest integration tests (downloads kube-apiserver/etcd).
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -tags=integration -count=1 -timeout=300s ./internal/controller/...
test-coverage: ## Run unit tests and write coverage profile (cover.out).
	go test -count=1 -coverprofile=cover.out -covermode=atomic ./... >/dev/null
	@echo ""
	@echo "Per-package coverage (must be >= MIN_COVERAGE, default 50%):"
	@go tool cover -func=cover.out | grep -E "total:" | awk '{print "  " $$0}'
build: generate fmt vet ## Build the operator binary.
	go build -o bin/manager ./cmd

.PHONY: test-py test-py-deps
test-py-deps: ## Install Python test dependencies into a local venv.
	$(PYTHON) -m venv $(PYTEST_VENV)
	$(PYTEST_PYTHON) -m pip install -r worker/requirements-test.txt
test-py: ## Run Python worker tests.
	$(PYTEST_PYTHON) -m unittest discover -s worker/tests

.PHONY: api-docs
api-docs: manifests ## Regenerate docs/API.md from the rendered CRD YAML.
	$(PYTHON) hack/gen-api-docs.py

.PHONY: lint-helm lint-kustomize
lint-helm: ## Lint the Helm chart.
	@which helm >/dev/null || (echo "helm not found; install from https://helm.sh"; exit 1)
	helm lint deploy/helm/kuberag/
	helm template kuberag-scoped deploy/helm/kuberag \
		--namespace kuberag-system \
		--set rbac.scope=namespace \
		--set rbac.watchNamespace=tenant-a >/dev/null

lint-kustomize: ## Render the Kustomize base to catch broken resource references.
	go run sigs.k8s.io/kustomize/kustomize/v5@v5.7.1 build config >/dev/null

NAMESPACE ?= default
RETRIEVER ?= kuberag-retriever
CONCURRENCY ?= 4
DURATION ?= 30

.PHONY: benchmark
benchmark: ## Run a load benchmark against a deployed retriever.
	./hack/benchmark.sh $(NAMESPACE) $(RETRIEVER) $(CONCURRENCY) $(DURATION)

.PHONY: upgrade-test
upgrade-test: ## Run the Helm chart upgrade simulation.
	./hack/upgrade-test.sh

.PHONY: govulncheck
govulncheck: ## Scan Go dependencies for known vulnerabilities.
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: demo
demo: ## Create a k3d cluster and run a full end-to-end demo.
	./hack/demo.sh

.PHONY: run
run: generate ## Run the operator against the current kubeconfig.
	go run ./cmd

.PHONY: docker-build docker-build-worker docker-build-retriever docker-build-all
docker-build: ## Build the operator image.
	docker build -t $(IMG) .
docker-build-worker: ## Build the worker image.
	docker build -t $(WORKER_IMG) -f worker/Dockerfile worker/
docker-build-retriever: ## Build the retriever server image.
	docker build -t $(RETRIEVER_IMG) -f worker/Dockerfile.retriever worker/
docker-build-all: docker-build docker-build-worker docker-build-retriever ## Build all images.

.PHONY: install uninstall deploy undeploy worker-rbac
install: manifests ## Install CRDs into the cluster.
	kubectl apply -f config/crd
uninstall:
	kubectl delete -f config/crd
deploy: manifests ## Deploy operator (CRD + RBAC + manager) into the cluster.
	kubectl apply -f config/crd
	kubectl apply -f config/rbac/role.yaml
	kubectl apply -f config/manager/manager.yaml          # creates the namespace
	kubectl apply -f config/rbac/leader_election_role.yaml # needs the namespace
worker-rbac: ## Install the legacy shared worker ServiceAccount + RBAC.
	kubectl apply -f config/rbac/worker_rbac.yaml -n default
undeploy:
	kubectl delete -f config/manager/manager.yaml || true
	kubectl delete -f config/rbac/leader_election_role.yaml || true
	kubectl delete -f config/rbac/role.yaml || true
	kubectl delete -f config/crd || true

.PHONY: sample sample-clean
sample: ## Apply Qdrant + eval dataset + example KnowledgeBase + Retriever.
	kubectl apply -f config/samples/qdrant.yaml
	kubectl apply -f config/samples/eval-dataset.yaml
	kubectl apply -f config/samples/knowledgebase.yaml
	kubectl apply -f config/samples/retriever.yaml
sample-clean:
	kubectl delete -f config/samples/retriever.yaml --ignore-not-found
	kubectl delete -f config/samples/knowledgebase.yaml --ignore-not-found
	kubectl delete -f config/samples/eval-dataset.yaml --ignore-not-found
	kubectl delete -f config/samples/qdrant.yaml --ignore-not-found

.PHONY: verify-pss
verify-pss: ## Verify Pod Security Standards compliance.
	./hack/verify-pss.sh

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'
