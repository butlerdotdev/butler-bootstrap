# ADR-001: KIND-based Bootstrap Controller Pattern

## Status

Accepted

## Context

Butler needs to bootstrap management clusters on infrastructure where no Kubernetes cluster exists yet. This creates a chicken-and-egg problem: we want to use Kubernetes controllers for orchestration, but we have no cluster to run them on.

Several approaches were considered:

1. **Shell scripts**: Direct CLI calls to provision VMs, configure Talos, install addons
2. **Standalone binary**: Go binary that handles everything without Kubernetes
3. **KIND bootstrap cluster**: Temporary local Kubernetes cluster to run controllers

The shell script approach was initially implemented but proved fragile. It suffered from:
- Hardcoded wait times instead of proper readiness checks
- No idempotency or retry logic
- Massive code duplication across providers
- Difficult to test and debug

## Decision

We use a temporary KIND cluster on the operator's local machine to run butler-bootstrap and provider controllers during management cluster creation.

The flow:
1. `butleradm bootstrap` creates a KIND cluster
2. CRDs and controllers are deployed to KIND
3. A ClusterBootstrap CR is created
4. Controllers reconcile: provision VMs, configure Talos, install addons
5. Once the management cluster is ready, KIND is deleted

This gives us:
- Standard Kubernetes reconciliation patterns (watch, retry, idempotency)
- Clear separation between orchestration (butler-bootstrap) and VM provisioning (butler-provider-*)
- Testable controllers using envtest
- Status reporting via CR status fields
- Ability to pause/resume bootstrap by modifying the CR

## Consequences

### Positive

- Controllers follow standard Kubernetes patterns and are easier to reason about
- Provider implementations are isolated and can be developed independently
- Bootstrap state is visible via kubectl against the KIND cluster
- Failed bootstraps can be debugged by inspecting CR status and events
- Same controller code can potentially run in other contexts (existing cluster, CI)

### Negative

- Requires Docker on the operator's machine to run KIND
- Adds startup latency (KIND cluster creation takes 30-60 seconds)
- Local machine must maintain network connectivity to target infrastructure throughout bootstrap
- KIND cluster consumes local resources during bootstrap

### Neutral

- KIND cluster is ephemeral and deleted after bootstrap completes
- Operator does not need to understand the KIND internals; butleradm abstracts it away
