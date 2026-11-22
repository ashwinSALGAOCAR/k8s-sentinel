#!/usr/bin/env bash
# Standalone, Go-toolchain-free proof that hardening genuinely detects gaps,
# applies only when asked, patches correctly, and is idempotent — the
# hardening-side counterpart to scripts/verify-isolation.sh. Runs against
# the already-deployed service (see manifests/k8s-sentinel/deploy.yaml).
#
# Usage: scripts/verify-hardening.sh
# Requires: kind-k8s-sentinel cluster up, manifests/samples/ (hardening-demo.yaml
# AND tenant-a.yaml — gateway is used as the curl client for the post-apply
# traffic check) and manifests/k8s-sentinel/deploy.yaml applied, kubectl on PATH.
set -euo pipefail

CONTEXT="kind-k8s-sentinel"
NAMESPACE="k8s-sentinel-system"
SERVICE="k8s-sentinel"
TARGET_NAMESPACE="hardening-demo"
TARGET_DEPLOYMENT="app"
LOCAL_PORT=18081
FAILURES=0

log()  { echo "==> $*"; }
pass() { echo "  [PASS] $*"; }
fail() { echo "  [FAIL] $*"; FAILURES=$((FAILURES + 1)); }

kctl() { kubectl --context "$CONTEXT" "$@"; }

deployment_security_context() {
	kctl -n "$TARGET_NAMESPACE" get deployment "$TARGET_DEPLOYMENT" \
		-o jsonpath='{.spec.template.spec.containers[0].securityContext}' 2>/dev/null
}

deployment_resources() {
	kctl -n "$TARGET_NAMESPACE" get deployment "$TARGET_DEPLOYMENT" \
		-o jsonpath='{.spec.template.spec.containers[0].resources}' 2>/dev/null
}

post_harden() {
	local apply="$1"
	curl -s -X POST "http://localhost:${LOCAL_PORT}/harden" \
		-H "Content-Type: application/json" \
		-d "{\"namespaces\":[\"${TARGET_NAMESPACE}\"],\"apply\":${apply}}"
}

PORT_FORWARD_PID=""

cleanup() {
	# `kubectl apply` alone won't reset this: strategic merge only removes
	# fields it previously saw in last-applied-configuration, and the
	# hardening patch was applied via a direct client-go Update (not
	# `apply`), so it's invisible to that mechanism — plain re-apply left
	# the Deployment hardened across repeated runs (found by actually
	# re-running this script twice). Delete-then-recreate guarantees a
	# genuinely fresh, unhardened object every time.
	kctl delete deployment "$TARGET_DEPLOYMENT" -n "$TARGET_NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
	kctl apply -f manifests/samples/hardening-demo.yaml >/dev/null 2>&1 || true

	if [[ -n "$PORT_FORWARD_PID" ]] && kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then
		kill "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
		for _ in $(seq 1 10); do
			kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1 || break
			sleep 0.2
		done
		kill -9 "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

log "Port-forwarding svc/${SERVICE} in ${NAMESPACE}..."
# Call kubectl directly (not through a function) so $! is kubectl's own PID —
# see scripts/verify-isolation.sh's cleanup() comment for why that matters.
kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward "svc/${SERVICE}" "${LOCAL_PORT}:8080" >/tmp/k8s-sentinel-harden-port-forward.log 2>&1 &
PORT_FORWARD_PID=$!

for _ in $(seq 1 20); do
	if curl -s -o /dev/null "http://localhost:${LOCAL_PORT}/healthz"; then break; fi
	sleep 0.5
done

log "1. Baseline: the demo Deployment has no resources/securityContext yet"
# resources is a struct, not a pointer — kubectl always prints "{}" even
# when nothing is set, so check for the absence of requests/limits keys
# rather than an empty string.
BASELINE_RESOURCES="$(deployment_resources)"
if [[ -z "$(deployment_security_context)" ]] && ! echo "$BASELINE_RESOURCES" | grep -q '"requests"\|"limits"'; then
	pass "baseline: no securityContext or resources set"
else
	fail "baseline: expected the demo Deployment to start unhardened"
fi

log "2. Dry-run (apply omitted): reports gaps, mutates nothing"
DRYRUN_RESPONSE="$(post_harden false)"
if echo "$DRYRUN_RESPONSE" | grep -q '"MissingRequests":true' && echo "$DRYRUN_RESPONSE" | grep -q '"MissingLimits":true'; then
	pass "dry-run correctly reports missing resources"
else
	fail "dry-run did not report missing resources: $DRYRUN_RESPONSE"
fi
if echo "$DRYRUN_RESPONSE" | grep -q '"applied":false'; then
	pass "dry-run reports applied:false"
else
	fail "dry-run did not report applied:false: $DRYRUN_RESPONSE"
fi
if [[ -z "$(deployment_security_context)" ]]; then
	pass "dry-run did not actually mutate the Deployment"
else
	fail "dry-run mutated the Deployment — it must not"
fi

log "3. Apply: patches the Deployment for real"
APPLY_RESPONSE="$(post_harden true)"
if echo "$APPLY_RESPONSE" | grep -q '"applied":true'; then
	pass "apply reports applied:true"
else
	fail "apply did not report applied:true: $APPLY_RESPONSE"
fi

log "   (waiting for the rollout — this image declares a real non-root user, so it should succeed)"
kctl -n "$TARGET_NAMESPACE" rollout status "deployment/${TARGET_DEPLOYMENT}" --timeout=60s >/dev/null 2>&1 || true

# `rollout status` alone is NOT sufficient proof of health: it reported
# "successfully rolled out" once for real while the pod was actually
# crash-looping underneath (readOnlyRootFilesystem broke nginx's temp dirs
# — a timing race where the pod was briefly Available right before its
# next crash). Give a crash-looper time to actually crash again, then
# check restart count and genuinely curl the workload, not just its spec.
sleep 5
RESTARTS="$(kctl -n "$TARGET_NAMESPACE" get pods -l app=hardening-demo \
	-o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null || echo "?")"
if [[ "$RESTARTS" == "0" ]]; then
	pass "pod has zero restarts after hardening (not crash-looping)"
else
	fail "pod has restarted ${RESTARTS} time(s) after hardening — likely crash-looping"
fi

GW_POD="$(kctl -n tenant-a get pod -l app=gateway -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
APP_CODE="$(kctl -n tenant-a exec "$GW_POD" -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
	"${TARGET_DEPLOYMENT}.${TARGET_NAMESPACE}.svc.cluster.local:8080" 2>/dev/null || true)"
if [[ "$APP_CODE" == "200" ]]; then
	pass "hardened workload actually serves traffic (HTTP 200)"
else
	fail "hardened workload does not serve traffic after apply, got '${APP_CODE}'"
fi

SC="$(deployment_security_context)"
if echo "$SC" | grep -q '"runAsNonRoot":true' && echo "$SC" | grep -q '"readOnlyRootFilesystem":true' && echo "$SC" | grep -q '"drop":\["ALL"\]'; then
	pass "patched securityContext matches the baseline"
else
	fail "patched securityContext missing expected fields: $SC"
fi
RES="$(deployment_resources)"
if echo "$RES" | grep -q '"requests"' && echo "$RES" | grep -q '"limits"'; then
	pass "patched resources include both requests and limits"
else
	fail "patched resources missing requests/limits: $RES"
fi

log "4. Re-applying: an already-hardened workload is a true no-op"
REAPPLY_RESPONSE="$(post_harden true)"
if echo "$REAPPLY_RESPONSE" | grep -q '"applied":false'; then
	pass "already-hardened workload correctly reports applied:false"
else
	fail "re-applying to an already-hardened workload should be a no-op: $REAPPLY_RESPONSE"
fi

log "5. Protected namespaces are rejected"
PROTECTED_CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://localhost:${LOCAL_PORT}/harden" \
	-H "Content-Type: application/json" -d '{"namespaces":["kube-system"]}')"
if [[ "$PROTECTED_CODE" == "400" ]]; then
	pass "kube-system correctly rejected with 400"
else
	fail "expected 400 for a protected namespace, got $PROTECTED_CODE"
fi

echo ""
if [[ "$FAILURES" -eq 0 ]]; then
	echo "RESULT: PASS — hardening detects gaps, applies only on request, patches correctly, and is idempotent."
	exit 0
else
	echo "RESULT: FAIL — ${FAILURES} assertion(s) failed, see [FAIL] lines above."
	exit 1
fi
