IMG ?= ghcr.io/furkandogmus/rag-operator:latest
WORKER_IMG ?= ghcr.io/furkandogmus/rag-worker:latest
RETRIEVER_IMG ?= ghcr.io/furkandogmus/rag-retriever:latest
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

.PHONY: all
all: build

.PHONY: generate
generate: ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths="./api/..."

.PHONY: manifests
manifests: ## Generate CRD + RBAC manifests.
	$(CONTROLLER_GEN) crd rbac:roleName=rag-operator-role paths="./..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac

.PHONY: fmt vet build test
fmt:
	go fmt ./...
vet:
	go vet ./...
test: generate fmt vet ## Run Go unit tests.
	go test ./...
build: generate fmt vet ## Build the operator binary.
	go build -o bin/manager ./cmd

.PHONY: test-py
test-py: ## Run Python worker tests.
	cd worker && python3 -m unittest discover -s tests

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
worker-rbac: ## Install the worker ServiceAccount + RBAC (edit namespace as needed).
	kubectl apply -f config/rbac/worker_rbac.yaml
undeploy:
	kubectl delete -f config/manager/manager.yaml || true
	kubectl delete -f config/rbac/leader_election_role.yaml || true
	kubectl delete -f config/rbac/role.yaml || true
	kubectl delete -f config/crd || true

.PHONY: sample sample-clean
sample: worker-rbac ## Apply Qdrant + eval dataset + example KnowledgeBase + Retriever.
	kubectl apply -f config/samples/qdrant.yaml
	kubectl apply -f config/samples/eval-dataset.yaml
	kubectl apply -f config/samples/knowledgebase.yaml
	kubectl apply -f config/samples/retriever.yaml
sample-clean:
	kubectl delete -f config/samples/retriever.yaml --ignore-not-found
	kubectl delete -f config/samples/knowledgebase.yaml --ignore-not-found
	kubectl delete -f config/samples/eval-dataset.yaml --ignore-not-found
	kubectl delete -f config/samples/qdrant.yaml --ignore-not-found

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'
