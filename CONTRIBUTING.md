# Contributing to Butler Bootstrap

Thank you for your interest in contributing to Butler Bootstrap!

## Development Setup

### Prerequisites

- Go 1.24+
- Docker
- kubectl
- make
- Access to a Kubernetes cluster (for integration testing)

### Building

```bash
# Build binary
make build

# Build container image
make docker-build IMG=ghcr.io/butlerdotdev/butler-bootstrap:dev
```

### Running Locally

```bash
# Run controller locally against your kubeconfig
make run
```

### Running Tests

```bash
# Unit tests
make test

# Linting
make lint

# All checks
make test && make lint
```

## Code Guidelines

### Project Structure

- `cmd/main.go` - Controller manager entry point
- `internal/controller/` - Reconciliation logic
- `internal/addons/` - Helm-based addon installation
- `internal/talos/` - Talos API client wrapper
- `internal/crds/` - Embedded CRD YAML for self-registration

### Bootstrap Phases

The controller drives ClusterBootstrap through these phases:

```
Pending → ProvisioningMachines → ConfiguringTalos → BootstrappingCluster → InstallingAddons → Pivoting → Ready
```

Each phase has specific responsibilities:
- **ProvisioningMachines**: Creates MachineRequest CRDs, waits for VMs
- **ConfiguringTalos**: Generates and applies Talos configs
- **BootstrappingCluster**: Bootstraps first control plane, retrieves kubeconfig
- **InstallingAddons**: Installs platform components in dependency order
- **Pivoting**: Makes cluster self-managing (HA only)
- **Ready**: Bootstrap complete

### Adding a New Addon

1. Add installation method to `AddonInstallerInterface` in `internal/addons/installer.go`
2. Implement the method with Helm SDK
3. Add to the installation order in the controller
4. Update documentation

### Testing

The controller uses `envtest` for unit testing. Key interfaces (`TalosClientInterface`, `AddonInstallerInterface`) are designed for mocking.

```go
// Example test with mock
func TestReconcile_ConfiguringTalos(t *testing.T) {
    mockTalos := &mocks.MockTalosClient{}
    mockTalos.On("GenerateConfig", mock.Anything, mock.Anything).Return(&talos.TalosConfigs{}, nil)

    reconciler := &ClusterBootstrapReconciler{
        TalosClient: mockTalos,
    }
    // ...
}
```

### Key Conventions

- Requeue intervals: `requeueShort` (5s) for active operations, `requeueMedium` (15s) for waiting
- Use finalizers for cleanup: `clusterbootstrap.butler.butlerlabs.dev/finalizer`
- Status conditions follow K8s conventions: `Ready`, `Progressing`, `Failed`
- Talos configs stored in Secrets in the ClusterBootstrap namespace

## Pull Request Process

1. Fork the repository
2. Create a feature branch from `main`
3. Make your changes
4. Run tests and linting: `make test && make lint`
5. Commit with conventional commit messages
6. Push to your fork and open a PR

### Commit Message Format

```
type(scope): description

[optional body]

[optional footer]
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`

Examples:
- `feat(addons): add external-dns installation`
- `fix(talos): handle config apply timeout`
- `docs: update bootstrap phase documentation`

## Developer Certificate of Origin

By contributing to this project, you agree to the Developer Certificate of Origin (DCO). This means you certify that you wrote the contribution or have the right to submit it under the project's license.

Sign off your commits with `git commit -s` or add `Signed-off-by: Your Name <your.email@example.com>` to your commit messages.

Any merge request, issue posts, commit, or any other content development on this project legally constitutes signed acceptance of the terms in the DCO, meaning that any contribution to this project in any way releases any legal claim on those contributions including copyright, patents, trademarks, and any other intellectual property rights.

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
