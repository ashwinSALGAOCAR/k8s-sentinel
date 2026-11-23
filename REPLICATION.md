# Replication log

A full, honest, chronological record of every terminal command run and every file created
while building this project - not a cleaned-up "how it should have gone," the actual
sequence, including the real mistakes hit along the way and how they were fixed. If you're
replicating this project from scratch, follow the commands in order; the "bug hit" notes
tell you what to expect if you hit the same thing.

Kept in real time while building this.

---

## Phase 0 — Environment setup

**1. Scaffold the project and initialize the Go module**
```bash
mkdir -p "~/personal projects/k8s-sentinel" && cd "~/personal projects/k8s-sentinel" \
  && go mod init github.com/ashwinSALGAOCAR/k8s-sentinel && ls -la
```
Files created: `go.mod`
Why: Every Go project needs a module before any code can be written or any dependency added.

**2. Check for Docker Desktop, then install it**
```bash
ls /Applications/ | grep -i docker; brew list --cask 2>/dev/null | grep -i docker
brew install --cask docker 2>&1 | tail -30
open -a Docker
docker version   # re-run until it shows a Server: section, not just Client:
```
Files created: none (installs an application).
Why: `kind` runs Kubernetes nodes as Docker containers — it needs the actual daemon running,
not just the CLI client. `docker version` must show a `Server:` block before `kind` will work.

**3. Confirm the Go toolchain is actually on PATH**
```bash
go version
which go; /opt/homebrew/bin/go version 2>&1; echo "PATH=$PATH"
```
Bug hit: the first `go version` produced no output in my shell — the
Homebrew-installed `go` wasn't yet on `PATH`. Confirmed the binary existed at
`/opt/homebrew/bin/go` and that later shells picked it up once `PATH` was sourced correctly.

---

## Phase 1 — Cluster bootstrap

**4. Write the kind cluster config and the setup script, then run it**
```bash
chmod +x scripts/setup-cluster.sh && ./scripts/setup-cluster.sh
```
Files created (via editor, not the command itself): `kind-cluster-config.yaml` (initially at
the project root), `scripts/setup-cluster.sh`.
Why: `scripts/setup-cluster.sh` is the one-shot, idempotent bootstrap — checks
docker/kind/kubectl prerequisites, creates the `kind` cluster with the default CNI disabled,
installs the Tigera Operator + Calico, and waits for everything to go healthy.

**5. Bug hit: config file path mismatch**
```bash
kind get clusters 2>&1; echo "---"; docker ps --filter "name=k8s-sentinel" 2>&1
mv kind-cluster-config.yaml scripts/kind-cluster-config.yaml && ls scripts/
```
The script expects `kind-cluster-config.yaml` to live in `scripts/` (next to the script
itself), but it was created at the project root. Fixed by moving the file, not by changing
the script's path logic.

**6. Re-run the setup script — second bug hit: false "ready" on empty status**
```bash
./scripts/setup-cluster.sh
kubectl --context kind-k8s-sentinel get tigerastatus; echo "---pods---"
kubectl --context kind-k8s-sentinel get pods -n calico-system -o wide 2>&1
kubectl --context kind-k8s-sentinel get nodes -o wide
kubectl --context kind-k8s-sentinel get tigerastatus   # re-checked after a short wait
```
The script's wait loop used `grep -qv True` to detect "not all components ready" — this
returns a false "success" the instant `tigerastatus` output is *empty* (operator hasn't
created any status objects yet), not just when every row is genuinely `True`. Fixed in the
script to require at least one non-empty status line before considering the wait satisfied.
Confirmed afterward that every Calico component (including `goldmane`, the slowest to
settle) genuinely reached `AVAILABLE: True`.

**7. Create and apply the sample workloads, confirm baseline connectivity**
```bash
kubectl --context kind-k8s-sentinel apply -f manifests/samples/ \
  && kubectl --context kind-k8s-sentinel -n tenant-a rollout status deployment/gateway --timeout=90s \
  && kubectl --context kind-k8s-sentinel -n tenant-b rollout status deployment/dashboard --timeout=90s \
  && kubectl --context kind-k8s-sentinel -n tenant-b rollout status deployment/widget --timeout=90s \
  && kubectl --context kind-k8s-sentinel -n tenant-c rollout status deployment/widget --timeout=90s

GW_POD=$(kubectl --context kind-k8s-sentinel -n tenant-a get pod -l app=gateway -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 dashboard.tenant-b.svc.cluster.local
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 widget.tenant-b.svc.cluster.local
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 widget.tenant-c.svc.cluster.local
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 https://example.com
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- nslookup dashboard.tenant-b.svc.cluster.local
```
Files created (via editor): `manifests/samples/tenant-a.yaml`, `tenant-b.yaml`, `tenant-c.yaml`.
Why: establishes the five-vector connectivity baseline (blocked target, same-namespace
bystander, cross-namespace bystander, external egress, DNS) that every later isolation test
diffs against.

---

## Phase 2 — Service skeleton

**8. Pull in client-go and write the skeleton**
```bash
go mod init github.com/ashwinSALGAOCAR/k8s-sentinel && \
  go get k8s.io/client-go@v0.33.0 k8s.io/api@v0.33.0 k8s.io/apimachinery@v0.33.0 2>&1 | tail -20
go mod tidy 2>&1 | tail -20 && go build ./... 2>&1 && go vet ./... 2>&1 && echo "BUILD OK"
```
Files created (via editor): `internal/k8sclient/client.go`, `internal/httpapi/server.go`,
`cmd/server/main.go`.
Why: `internal/k8sclient` builds a clientset (in-cluster first, kubeconfig fallback);
`internal/httpapi` is the `net/http` server skeleton with `/healthz`+`/readyz`; `cmd/server`
is the entrypoint with graceful shutdown.

**9. Live-verify against the real cluster**
```bash
go build -o /tmp/k8s-sentinel-server ./cmd/server && kubectl config use-context kind-k8s-sentinel >/dev/null
/tmp/k8s-sentinel-server > /tmp/server.log 2>&1 &
curl -s -w "\nHTTP %{http_code}\n" http://localhost:8080/healthz
curl -s -w "\nHTTP %{http_code}\n" http://localhost:8080/readyz
kill -TERM $!   # or the backgrounded PID
```
Confirmed `/healthz`/`/readyz` both return `200` against the real cluster, and `SIGTERM`
produces a clean shutdown log sequence (`shutdown signal received` → `shutdown complete`).

---

## Phase 3 — Network isolation

**10. Write the generator + unit tests, fix two import-path bugs**
```bash
go build ./... 2>&1 && go vet ./... 2>&1 && go test ./... -v 2>&1
go mod tidy 2>&1 | tail -10 && go build ./... && go vet ./... && go test ./... -v
go get k8s.io/apimachinery@v0.33.0 k8s.io/api@v0.33.0 2>&1 | tail -10 && go mod tidy 2>&1 | tail -10 \
  && go build ./... && go vet ./... && go test ./... -v
```
Files created (via editor): `internal/netpol/generator.go`, `internal/netpol/generator_test.go`.
Bugs hit: wrong import path `k8s.io/apimachinery/pkg/apis/meta/v1/intstr` (correct is
`k8s.io/apimachinery/pkg/util/intstr`), which caused `go mod tidy` to auto-upgrade
`client-go`/`api`/`apimachinery` to `v0.37.2` while trying to resolve it — fixed the import,
then explicitly re-pinned all three back to `v0.33.0` to match `client-go`.

**11. Write the HTTP handlers, build/vet/test**
```bash
go mod tidy 2>&1 | tail -10 && go build ./... && go vet ./... && go test ./...
```
Files created (via editor): `internal/httpapi/isolate.go`.

**12. Live isolate/restore smoke test against the real cluster**
```bash
go build -o /tmp/k8s-sentinel-server ./cmd/server && kubectl config use-context kind-k8s-sentinel >/dev/null
/tmp/k8s-sentinel-server > /tmp/server.log 2>&1 &
curl -s -w "\nHTTP %{http_code}\n" http://localhost:8080/readyz

curl -s -X POST http://localhost:8080/isolate -H "Content-Type: application/json" \
  -d '{"a":{"namespace":"tenant-a","selector":{"app":"gateway"}},"b":{"namespace":"tenant-b","selector":{"app":"dashboard"}}}' \
  -w "\nHTTP %{http_code}\n"

sleep 3
GW_POD=$(kubectl --context kind-k8s-sentinel -n tenant-a get pod -l app=gateway -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 dashboard.tenant-b.svc.cluster.local   # blocked
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 widget.tenant-b.svc.cluster.local     # still works
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 widget.tenant-c.svc.cluster.local     # still works
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 https://example.com                  # still works
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- nslookup dashboard.tenant-b.svc.cluster.local                                                   # still resolves

curl -s -X DELETE http://localhost:8080/isolate/<id> -w "\nHTTP %{http_code}\n"
kubectl --context kind-k8s-sentinel get networkpolicy -A
curl -s http://localhost:8080/isolate -w "\nHTTP %{http_code}\n"
```
Proved manually, by hand, before any automated test existed: isolating `tenant-a/gateway`
↔`tenant-b/dashboard` blocks only that pair and restores cleanly on delete.

**13. Write and run the automated integration test**
```bash
go build -tags=integration ./... && go vet -tags=integration ./... && echo "BUILD/VET OK" \
  && kubectl config use-context kind-k8s-sentinel >/dev/null \
  && go test -tags=integration ./internal/netpol/... -run TestIsolation -v
```
Files created (via editor): `internal/netpol/integration_test.go`.
Bug hit: `defer srv.Close()` ran before a `t.Cleanup`-registered safety-net isolation-delete
callback (Go runs `defer` before any `t.Cleanup` func), closing the test server before the
cleanup call could reach it. Fixed by registering the server's own close via `t.Cleanup`
too, ordered (LIFO) to run *after* the isolation cleanup. Re-ran and confirmed clean:
```bash
kubectl --context kind-k8s-sentinel get networkpolicy -A
go vet -tags=integration ./... && go test -tags=integration ./internal/netpol/... -run TestIsolation -v
go test ./... -v | tail -20   # confirm the default (non-integration) suite is still fast/cluster-free
gofmt -l .
```

---

## Phase 4 — Workload hardening

**14. Write the pure logic layer, build/vet**
```bash
gofmt -l . && gofmt -w . && go build ./... && go vet ./... && echo "BUILD/VET OK"
```
Files created (via editor): `internal/harden/baseline.go`, `internal/harden/detector.go`,
`internal/harden/patcher.go`.

**15. Write and run the unit tests**
```bash
gofmt -w . && go build ./... && go vet ./... && go test ./internal/harden/... -v
```
Files created (via editor): `internal/harden/detector_test.go` (10 tests, all passing).

**16. Write the HTTP layer and its handler-level tests**
```bash
gofmt -w . && go mod tidy 2>&1 | tail -10 && go build ./... && go vet ./... && go test ./...
gofmt -w . && go mod tidy 2>&1 | tail -10 && go build ./... && go vet ./... && go test ./... -v
```
Files created (via editor): `internal/httpapi/harden.go`, `internal/httpapi/harden_test.go`
(4 tests using `client-go/kubernetes/fake`, no real cluster needed).

**17. Live dry-run and apply against the real cluster — found a real design-decision bug**
```bash
go build -o /tmp/k8s-sentinel-server ./cmd/server && kubectl config use-context kind-k8s-sentinel >/dev/null
/tmp/k8s-sentinel-server > /tmp/server.log 2>&1 &
curl -s -X POST http://localhost:8080/harden -H "Content-Type: application/json" -d '{"namespaces":["tenant-a"]}' | python3 -m json.tool

kubectl --context kind-k8s-sentinel -n tenant-a get deployment gateway -o jsonpath='{.spec.template.spec.containers[0].securityContext}'
curl -s -X POST http://localhost:8080/harden -H "Content-Type: application/json" -d '{"namespaces":["tenant-a"],"apply":true}' | python3 -m json.tool
kubectl --context kind-k8s-sentinel -n tenant-a rollout status deployment/gateway --timeout=60s
kubectl --context kind-k8s-sentinel -n tenant-a get deployment gateway -o jsonpath='{.spec.template.spec.containers[0].securityContext}'
kubectl --context kind-k8s-sentinel -n tenant-a get deployment gateway -o jsonpath='{.spec.template.spec.containers[0].resources}'
kubectl --context kind-k8s-sentinel -n tenant-a get pods
```
Bug hit (a real, interview-defensible finding, not papered over): applying the baseline to
`tenant-a/gateway` (image `nicolaka/netshoot`, no non-root `USER` in its Dockerfile) made the
new pod fail with `container has runAsNonRoot and image has non-numeric user (root), cannot
verify user is non-root`:
```bash
kubectl --context kind-k8s-sentinel -n tenant-a describe pod gateway-7c886f56d4-fhd5b | tail -20
kubectl --context kind-k8s-sentinel -n tenant-a get deployment gateway -o jsonpath='{.spec.template.spec.containers[0].image}'
```
Decision made (documented in `internal/harden/baseline.go` and the README's "Known
limitations"): keep `runAsNonRoot: true` without forcing a `runAsUser`, since failing loudly
is more honest than silently forcing a UID that could mask a real problem or break an image
whose files are owned by a different UID. Restored the cluster:
```bash
kill %1 2>/dev/null; kubectl --context kind-k8s-sentinel -n tenant-a rollout undo deployment/gateway
kubectl --context kind-k8s-sentinel -n tenant-a rollout status deployment/gateway --timeout=60s
kubectl --context kind-k8s-sentinel apply -f manifests/samples/tenant-a.yaml
gofmt -l . && go build ./... && go vet ./... && go test ./...
```

---

## Phase 5 — Deployment manifests

**18. Write the Dockerfile and the tool's own manifests**
Files created (via editor): `Dockerfile`, `manifests/k8s-sentinel/deploy.yaml` (Namespace,
ServiceAccount, ClusterRole/ClusterRoleBinding, Deployment, Service).

**19. Build the image, load it into kind, deploy it, and live-verify it for real**
```bash
docker build -t k8s-sentinel:dev . 2>&1 | tail -30
kind load docker-image k8s-sentinel:dev --name k8s-sentinel \
  && kubectl --context kind-k8s-sentinel apply -f manifests/k8s-sentinel/deploy.yaml

kubectl --context kind-k8s-sentinel -n k8s-sentinel-system rollout status deployment/k8s-sentinel --timeout=60s
kubectl --context kind-k8s-sentinel -n k8s-sentinel-system get pods -o wide
kubectl --context kind-k8s-sentinel -n k8s-sentinel-system get pod -l app=k8s-sentinel -o jsonpath='{.items[0].spec.containers[0].securityContext}'
kubectl --context kind-k8s-sentinel -n k8s-sentinel-system logs -l app=k8s-sentinel --tail=20
```
Confirmed the pod reaches `1/1 Running` under the real hardened `securityContext`, and the
readiness probe (which calls `Discovery().ServerVersion()`) passing proved the
`nonResourceURLs: ["/version"]` RBAC rule is correct.

**20. Prove the ClusterRole is actually sufficient, from inside the cluster**
```bash
SVC=k8s-sentinel.k8s-sentinel-system.svc.cluster.local
kubectl --context kind-k8s-sentinel -n tenant-a exec deploy/gateway -- curl -s -X POST http://$SVC:8080/harden -H "Content-Type: application/json" -d '{"namespaces":["tenant-a"]}'
kubectl --context kind-k8s-sentinel -n tenant-a exec deploy/gateway -- curl -s -X POST http://$SVC:8080/isolate -H "Content-Type: application/json" -d '{"a":{"namespace":"tenant-a","selector":{"app":"gateway"}},"b":{"namespace":"tenant-b","selector":{"app":"dashboard"}}}'
kubectl --context kind-k8s-sentinel -n tenant-a exec deploy/gateway -- curl -s http://$SVC:8080/isolate

kubectl --context kind-k8s-sentinel -n tenant-a exec deploy/gateway -- curl -s -X DELETE http://$SVC:8080/isolate/<id> -w "\nstatus=%{http_code}\n"
kubectl --context kind-k8s-sentinel get networkpolicies -A
```
Confirmed real `/harden` and `/isolate` calls succeed through the actual in-cluster Service,
using the tool's own scoped ServiceAccount (not the broad local kubeconfig used in earlier
phases) — i.e. the RBAC is exactly sufficient, nothing broader granted "just in case."

---

## Phase 6 — Tests + verification script

**21. Write and run the standalone bash verification script**
```bash
chmod +x scripts/verify-isolation.sh && gofmt -l . && go build ./... && go vet ./... && go test ./...
./scripts/verify-isolation.sh
```
Files created (via editor): `scripts/verify-isolation.sh`.
Why: a Go-toolchain-free proof, runnable by anyone, that isolation blocks only the intended
pair and restores cleanly — port-forwards to the real deployed Service and runs the same
five-vector check as the integration test.

**22. Bug hit: leaked port-forward process, found and fixed by actually running the script twice**
```bash
pgrep -f "port-forward.*k8s-sentinel" || echo "no leftover port-forward processes"
ps -p <pid> -o pid,ppid,command 2>/dev/null
pgrep -fl "port-forward.*k8s-sentinel"; kill -9 <pid> 2>/dev/null || true; sleep 1; pgrep -fl "port-forward.*k8s-sentinel" || echo "cleaned up"

./scripts/verify-isolation.sh; echo "exit=$?"; sleep 1; pgrep -fl "port-forward.*k8s-sentinel" || echo "no leftover port-forward process"
ps -p <pid> -o pid,ppid,pgid,command
kill -9 <pid> 2>/dev/null; sleep 1; pgrep -fl "port-forward.*k8s-sentinel" || echo cleaned

pkill -9 -f "port-forward.*k8s-sentinel" 2>/dev/null; sleep 1
./scripts/verify-isolation.sh; echo "exit=$?"; sleep 1; pgrep -fl "port-forward.*k8s-sentinel" || echo "no leftover port-forward process"
```
Root cause: the script backgrounded `kubectl port-forward` via a shell *function* wrapper
(`kctl ... &`) — `$!` after backgrounding a function call captures the wrapper subshell's
PID, not `kubectl`'s own PID, so `kill "$PID"` killed the subshell while `kubectl` itself
survived, orphaned (`PPID 1`). Fixed by calling `kubectl` directly (not through the
function) for the one call whose PID needs to be tracked, plus a SIGTERM-then-wait-then-
SIGKILL fallback for cases where `kubectl port-forward` doesn't exit promptly. Re-ran twice
afterward and confirmed zero leftover processes both times.

**23. Added the hardening-side counterpart script, found two more real bugs by actually running it twice**
```bash
chmod +x scripts/verify-hardening.sh
kubectl --context kind-k8s-sentinel apply -f manifests/samples/hardening-demo.yaml
kubectl --context kind-k8s-sentinel -n hardening-demo rollout status deployment/app --timeout=60s
./scripts/verify-hardening.sh; echo "exit=$?"
```
Files created: `manifests/samples/hardening-demo.yaml` (a dedicated, deliberately unhardened
`nginxinc/nginx-unprivileged` workload — plain `nginx` and every other existing sample image
run as root by default, which would hit the exact `runAsNonRoot` failure from Phase 4 instead
of proving the full apply-and-stay-healthy path), `scripts/verify-hardening.sh`.

Bug hit #1: the "baseline is unhardened" check tested `kubectl`'s `.resources` output for an
empty string, but `resources` is a non-pointer struct — Kubernetes always prints `{}}` even
when nothing is set. Fixed by checking for the absence of `requests`/`limits` keys instead.

Bug hit #2, found only by re-running the script a second time: the cleanup step re-`apply`'d
the original manifest to reset the demo workload, but `kubectl apply`'s 3-way strategic merge
only strips fields it previously recorded in `last-applied-configuration` — the hardening
patch was written via a direct `client-go` `Update()`, which that annotation never saw, so
the Deployment stayed hardened across runs and the second run found zero gaps. Fixed by
deleting and recreating the Deployment in cleanup instead of relying on `apply` alone:
```bash
kubectl --context kind-k8s-sentinel delete deployment app -n hardening-demo --ignore-not-found \
  && kubectl --context kind-k8s-sentinel apply -f manifests/samples/hardening-demo.yaml \
  && kubectl --context kind-k8s-sentinel -n hardening-demo rollout status deployment/app --timeout=60s
./scripts/verify-hardening.sh; echo "exit=$?"
./scripts/verify-hardening.sh; echo "exit=$?"   # run twice in a row to prove repeatability
```
Confirmed both runs pass cleanly with zero leftover port-forward processes.

**24. Manual CLI testing (after Phase 6 was already marked done) surfaced a real bug the script itself was missing**
```bash
kubectl --context kind-k8s-sentinel -n k8s-sentinel-system port-forward svc/k8s-sentinel 18080:8080
curl -s -X POST http://localhost:18080/harden -H "Content-Type: application/json" -d '{"namespaces":["hardening-demo"],"apply":true}' | python3 -m json.tool
kubectl --context kind-k8s-sentinel -n hardening-demo rollout status deployment/app --timeout=60s
kubectl --context kind-k8s-sentinel -n hardening-demo get pods
kubectl --context kind-k8s-sentinel -n hardening-demo describe pod <crashing-pod-name> | tail -20
kubectl --context kind-k8s-sentinel -n hardening-demo logs <crashing-pod-name> --previous
```
`rollout status` reported "successfully rolled out," but `kubectl get pods` showed the pod
actually `0/1 Error`, restarting repeatedly. The logs confirmed the real cause:
```
mkdir() "/tmp/proxy_temp" failed (30: Read-only file system)
```
`readOnlyRootFilesystem: true` (part of the hardening baseline) broke nginx's temp/cache
directories — a different failure mode than the `runAsNonRoot` finding in Phase 4, and one
`scripts/verify-hardening.sh` had been silently missing: its "rollout succeeded" check was
trusting `kubectl rollout status`, which reported success on a timing race (the pod was
briefly `Available` right before crashing again). This meant the earlier "confirmed passing
twice in a row" claim had **not** actually proven the hardened pod stayed healthy.

Fixed two things:
```bash
kubectl --context kind-k8s-sentinel delete deployment app -n hardening-demo --ignore-not-found \
  && kubectl --context kind-k8s-sentinel apply -f manifests/samples/hardening-demo.yaml \
  && kubectl --context kind-k8s-sentinel -n hardening-demo rollout status deployment/app --timeout=60s
```
1. Added an `emptyDir` volume mounted at `/tmp` to `manifests/samples/hardening-demo.yaml`
   — a fixture-only accommodation so this demo can survive the baseline; the tool itself
   still provisions no volumes for anyone else it hardens (documented as a known limitation,
   not "fixed" generically, since the tool has no way to know an arbitrary app's writable-
   path needs).
2. `scripts/verify-hardening.sh` now checks the pod's actual `restartCount` (must be `0`) and
   genuinely curls the hardened workload's Service after apply, instead of trusting
   `rollout status` alone:
```bash
pkill -9 -f "port-forward.*k8s-sentinel" 2>/dev/null
./scripts/verify-hardening.sh; echo "exit=$?"
./scripts/verify-hardening.sh; echo "exit=$?"
```
Re-verified clean (the first of the two runs above failed only on stale state left over from
the manual testing immediately before it — its own cleanup step reset that, and the second
run passed all 12 checks, including the two new ones).

---

## Reproducing this from a clean machine

The commands alone aren't enough — most of them only work because a specific file was
written first. Below is the same path as above, but with every file creation placed in the
exact order it actually happened, so you know what must exist before each command will do
anything useful. "create:" = written with an editor, not produced by the command that follows.

```bash
# --- Phase 0: environment ---
# macOS + Homebrew assumed. If Homebrew itself isn't installed:
#   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
brew --version   # confirm Homebrew itself is present

brew install go                       # installs the Go toolchain
go version                            # verify

brew install --cask docker && open -a Docker
docker version                        # must show a Server: section, not just Client: — re-run until the daemon is up

brew install kind
kind version                          # verify

brew install kubectl
kubectl version --client              # verify

# NOTE: Calico is NOT installed here. scripts/setup-cluster.sh (Phase 1 below)
# installs it automatically via the Tigera Operator — kind's default CNI
# (kindnet) does not enforce NetworkPolicy at all, so Calico is a hard
# requirement for this project to work, just not a manual prerequisite step.

mkdir -p "k8s-sentinel" && cd "k8s-sentinel"
go mod init github.com/ashwinSALGAOCAR/k8s-sentinel        # create: go.mod

# --- Phase 1: cluster bootstrap ---
# create: scripts/kind-cluster-config.yaml
# create: scripts/setup-cluster.sh
chmod +x scripts/setup-cluster.sh && ./scripts/setup-cluster.sh
# create: manifests/samples/tenant-a.yaml
# create: manifests/samples/tenant-b.yaml
# create: manifests/samples/tenant-c.yaml
kubectl --context kind-k8s-sentinel apply -f manifests/samples/
kubectl --context kind-k8s-sentinel -n tenant-a rollout status deployment/gateway --timeout=90s

# --- Phase 2: service skeleton ---
go get k8s.io/client-go@v0.33.0 k8s.io/api@v0.33.0 k8s.io/apimachinery@v0.33.0   # updates go.mod/go.sum
# create: internal/k8sclient/client.go
# create: internal/httpapi/server.go
# create: cmd/server/main.go
go build ./... && go vet ./...

# --- Phase 3: network isolation ---
# create: internal/netpol/generator.go
# create: internal/netpol/generator_test.go
go test ./internal/netpol/... -v
# create: internal/httpapi/isolate.go
go build ./... && go test ./...
# create: internal/netpol/integration_test.go
go test -tags=integration ./internal/netpol/... -run TestIsolation -v

# --- Phase 4: workload hardening ---
# create: internal/harden/baseline.go
# create: internal/harden/detector.go
# create: internal/harden/patcher.go
go build ./... && go vet ./...
# create: internal/harden/detector_test.go
go test ./internal/harden/... -v
# create: internal/httpapi/harden.go
# create: internal/httpapi/harden_test.go
go build ./... && go test ./...

# --- Phase 5: deployment manifests ---
# create: Dockerfile
# create: manifests/k8s-sentinel/deploy.yaml
docker build -t k8s-sentinel:dev .
kind load docker-image k8s-sentinel:dev --name k8s-sentinel
kubectl --context kind-k8s-sentinel apply -f manifests/k8s-sentinel/deploy.yaml
kubectl --context kind-k8s-sentinel -n k8s-sentinel-system rollout status deployment/k8s-sentinel --timeout=60s

# --- Phase 6: verification ---
# create: scripts/verify-isolation.sh
chmod +x scripts/verify-isolation.sh && ./scripts/verify-isolation.sh
# create: manifests/samples/hardening-demo.yaml
# create: scripts/verify-hardening.sh
kubectl --context kind-k8s-sentinel apply -f manifests/samples/hardening-demo.yaml
chmod +x scripts/verify-hardening.sh && ./scripts/verify-hardening.sh
```

---

## Manual testing via CLI

The two verification scripts (`scripts/verify-isolation.sh`, `scripts/verify-hardening.sh`)
automate all of this, but here's the same thing broken into individual commands — useful
for poking at the running service by hand, or for the interview walkthrough.

**0. Port-forward to the real deployed service** (run first, leave it running in its own terminal):
```bash
kubectl --context kind-k8s-sentinel -n k8s-sentinel-system port-forward svc/k8s-sentinel 18080:8080
```

### Isolation — `GET` / `POST` / `DELETE` `/isolate`

**1. List active isolations** (expect `[]` if nothing is isolated yet):
```bash
curl -s http://localhost:18080/isolate | python3 -m json.tool
```

**2. Create one** — `tenant-a/gateway` ↔ `tenant-b/dashboard`:
```bash
curl -s -X POST http://localhost:18080/isolate \
  -H "Content-Type: application/json" \
  -d '{"a":{"namespace":"tenant-a","selector":{"app":"gateway"}},"b":{"namespace":"tenant-b","selector":{"app":"dashboard"}}}' \
  | python3 -m json.tool
```
Note the `"id"` in the response — needed for the delete below.

**3. List again** — confirms the entry now shows up:
```bash
curl -s http://localhost:18080/isolate | python3 -m json.tool
```

**4. Restore** (substitute the real id from step 2):
```bash
curl -s -X DELETE http://localhost:18080/isolate/<id> -w "\nHTTP %{http_code}\n"
```
Expect `204`. A follow-up `GET /isolate` should be back to `[]`.

### Proving the rule actually blocks traffic (and only the intended pair)

Run these from inside `tenant-a/gateway` *while* the isolation from step 2 above is active:

**5. Find the gateway pod:**
```bash
GW_POD=$(kubectl --context kind-k8s-sentinel -n tenant-a get pod -l app=gateway -o jsonpath='{.items[0].metadata.name}')
echo "$GW_POD"
```

**6. The isolated target — should be BLOCKED:**
```bash
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- \
  curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 dashboard.tenant-b.svc.cluster.local
```
Expect it to hang until the 5s timeout and print `HTTP 000` — not `200`.

**7. Same-namespace bystander — should still WORK** (proves the block is scoped to
`app: dashboard`, not all of `tenant-b`):
```bash
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- \
  curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 widget.tenant-b.svc.cluster.local
```

**8. Cross-namespace bystander — should still WORK:**
```bash
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- \
  curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 widget.tenant-c.svc.cluster.local
```

**9. External egress — should still WORK:**
```bash
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- \
  curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 https://example.com
```

**10. DNS resolution — should still WORK**, even for the blocked hostname (only the actual
connection is blocked, not the lookup):
```bash
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- \
  nslookup dashboard.tenant-b.svc.cluster.local
```

**11. Inspect the actual `NetworkPolicy` objects the request created:**
```bash
kubectl --context kind-k8s-sentinel get networkpolicy -A
kubectl --context kind-k8s-sentinel -n tenant-a get networkpolicy -o yaml
kubectl --context kind-k8s-sentinel -n tenant-b get networkpolicy -o yaml
```

If #6 is blocked and #7–#10 all succeed, the rule is working exactly as designed: it blocks
only the intended pair and preserves everything else.

### Hardening — dry-run / apply / verify against `hardening-demo`

Uses the same port-forward from step 0 above.

**12. Baseline — confirm the demo workload currently has no resources/securityContext:**
```bash
kubectl --context kind-k8s-sentinel -n hardening-demo get deployment app -o jsonpath='{.spec.template.spec.containers[0].securityContext}'
kubectl --context kind-k8s-sentinel -n hardening-demo get deployment app -o jsonpath='{.spec.template.spec.containers[0].resources}'
```
Expect empty/`{}` for both.

**13. Dry-run (`apply` omitted, defaults to `false`) — should report gaps, mutate nothing:**
```bash
curl -s -X POST http://localhost:18080/harden \
  -H "Content-Type: application/json" \
  -d '{"namespaces":["hardening-demo"]}' \
  | python3 -m json.tool
```

**14. Confirm step 13 really didn't mutate anything:**
```bash
kubectl --context kind-k8s-sentinel -n hardening-demo get deployment app -o jsonpath='{.spec.template.spec.containers[0].securityContext}'
```
Expect still empty.

**15. Apply for real:**
```bash
curl -s -X POST http://localhost:18080/harden \
  -H "Content-Type: application/json" \
  -d '{"namespaces":["hardening-demo"],"apply":true}' \
  | python3 -m json.tool
```

**16. Confirm the pod is genuinely healthy — not just `rollout status`** (see Phase 6's
Real bug #3: `rollout status` alone can report success on a timing race while the pod is
actually crash-looping):
```bash
kubectl --context kind-k8s-sentinel -n hardening-demo rollout status deployment/app --timeout=60s
sleep 5
kubectl --context kind-k8s-sentinel -n hardening-demo get pods
```
Expect `1/1 Running`, `0` restarts.

**17. Confirm the patched fields, and that it actually serves traffic:**
```bash
kubectl --context kind-k8s-sentinel -n hardening-demo get deployment app -o jsonpath='{.spec.template.spec.containers[0].securityContext}'
kubectl --context kind-k8s-sentinel -n hardening-demo get deployment app -o jsonpath='{.spec.template.spec.containers[0].resources}'
GW_POD=$(kubectl --context kind-k8s-sentinel -n tenant-a get pod -l app=gateway -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-k8s-sentinel -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w "HTTP %{http_code}\n" --max-time 5 app.hardening-demo.svc.cluster.local:8080
```
Expect `HTTP 200`.

**18. Re-apply — should be a true no-op (`applied: false`) since it's already hardened:**
```bash
curl -s -X POST http://localhost:18080/harden \
  -H "Content-Type: application/json" \
  -d '{"namespaces":["hardening-demo"],"apply":true}' \
  | python3 -m json.tool
```

**19. Protected namespace rejection:**
```bash
curl -s -o /dev/null -w "HTTP %{http_code}\n" -X POST http://localhost:18080/harden \
  -H "Content-Type: application/json" \
  -d '{"namespaces":["kube-system"]}'
```
Expect `400`.
