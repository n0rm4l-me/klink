IMG ?= ghcr.io/n0rm4l-me/klink:0.2.0

LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION ?= v0.20.1

.PHONY: all
all: build

.PHONY: build
build:
	go build -o bin/manager ./cmd/main.go

.PHONY: generate
generate: controller-gen
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=2026 paths="./..."

.PHONY: manifests
manifests: controller-gen
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=testdata/crd
	cp testdata/crd/deps.klink.dev_workloaddependencies.yaml charts/klink/templates/crd.yaml

.PHONY: test
test:
	KUBEBUILDER_ASSETS=$(shell $(LOCALBIN)/setup-envtest use 1.33 --bin-dir $(LOCALBIN)/k8s -p path) \
		go test ./internal/... -timeout 120s

# Run e2e tests against a real cluster (requires kubectl context + klink deployed)
.PHONY: test-e2e
test-e2e:
	go test ./test/e2e/... -v -timeout 10m

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: image-build
image-build:
	podman build --platform linux/amd64 \
		--build-arg TARGETOS=linux \
		--build-arg TARGETARCH=amd64 \
		-t $(IMG) .

.PHONY: image-push
image-push:
	podman push $(IMG)

.PHONY: controller-gen
controller-gen:
	@[ -f $(CONTROLLER_GEN) ] || { \
		mkdir -p $(LOCALBIN); \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); \
	}
