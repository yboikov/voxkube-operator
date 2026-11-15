IMG ?= ghcr.io/yboikov/voxkube-operator:latest
NAMESPACE ?= openvox
CONTAINER_TOOL ?= $(shell which podman 2>/dev/null || which docker 2>/dev/null)
CONTROLLER_GEN = go tool controller-gen
GOVULNCHECK = go tool govulncheck

.PHONY: all
all: build

##@ Development

.PHONY: manifests
manifests: ## Generate CRD manifests for VoxKube (voxkube.io/v1alpha1) and sync into Helm chart.
	$(CONTROLLER_GEN) crd paths="./..." output:crd:dir=config/crd/bases
	mkdir -p charts/voxkube-operator/crds
	cp config/crd/bases/voxkube.io_*.yaml charts/voxkube-operator/crds/

.PHONY: generate
generate: ## Generate deepcopy methods.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet ## Run unit and controller tests.
	go test ./... -v -coverprofile cover.out

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build voxkube-operator manager binary.
	go build -o bin/manager ./cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run voxkube-operator locally against the configured cluster.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build voxkube-operator container image.
	$(CONTAINER_TOOL) build -t $(IMG) -f images/voxkube-operator/Containerfile .

.PHONY: docker-push
docker-push: ## Push voxkube-operator container image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Deployment & Helm

.PHONY: install
install: manifests ## Install voxkube-operator via Helm.
	helm upgrade --install voxkube-operator charts/voxkube-operator \
		--namespace $(NAMESPACE) --create-namespace

.PHONY: uninstall
uninstall: ## Remove voxkube-operator and CRDs from the cluster.
	-helm uninstall voxkube-operator --namespace $(NAMESPACE) 2>/dev/null
	-kubectl delete -f config/crd/bases/ --ignore-not-found

.PHONY: helm-lint
helm-lint: ## Lint the voxkube-operator Helm chart.
	helm lint charts/voxkube-operator

.PHONY: helm-template
helm-template: ## Render voxkube-operator Helm chart templates locally.
	helm template voxkube charts/voxkube-operator --set cluster.enabled=true

##@ Help

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
