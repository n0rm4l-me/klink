# Contributing to klink

## Development Setup

**Requirements:** Go 1.22+, kubebuilder v4, podman or docker, kind

```bash
git clone https://github.com/n0rm4l-me/klink
cd klink
go mod download
```

## Running Tests

```bash
# Install envtest binaries (first time only)
GOBIN=$(pwd)/bin go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
bin/setup-envtest use 1.33 --bin-dir bin/k8s

# Run tests
make test
```

## Building

```bash
# Generate CRDs and deepcopy code
make manifests generate

# Build binary
make build

# Build container image
make image-build IMG=your-registry/klink:dev

# Push image
make image-push IMG=your-registry/klink:dev
```

## Testing on a Local Cluster

```bash
# Start kind cluster
kind create cluster

# Deploy klink
helm upgrade --install klink ./charts/klink --namespace klink-system --create-namespace

# Apply a test WorkloadDependency
kubectl apply -f config/samples/deps_v1alpha1_workloaddependency.yaml
```

## Pull Request Process

1. Fork the repository and create a branch from `main`
2. Make your changes with tests
3. Run `make manifests generate` if you modified API types
4. Ensure `go build ./...` and `make test` pass
5. Submit a PR with a clear description

### Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add StatefulSet support
fix: nil check on annotations map
docs: update API reference
test: add gate mode webhook tests
refactor: extract workload accessor interface
```

## Code Style

- No comments explaining *what* code does — only non-obvious *why*
- No unnecessary abstractions
- Errors wrapped with `fmt.Errorf("context: %w", err)`
- Context always first argument

## Reporting Issues

Use GitHub Issues. For security vulnerabilities, see [SECURITY.md](SECURITY.md).
