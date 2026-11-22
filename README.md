# k8s-sentinel

A small internal Go service for SREs to isolate workloads on demand (network-level) and
harden workloads on demand (resource limits + securityContext), running inside the
cluster it manages.

> Status: all 7 phases (environment through documentation) complete and live-verified
> against a real `kind`+Calico cluster. This README was updated phase-by-phase as the
> work landed, not written retroactively — see `REPLICATION.md` for the full command log.

---

## TL;DR

- **What it is**: an HTTP service (`cmd/server`, `internal/httpapi`) that runs inside the cluster and does two things on demand: `POST /isolate` blocks traffic between two named workloads via real `NetworkPolicy` objects (`internal/netpol`), and `POST /harden` scans namespaces for missing resource limits/`securityContext` and optionally patches them (`internal/harden`).
- **Biggest gotcha solved**: `kind`'s default CNI doesn't enforce `NetworkPolicy` at all — swapped in Calico, otherwise isolation would silently do nothing.
- **How "block A↔B, keep everything else" is expressed**: standard `NetworkPolicy` has no negation operator, so it's built from `NotIn` namespace-label match expressions instead of a CNI-specific deny rule — kept portable on purpose.
- **State**: no in-memory state anywhere — Kubernetes labels are the source of truth for both isolation (`k8s-sentinel.io/managed-by`) and hardening (re-detected fresh on every call), so the service is safely restartable.
- **Two real findings, not just reasoning**: (1) `runAsNonRoot: true` without a forced `runAsUser` can crash-loop images with no non-root `USER` declared — kept as-is deliberately, fails loudly rather than silently masking the problem; (2) `readOnlyRootFilesystem: true` can crash-loop apps that write at runtime (confirmed against nginx) — the tool doesn't provision volumes for anyone, same reasoning.
- **Proof it actually works, not just unit tests**: `scripts/verify-isolation.sh` and `scripts/verify-hardening.sh` are standalone, Go-toolchain-free bash scripts that port-forward to the *real deployed* service and exercise it end-to-end against the live cluster.
- **Full detail below**: design-decision table, phase-by-phase build log, every real bug hit and how it was found/fixed, and (in `REPLICATION.md`) the complete command-by-command replication log plus a manual CLI testing walkthrough.

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

## Key design decisions (made upfront, revisited if wrong)

These are the open questions the brief deliberately leaves to us. Documenting the call now;
will justify at interview.

| Decision | Choice | Why |
|---|---|---|
| **How isolation behaves as pods come and go** | Implemented as real Kubernetes `NetworkPolicy` objects, selector-based, not a static list of pod IPs. | NetworkPolicies are continuously enforced by the CNI against *current* matching pods — new pods matching the selector are automatically covered without the tool re-running anything. This is the only approach that's correct "over time," not just at the moment of the API call. |
| **CNI choice for `kind`** | Disable the default `kindnet` CNI, install **Calico** instead. | `kindnet` (kind's default) does **not enforce** `NetworkPolicy` at all — policies would apply cleanly via the API but silently do nothing. This is the single biggest hidden gotcha in this assignment; without this, "verify that isolation blocks traffic" cannot actually be demonstrated. |
| **How to express "block A↔B only, keep everything else working"** | Use Kubernetes' auto-injected `kubernetes.io/metadata.name` namespace label with `NotIn` match expressions, combined with multiple `to`/`from` peer entries in one rule. | Vanilla `NetworkPolicy` has no negation operator for selectors directly, but `matchExpressions` supports `NotIn`/`DoesNotExist`. Combining "allow to all namespaces except B's" + "within B's namespace, allow to everything except B's specific selector" reproduces an "allow all except this one peer" rule using only additive, standard `NetworkPolicy` — no CNI-specific extensions (e.g. Calico's own deny-policy CRDs) needed, keeping it portable. |
| **State tracking for "undo"** | No in-memory state. Every `NetworkPolicy` the tool creates gets a `k8s-sentinel.io/managed-by=k8s-sentinel` label + a request ID label. "Turn off" = delete-by-label-selector. "List active" = read live cluster state by label, not memory. | The service "runs in a cluster... think about how it behaves over time" — if isolation state lived only in memory, a pod restart would orphan live `NetworkPolicy` objects with no record of them. Making Kubernetes itself the source of truth makes the tool stateless and restart-safe by construction. |
| **What counts as "hardened" `securityContext`** | `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]`, `seccompProfile.type: RuntimeDefault` — applied per-container. | A widely recognized minimal hardening baseline (matches Pod Security Standards "restricted" profile), not an arbitrary personal choice. |
| **Default requests/limits vs. report-only** | Default behavior is **report only** (dry-run); applying a concrete default (e.g. `100m/128Mi` request, `500m/512Mi` limit) requires an explicit `apply: true` AND the caller may override the defaults in the request body. | Silently inventing resource numbers for workloads the tool doesn't understand is risky — e.g. a dashboard needs very different limits than a batch job. Report-first makes the SRE decide; explicit apply-with-override is the safety valve. |
| **Which workload types are handled** | `Deployment`, `StatefulSet`, `DaemonSet` only. | These share `.spec.template.spec` and cover the overwhelming majority of real workloads in the time budget. Bare `Pod`, `Job`, `CronJob` are out of scope for v1 — documented as a known limitation. |
| **How an SRE triggers actions / sees results** | A small HTTP API (JSON in/out) exposed by the running Deployment — matches "runs in a cluster," not a one-shot local CLI. `/healthz`+`/readyz` included since "think about... when things aren't healthy." | The brief explicitly contrasts "runs in a cluster" against "just on a good day" — implies a persistent service, not a script you run once from a laptop. |
| **Namespaces the tool will never touch** | `kube-system`, `kube-public`, `kube-node-lease`, `calico-system`/`tigera-operator` (CNI's own namespace), and the tool's own namespace. Hardcoded denylist, checked before any mutating action. | Prevents the tool from ever isolating or hardening its own control-plane/CNI dependencies, which would be catastrophic and hard to recover from. |
| **Undo for hardening** | Out of core scope per the brief (bonus list). Approach if tackled later: snapshot the pre-patch `resources`/`securityContext` into an annotation before applying, so a future `/harden/undo` could restore from it. Documented here so the "how I'd approach it" requirement is met even without building it. | Explicit bonus-list item; core task says this is fine to skip. |
| **Pre-existing conflicting NetworkPolicies** | Not merged/reconciled with. Before creating an isolation, the tool lists existing `NetworkPolicy` objects already selecting the target pod(s); if any exist that we don't own (no `k8s-sentinel.io/managed-by` label), the response includes a warning rather than claiming unconditional success. | `NetworkPolicy` rules are additive/unioned across all policies selecting a pod — a pre-existing broad "allow" policy would silently let traffic through regardless of what our policy says. Full reconciliation is out of scope for the time budget; honestly flagging the conflict is the cheap, correct middle ground. |
| **Preserving DNS and external egress** | Every generated Egress policy always includes (a) an `ipBlock: 0.0.0.0/0` allow rule for external traffic, and (b) an explicit allow rule to `kube-system` (CoreDNS) on TCP/UDP 53 — not optional, baked into every policy. | The instant a pod is selected by an Egress-type `NetworkPolicy`, it becomes default-deny for *everything* not explicitly allowed — including DNS resolution and the public internet. Without these two baseline rules, "other traffic should keep working" would be silently violated the moment isolation is applied, in a very visible way (nothing resolves hostnames). |
| **Edge case checked: does the broad `ipBlock: 0.0.0.0/0` egress rule leak a path around the isolation?** | Tested live, not just reasoned about: with `tenant-a/gateway` isolated from `tenant-b/dashboard`, curled `dashboard`'s pod IP directly (bypassing DNS/the Service entirely) from `gateway`. Still blocked (`HTTP 000`), same as the DNS-based check. | `0.0.0.0/0` is a raw CIDR that mathematically includes the cluster's own pod subnet (`192.168.0.0/16` here), and Kubernetes' own docs only say `ipBlock` *should* be used for cluster-external IPs — the API spec doesn't itself guarantee pod IPs are excluded from matching it, and `NetworkPolicy` rules are OR'd (any matching rule allows), so this was a real question, not a theoretical one. Confirmed Calico evaluates pod-to-pod traffic against the `NamespaceSelector`/`PodSelector` rules regardless of whether the destination IP also happens to fall inside an `ipBlock` allow rule — no fix needed, but worth having actually verified rather than assumed. |

---

## Design trade-off: standard `NetworkPolicy` vs. Calico-native policies

Considered seriously before committing, not picked by default — worth recording the actual reasoning:

**What Calico's own `NetworkPolicy`/`GlobalNetworkPolicy` CRDs offer that vanilla `NetworkPolicy` doesn't**: an explicit `Deny` action with numeric priority ordering. Standard Kubernetes `NetworkPolicy` has no deny verb at all — every policy is purely additive, and the final allowed traffic for a pod is the *union* of every policy selecting it. That means the "pre-existing conflicting policy" gap above (a broad pre-existing "allow" policy silently overriding our intended isolation) is a **structural limitation of the standard API**, not a bug in our implementation — Calico's priority-ordered `Deny` would genuinely close that specific gap at the API level, not just detect it.

**Everything else is *not* solved by switching to Calico's API** — these are selector-based/CNI-agnostic issues either way:
- DNS/external-egress breakage on first policy attachment happens with any CNI enforcing `NetworkPolicy`, Calico included.
- Selector-specificity collateral matches (a label selector catching more pods than intended) is a risk of any selector-based targeting system.

**Decision: ship standard `NetworkPolicy`, not Calico-native policies.** Reasoning:
1. **Portability** — ties the tool to any CNI that enforces standard `NetworkPolicy` (Calico, Cilium, AWS VPC CNI's network policy agent, etc.), not just Calico-managed clusters.
2. **Matches the brief's own hint** toward plain `client-go` being "enough for the core tasks" — reaching for a CNI-specific extension without a forcing requirement reads as scope creep against what's being evaluated.
3. **The gap left open (pre-existing allow policies) has an honest, cheap mitigation already in place** (the conflict-detection warning above) — converts a silent failure into a visible one, which is the right bar for this task's scope.

**Documented path if stronger guarantees are ever needed**: if a future requirement demands isolation that holds even against adversarial/pre-existing allow policies, and the target clusters are confirmed to run Calico, swap the policy-generation layer to emit Calico's `NetworkPolicy` with an explicit high-priority `Deny` instead of standard `NetworkPolicy`'s additive-only model. Not pursued now because it would make the tool non-portable to any other CNI without a concrete need forcing that trade-off today.

*(May split this section into its own design-doc README later if it grows further — noting that here rather than doing it preemptively.)*

---

## Action plan (phased)

### Phase 0 — Environment setup
- [x] Install Go (`brew install go`) — done
- [x] Install `kind` (`brew install kind`) — done
- [x] Install Docker Desktop (`brew install --cask docker`) and start it — done, verified via `docker version` showing a reachable `Server:` daemon
- [ ] `git init`, initial commit, create GitHub repo at `github.com/ashwinSALGAOCAR/k8s-sentinel` — not yet done (directory is not a git repo yet)

### Phase 1 — Cluster bootstrap
- [x] `kind-cluster-config.yaml` written (lives in `scripts/`, alongside the script that consumes it) — 1 control-plane + 1 worker node, `disableDefaultCNI: true`, `podSubnet: 192.168.0.0/16` (matches Calico's default IPAM pool)
- [x] `scripts/setup-cluster.sh` written — checks prerequisites (docker/kind/kubectl on PATH, daemon reachable), creates the `kind` cluster, auto-detects the server's Kubernetes minor version to pick the matching Calico CRD manifest, installs the Tigera Operator + Calico custom resources, waits for `tigerastatus` to go all-`True` and all nodes `Ready`
- [x] **Cluster created and verified** — `kind-k8s-sentinel` is up (1 control-plane + 1 worker, `v1.37.0`), both nodes `Ready`. All Calico `tigerastatus` components (`apiserver`, `calico`, `goldmane`, `ippools`, `tiers`, `whisker`) report `AVAILABLE: True`. `calico-node` pods (the actual per-node `NetworkPolicy` enforcement agents) are `1/1 Running` on both nodes.
  - Bug fixed in the script's wait loop along the way: it used `grep -qv True` to detect "not all ready," which returns a false success on *empty* `tigerastatus` output (operator hasn't created any status objects yet) — not just on a genuine all-True result. Fixed to require at least one non-empty status line before considering the wait satisfied.
  - Minor setup hiccup: the config file was initially at the project root; the script expects it in `scripts/` — moved to match, rather than changing the script's path logic.
- [x] Sample workloads created (`manifests/samples/`), applied, and rolled out successfully:
  - `tenant-a/gateway` — `nicolaka/netshoot`, sleeps forever, used as the exec-into-and-test client (the "A" side of the brief's example)
  - `tenant-b/dashboard` — `nginx`, the workload actually isolated in tests/demos (the "B" side)
  - `tenant-b/widget` — bystander in the **same namespace** as the isolated target, different selector; proves the generated policy's exclusion is scoped to `app: dashboard` specifically, not all of `tenant-b`
  - `tenant-c/widget` — bystander in a **completely different namespace**; proves the "allow all namespaces except tenant-b" rule genuinely covers other namespaces, not just the two involved in the isolation request
- [x] **Baseline connectivity confirmed (pre-isolation, known-good reference point)** — from `tenant-a/gateway`: `tenant-b/dashboard` → `200`, `tenant-b/widget` → `200`, `tenant-c/widget` → `200`, external internet (`example.com`) → `200`, DNS resolution → resolves correctly. This baseline is what Phase 3's isolation test will diff against: after isolating `tenant-a/gateway` ↔ `tenant-b/dashboard`, only the first of these five should change to a failure/timeout.

### Phase 2 — Service skeleton
- [x] Go module initialized: `github.com/ashwinSALGAOCAR/k8s-sentinel`, Go 1.27.4, `k8s.io/client-go v0.33.0` + `k8s.io/api`/`k8s.io/apimachinery`
- [x] `internal/k8sclient` — builds a clientset, trying in-cluster config first and falling back to kubeconfig (`KUBECONFIG` env, `--kubeconfig` flag, or `~/.kube/config`) for local dev — no `controller-runtime`/`kubebuilder`, this tool is request/response driven, not a reconcile loop
- [x] `internal/httpapi` — HTTP server using the standard library's `net/http` `ServeMux` (Go 1.22+ method+pattern routing, no external router dependency needed); takes `kubernetes.Interface`, not the concrete clientset, specifically so later unit tests can substitute `client-go/kubernetes/fake`
  - `GET /healthz` — liveness only, never calls the Kubernetes API (a slow/unreachable API server should make the pod *un-ready*, not get the process itself killed and restarted)
  - `GET /readyz` — readiness, calls `Discovery().ServerVersion()` to confirm the Kubernetes API is actually reachable
- [x] `cmd/server/main.go` — structured logging via `log/slog` (JSON), graceful shutdown on `SIGTERM`/`SIGINT` with a 10s drain timeout — "think about how it behaves over time" means handling pod termination (scale-down, rollout, node drain) cleanly, not just staying up on a good day
- [x] **Verified end-to-end against the real `kind` cluster** (kubeconfig fallback path, not in-cluster — that path is exercised once this is actually deployed in Phase 5): `go build`/`go vet` clean, `/healthz` → `200 ok`, `/readyz` → `200 ok` (genuinely reached the cluster's API server), `SIGTERM` → clean log sequence (`shutdown signal received` → `shutdown complete`), no dropped connections

### Phase 3 — Network isolation
- [x] `internal/netpol/generator.go` — pure, cluster-free `NetworkPolicy` generation (the `NotIn`-namespace-label trick), plus the DNS/external-egress baseline rules and the ownership labels, covered by 9 passing unit tests (`internal/netpol/generator_test.go`) — including a dedicated test asserting the exact namespace-exclusion match expression, flagged in its own comment as the easiest part of this to silently get wrong
- [x] `internal/httpapi/isolate.go` — `POST /isolate` (create-or-update, so repeat requests are idempotent by construction via the deterministic, symmetric request ID), `DELETE /isolate/{id}` (restore, found by label not memory), `GET /isolate` (list, read live from the cluster) — plus the protected-namespace denylist and the pre-flight conflict-detection warning, both per the decisions table above
- [x] **Automated end-to-end test against the real `kind`+Calico cluster** (`internal/netpol/integration_test.go`, `//go:build integration` tag so the default `go test ./...` stays fast and cluster-independent; run explicitly with `go test -tags=integration ./internal/netpol/... -run TestIsolation -v`) — this is the "one tricky test" the brief asks for, chosen specifically because it's the one behavior that *can't* be verified with `client-go/kubernetes/fake`: a fake clientset would happily "create" a `NetworkPolicy` object with no CNI underneath enforcing it, so the test would pass while proving nothing real. **Confirmed passing**: isolates `tenant-a/gateway` ↔ `tenant-b/dashboard`, verifies the pair is actually blocked while the same-namespace bystander, cross-namespace bystander, external egress, and DNS all keep working, then clears the isolation and confirms full restoration (and confirms zero leftover `NetworkPolicy` objects afterward).
  - Bug fixed along the way, in the *test itself*: an ordering mistake between `defer srv.Close()` and a `t.Cleanup` safety-net callback — `defer` runs before `t.Cleanup` callbacks, so the safety-net's isolation-cleanup call hit an already-closed test server. Fixed by registering the server's own close via `t.Cleanup` too, ordered (LIFO) to run *after* the isolation cleanup rather than before it.

### Phase 4 — Workload hardening
- [x] `internal/harden/baseline.go` — the hardening baseline as data: `ResourceDefaults` (100m/128Mi request, 500m/512Mi limit, overridable per-request) and `hardenedSecurityContext()` (`runAsNonRoot`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`) — deliberately **not** setting `runAsUser` (see "Known limitations" below)
- [x] `internal/harden/detector.go` — pure `Detect(template) Plan`, reports exactly which containers are missing requests/limits and which specific securityContext fields fall short (not just a single boolean), so a dry-run response tells an SRE precisely what would change
- [x] `internal/harden/patcher.go` — pure `Patch(template, plan, resources)`, fills in only the detected gaps (deep-copies first, never mutates the input) — an already-hardened container, or one with only a partial gap, is left untouched everywhere except the specific missing fields
- [x] `internal/harden/detector_test.go` — 10 unit tests: already-hardened no-op, missing-resources-only, missing-securityContext-only, both-missing, partial-securityContext (only one field regressed — flagged as the trickiest case), multiple containers (only the one with gaps is flagged), patch fills only detected gaps, patch on an already-hardened template is a no-op, patch never mutates the original, resource overrides are respected. **All passing.**
- [x] `internal/httpapi/harden.go` — `POST /harden` handler; `workloadHandle` adapter unifying `Deployment`/`StatefulSet`/`DaemonSet` (the three types share no common Go interface for `.Spec.Template}`, so listing is per-type but detect/patch/apply logic runs once); report-only by default (`"apply": true` required to actually patch, matching the brief's dry-run-first spirit); protected-namespace denylist reused from the isolation feature; `retry.RetryOnConflict` around every `Update` call for safe concurrent patching
- [x] `internal/httpapi/harden_test.go` — handler-level tests using `client-go/kubernetes/fake` (no real cluster needed): dry-run-by-default never mutates the fake object, `apply: true` actually patches it, a protected namespace is rejected with `400`, an already-hardened workload reports `applied: false` and is left alone. **All passing.**
- [x] **Live-verified against the real `kind` cluster**: dry-run against `tenant-a` correctly reported every gap on the unhardened `gateway` Deployment without touching it; `apply: true` genuinely patched the real Deployment's `securityContext` and `resources` via `client-go`.
  - **Real finding during live verification, not just a test**: applying the baseline to `tenant-a/gateway` (image `nicolaka/netshoot:latest`, no non-root `USER` in its Dockerfile) made the new pod fail to start with `container has runAsNonRoot and image has non-numeric user (root), cannot verify user is non-root`. Investigated and treated as a design decision, not a bug to silently patch over — see "Known limitations."
  - Cluster restored afterward via `kubectl rollout undo` + re-`apply` of the original sample manifest, confirmed back to the pre-hardening state.

### Phase 5 — Deployment manifests
- [x] `Dockerfile` — multi-stage build: `golang:1.27-bookworm` compiles a static binary (`CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`), final image is `gcr.io/distroless/static-debian12:nonroot` — no shell, no package manager, and (unlike `nicolaka/netshoot` in the Phase 4 finding above) the base image itself declares a real non-root numeric UID (`65532`), so `runAsNonRoot` is satisfied without needing a forced `runAsUser`
- [x] `manifests/k8s-sentinel/deploy.yaml` — `Namespace` (`k8s-sentinel-system`), `ServiceAccount`, a cluster-scoped `ClusterRole`/`ClusterRoleBinding` whose rules are kept to exactly the verbs the code calls (`networkpolicies`: get/list/create/update/delete; `deployments`/`statefulsets`/`daemonsets`: list/update only — no `patch`, no `delete` on workloads, no `get` on types that are only ever listed; plus a `nonResourceURLs: ["/version"]` get rule for `/readyz`'s `Discovery().ServerVersion()` call), `Deployment`, and `Service`
- [x] The tool's own `Deployment` carries the exact same hardened `securityContext` and resource requests/limits (`100m`/`128Mi` request, `500m`/`512Mi` limit) that `internal/harden/baseline.go` enforces on everything else — deliberately kept identical, not just "similar," so the values can't silently drift apart
- [x] **Live-verified against the real `kind` cluster, not just manifest-validated**: built the image, `kind load docker-image`'d it onto both nodes, applied the full manifest set — pod reached `1/1 Running` with `readOnlyRootFilesystem: true`/`allowPrivilegeEscalation: false`/`capabilities.drop: [ALL]` genuinely in effect, and the readiness probe (which calls `/readyz` → `Discovery().ServerVersion()`) passed, proving the `nonResourceURLs` RBAC rule is both necessary and sufficient. Then, from inside the cluster (exec'd into `tenant-a/gateway`, talking to the real `k8s-sentinel.k8s-sentinel-system.svc.cluster.local` Service, using the tool's own scoped `ServiceAccount` rather than the broad kubeconfig used in earlier phases): ran a real dry-run `/harden` against `tenant-a` (correct gaps reported), created a real `/isolate` between `tenant-a/gateway` and `tenant-b/dashboard` (`200`, got an id), listed it (`GET /isolate` showed it), then deleted it (`204`) and confirmed zero leftover `NetworkPolicy` objects cluster-wide — i.e. the `ClusterRole` is exactly sufficient for every code path that actually runs, with nothing broader granted "just in case."

### Phase 6 — Tests + verification script
- [x] Full `go test ./...` passing across all packages (`internal/harden`: 10, `internal/httpapi`: 4, `internal/netpol`: 9) — fast, cluster-free, safe to run on any machine
- [x] `go test -tags=integration ./internal/netpol/... -run TestIsolation -v` — the one cluster-dependent test, still passing
- [x] `scripts/verify-isolation.sh` — the brief's required standalone "prove it" deliverable: a Go-toolchain-free bash script that port-forwards to the **actually deployed** `k8s-sentinel` Service (Phase 5's Deployment, not a local `go run`), then exercises the exact same five-vector check as `internal/netpol/integration_test.go` (baseline → isolate → blocked-pair + same-ns bystander + cross-ns bystander + DNS + external all correct → restore → confirm zero leftover `NetworkPolicy` objects), with clear `[PASS]`/`[FAIL]` output and a non-zero exit code on any failure
- [x] **Confirmed passing end-to-end against the real cluster** — all 12 assertions pass.
  - Real bug found and fixed while actually running the script (not just reading it): the cleanup trap backgrounded the port-forward via a shell *function* (`kctl ... &`); `$!` after backgrounding a function call captures the wrapper subshell's PID, not `kubectl`'s own PID, so `kill "$PID"` killed the subshell while `kubectl port-forward` itself survived, orphaned (`PPID 1`, reparented to init) and still bound to the local port. Fixed by calling `kubectl` directly (not through the function) for anything whose PID needs to be tracked and killed later, plus a SIGTERM-then-wait-then-SIGKILL fallback in case `kubectl port-forward` doesn't exit promptly on SIGTERM. Re-ran twice afterward to confirm zero leftover processes.
- [x] `scripts/verify-hardening.sh` — the hardening-side counterpart, same standalone bash/no-Go-toolchain style: baseline (no resources/securityContext) → dry-run (gaps reported, nothing mutated) → apply (patched for real, pod genuinely stays healthy and serves traffic — not just a `rollout status` check, see below) → re-apply (already-hardened workload is a true no-op) → protected-namespace rejection. Uses a dedicated fixture, `manifests/samples/hardening-demo.yaml` (`nginxinc/nginx-unprivileged` + an `emptyDir` mounted at `/tmp`), specifically because none of the other sample workloads can be hardened successfully — they all run as root by default and hit the `runAsNonRoot` failure documented above.
  - **Confirmed passing twice in a row** (proving the script is safely re-runnable, not just correct once).
  - Real bug #1: the baseline "nothing set yet" check compared `kubectl`'s `.resources` output to an empty string — but `resources` is a Go struct, not a pointer, so Kubernetes always serializes it as `{}` even when nothing is requested. Fixed by checking for the absence of `requests`/`limits` keys instead of an empty string.
  - Real bug #2, found specifically by running the script *twice*: the cleanup step re-`apply`'d the original manifest to reset the demo workload, but `kubectl apply`'s strategic-merge only removes fields it previously saw recorded in `last-applied-configuration` — since the hardening patch was written via a direct `client-go` `Update()` call (not `kubectl apply`), it was invisible to that mechanism, so the Deployment stayed hardened across runs and the second run found zero gaps. Fixed by deleting and recreating the Deployment in cleanup instead of just re-applying, which guarantees a genuinely fresh object every time.
  - **Real bug #3, the most significant, found during manual CLI testing after Phase 6 was already marked done**: `kubectl rollout status` reporting "successfully rolled out" is **not sufficient proof of health** — it genuinely reported success once while the pod was actually crash-looping underneath (`readOnlyRootFilesystem: true` broke nginx's temp directories: `mkdir() "/tmp/proxy_temp" failed (30: Read-only file system)`), a timing race where the pod was briefly `Available` right before its next crash. This meant the script's earlier "confirmed passing twice in a row" claim had **not actually proven the hardened pod stayed healthy**, only that the Deployment spec got patched correctly. Fixed two things: (1) added an `emptyDir` at `/tmp` to `hardening-demo.yaml` so this fixture can actually survive the baseline (a fixture-only accommodation — the tool itself still provisions no volumes for anyone else it hardens, see "Known limitations"); (2) the script now checks the pod's actual `restartCount` (must be `0`) and genuinely curls the hardened workload's Service after apply, instead of trusting `rollout status` alone. Re-verified: both checks pass cleanly post-fix.

### Phase 7 — Documentation + submission
- [x] **Full command-by-command replication log** — [`REPLICATION.md`](./REPLICATION.md): every terminal command actually run across Phases 0–6, in order, with what each command/file created and why, pulled directly from the session's own tool-call history rather than reconstructed from memory — includes every real bug hit (the `kind-cluster-config.yaml` path mismatch, the `tigerastatus` wait-loop false-positive, two import-path/dependency-pinning mistakes, the `defer`-vs-`t.Cleanup` ordering bug, the `runAsNonRoot`/`runAsUser` finding, and the leaked port-forward process) with the exact commands used to diagnose and fix each one — a quick-start "reproducing this from a clean machine" sequence is at the bottom
- Record actual time spent honestly against the stated 4–6 hour core-task budget
- Disclose AI-assisted development per the brief's explicit requirement, and be ready to explain/defend every part at interview

---

## Known limitations (to be filled in as decisions are finalized)
- No undo for hardening (bonus-list item, not core)
- `Job`/`CronJob`/bare `Pod` workload types not handled
- **No authentication/authorization on the HTTP API itself** — `/isolate` and `/harden` trust anything that can reach the Service, with no caller-identity check in the Go code. Deliberately out of scope for this pass: the focus was proving the isolation/hardening logic itself works, and this is the kind of concern that's normally solved *around* the service rather than inside it — e.g. a `NetworkPolicy` restricting which pods/namespaces can reach `k8s-sentinel-system`, or fronting it with a service mesh/mTLS, rather than hand-rolling auth into the handlers. Noted here rather than silently assumed away.
- **`runAsNonRoot: true` without `runAsUser` can make hardening fail an image outright** — confirmed live against `nicolaka/netshoot:latest`, which has no non-root `USER` directive: kubelet refuses to start the hardened pod with `cannot verify user is non-root`, rather than silently starting it as root. Considered and rejected adding an automatic `runAsUser: 65532` ("nonroot" distroless convention) fallback to the baseline:
  - **Why not force a UID**: it would make `runAsNonRoot` "pass" even on images never built to run as non-root, hiding exactly the thing this feature exists to catch — and a forced UID can itself break images whose files on disk are owned by a different UID, trading one silent failure mode for another.
  - **Why the current behavior is the right default**: it fails loudly and immediately (`CreateContainerConfigError`, visible in `kubectl get pods`/`describe pod`), is caught by the dry-run plan before anyone applies it for real, and matches the report-first philosophy already baked into this tool (`apply: true` is opt-in, never silent). The old pod also keeps serving traffic throughout, since the RollingUpdate default won't tear it down until the replacement passes its health checks.
  - **What this means in practice**: operators should expect that `apply: true` on an image whose base doesn't declare a non-root `USER` will need that fixed at the image level (or the namespace/workload excluded from hardening) — this tool surfaces the gap, it doesn't paper over it.
- **`readOnlyRootFilesystem: true` can crash-loop a workload that needs to write at runtime** — confirmed live against `nginxinc/nginx-unprivileged`, which (independent of the `runAsNonRoot` issue above) fails with `mkdir() "/tmp/proxy_temp" failed (30: Read-only file system)` because nginx needs writable temp/cache directories. The hardening tool does **not** provision `emptyDir` volumes or otherwise try to guess which paths an arbitrary workload needs writable — it has no generic way to know. This is the same category of trade-off as the `runAsNonRoot` finding above: the dry-run plan can't predict this failure mode either (it only inspects the pod spec, not the application's runtime behavior), so `apply: true` on a workload with this issue will crash-loop it, visibly, until an operator either adds the volume themselves or excludes that workload from hardening. `manifests/samples/hardening-demo.yaml` carries its own `emptyDir` at `/tmp` specifically so it can demonstrate the full successful path — that accommodation lives in the fixture, not in the tool.
- *(more to be added as implementation surfaces real constraints)*

