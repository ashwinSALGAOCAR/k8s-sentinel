# k8s-sentinel

A small internal Go service for SREs to isolate workloads on demand (network-level) and
harden workloads on demand (resource limits + securityContext), running inside the
cluster it manages.

> Status: all 7 phases (environment through documentation) complete and live-verified
> against a real `kind`+Calico cluster. Reasoning behind every decision is in
> [`DESIGN.md`](./DESIGN.md); the full build log is in [`REPLICATION.md`](./REPLICATION.md).

---

## TL;DR

- **What it is**: an HTTP service (`cmd/server`, `internal/httpapi`) that runs inside the cluster and does two things on demand: `POST /isolate` blocks traffic between two named workloads via real `NetworkPolicy` objects (`internal/netpol`), and `POST /harden` scans namespaces for missing resource limits/`securityContext` and optionally patches them (`internal/harden`).
- **Biggest gotcha solved**: `kind`'s default CNI doesn't enforce `NetworkPolicy` at all — swapped in Calico, otherwise isolation would silently do nothing.
- **How "block A↔B, keep everything else" is expressed**: standard `NetworkPolicy` has no negation operator, so it's built from `NotIn` namespace-label match expressions instead of a CNI-specific deny rule — kept portable on purpose.
- **State**: no in-memory state anywhere — Kubernetes labels are the source of truth for both isolation (`k8s-sentinel.io/managed-by`) and hardening (re-detected fresh on every call), so the service is safely restartable.
- **Two real findings, not just reasoning**: (1) `runAsNonRoot: true` without a forced `runAsUser` can crash-loop images with no non-root `USER` declared — kept as-is deliberately, fails loudly rather than silently masking the problem; (2) `readOnlyRootFilesystem: true` can crash-loop apps that write at runtime (confirmed against nginx) — the tool doesn't provision volumes for anyone, same reasoning.
- **Proof it actually works, not just unit tests**: `scripts/verify-isolation.sh` and `scripts/verify-hardening.sh` are standalone, Go-toolchain-free bash scripts that port-forward to the *real deployed* service and exercise it end-to-end against the live cluster.
- **Full detail**: [`DESIGN.md`](./DESIGN.md) for every design decision and trade-off; [`REPLICATION.md`](./REPLICATION.md) for the complete command-by-command build log, every real issue hit and how it was fixed, and a manual CLI testing walkthrough.

---

## Core tasks (from the brief)

1. **Network isolation (on demand)** — block traffic between two workloads (defined by
   namespace + label selector) without breaking anything else, and fully restore prior
   connectivity when turned off.
2. **Workload hardening (on demand)** — patch workloads in one or more namespaces that are
   missing resource requests/limits or a hardened `securityContext`, with a dry-run mode
   to preview changes before applying.

---

## Prerequisites

To replicate this setup/build from scratch, you need on your machine:

| Tool | Why it's needed | Verify with |
|---|---|---|
| **macOS with Homebrew** | Used to install everything below. | `brew --version` |
| **Go** | The tool itself is written in Go. | `go version` |
| **Docker Desktop** | `kind` runs Kubernetes nodes as Docker containers — needs the actual daemon running, not just the CLI client. | `docker version` (must show a `Server:` section, not just `Client:`) |
| **`kind`** | Creates the local Kubernetes cluster used for development and the required end-to-end verification test. | `kind version` |
| **`kubectl`** | Used by the setup script and for manual inspection/verification. | `kubectl version --client` |
| **Internet access** | The cluster setup script pulls Calico's manifests directly from GitHub at cluster-creation time. | — |

No Kubernetes cluster needs to pre-exist — `scripts/setup-cluster.sh` creates one from scratch.

---

## Design decisions and trade-offs

The brief deliberately leaves several choices open — how isolation should behave as pods
come and go, what counts as "hardened," how state is tracked, which namespaces are
off-limits, and more. Each one is recorded with its reasoning in **[`DESIGN.md`](./DESIGN.md)**,
including the trade-off analysis between standard `NetworkPolicy` and Calico-native
policies, and one edge case (a potential `ipBlock` bypass) that was tested live against
the real cluster rather than just reasoned about.

---

## How it's built: phase by phase

Built and verified in order — environment, cluster, service skeleton, isolation, hardening,
deployment manifests, then tests and verification scripts. Every real issue hit along the
way (and how it was fixed) is logged in detail in **[`REPLICATION.md`](./REPLICATION.md)**;
this is just the summary.

### Phase 0 — Environment setup
- [x] Go, Docker Desktop, `kind`, `kubectl` installed and verified
- [x] `git init`, GitHub repository created and pushed

### Phase 1 — Cluster bootstrap
- [x] `scripts/setup-cluster.sh` + `scripts/kind-cluster-config.yaml` — creates the `kind`
  cluster with the default CNI disabled and installs Calico (see `DESIGN.md` for why)
- [x] Sample workloads (`manifests/samples/tenant-a.yaml`, `tenant-b.yaml`, `tenant-c.yaml`)
  applied; baseline connectivity confirmed across all five test vectors before any
  isolation exists

### Phase 2 — Service skeleton
- [x] `internal/k8sclient` (clientset, in-cluster or kubeconfig), `internal/httpapi`
  (health/readiness), `cmd/server/main.go` (entrypoint, graceful shutdown)
- [x] Verified against the real cluster: `/healthz`/`/readyz` both `200`, clean shutdown on
  `SIGTERM`

### Phase 3 — Network isolation
- [x] `internal/netpol/generator.go` (`NetworkPolicy` generation) +
  `internal/httpapi/isolate.go` (`POST`/`GET`/`DELETE /isolate`) — 9 unit tests, all passing
- [x] `internal/netpol/integration_test.go` — automated end-to-end test against the real
  cluster: confirms isolation blocks only the intended pair and restores cleanly

### Phase 4 — Workload hardening
- [x] `internal/harden/{baseline,detector,patcher}.go` + `internal/httpapi/harden.go`
  (`POST /harden`) — 10 + 4 unit/handler tests, all passing
- [x] Live-verified against the real cluster: dry-run reports accurate gaps without
  mutating anything, `apply: true` patches correctly

### Phase 5 — Deployment manifests
- [x] Multi-stage `Dockerfile` (distroless, non-root) + `manifests/k8s-sentinel/deploy.yaml`
  (Namespace/ServiceAccount/ClusterRole/Deployment/Service)
- [x] Deployed to the real cluster: pod healthy under its own hardened `securityContext`,
  RBAC confirmed exactly sufficient for every code path that runs — nothing broader

### Phase 6 — Tests + verification scripts
- [x] `go test ./...` passing (23 tests, fast and cluster-free) plus the one
  cluster-dependent integration test
- [x] `scripts/verify-isolation.sh` and `scripts/verify-hardening.sh` — standalone bash
  scripts, no Go toolchain required, proving both features end-to-end against the real
  deployed service; both pass reliably on repeated runs

### Phase 7 — Documentation + submission
- [x] [`DESIGN.md`](./DESIGN.md) — every design decision and trade-off, with reasoning
- [x] [`REPLICATION.md`](./REPLICATION.md) — full command-by-command build log, every
  issue hit and how it was resolved, plus a manual CLI testing walkthrough
- [ ] Record actual time spent against the stated 4–6 hour core-task budget
- [ ] Disclose AI-assisted development per the brief's explicit requirement, and be ready
  to explain/defend every part at interview

---

## Known limitations
- No undo for hardening (bonus-list item, not core)
- `Job`/`CronJob`/bare `Pod` workload types not handled
- **No authentication/authorization on the HTTP API itself** — `/isolate` and `/harden` trust anything that can reach the Service, with no caller-identity check in the Go code. Deliberately out of scope for this pass: the focus was proving the isolation/hardening logic itself works, and this is the kind of concern that's normally solved *around* the service rather than inside it — e.g. a `NetworkPolicy` restricting which pods/namespaces can reach `k8s-sentinel-system`, or fronting it with a service mesh/mTLS, rather than hand-rolling auth into the handlers. Noted here rather than silently assumed away.
- **`runAsNonRoot: true` without `runAsUser` can make hardening fail an image outright** — confirmed live against `nicolaka/netshoot:latest`, which has no non-root `USER` directive: kubelet refuses to start the hardened pod with `cannot verify user is non-root`, rather than silently starting it as root. Considered and rejected adding an automatic `runAsUser: 65532` ("nonroot" distroless convention) fallback to the baseline:
  - **Why not force a UID**: it would make `runAsNonRoot` "pass" even on images never built to run as non-root, hiding exactly the thing this feature exists to catch — and a forced UID can itself break images whose files on disk are owned by a different UID, trading one silent failure mode for another.
  - **Why the current behavior is the right default**: it fails loudly and immediately (`CreateContainerConfigError`, visible in `kubectl get pods`/`describe pod`), is caught by the dry-run plan before anyone applies it for real, and matches the report-first philosophy already baked into this tool (`apply: true` is opt-in, never silent). The old pod also keeps serving traffic throughout, since the RollingUpdate default won't tear it down until the replacement passes its health checks.
  - **What this means in practice**: operators should expect that `apply: true` on an image whose base doesn't declare a non-root `USER` will need that fixed at the image level (or the namespace/workload excluded from hardening) — this tool surfaces the gap, it doesn't paper over it.
- **`readOnlyRootFilesystem: true` can crash-loop a workload that needs to write at runtime** — confirmed live against `nginxinc/nginx-unprivileged`, which (independent of the `runAsNonRoot` issue above) fails with `mkdir() "/tmp/proxy_temp" failed (30: Read-only file system)` because nginx needs writable temp/cache directories. The hardening tool does **not** provision `emptyDir` volumes or otherwise try to guess which paths an arbitrary workload needs writable — it has no generic way to know. This is the same category of trade-off as the `runAsNonRoot` finding above: the dry-run plan can't predict this failure mode either (it only inspects the pod spec, not the application's runtime behavior), so `apply: true` on a workload with this issue will crash-loop it, visibly, until an operator either adds the volume themselves or excludes that workload from hardening. `manifests/samples/hardening-demo.yaml` carries its own `emptyDir` at `/tmp` specifically so it can demonstrate the full successful path — that accommodation lives in the fixture, not in the tool.
