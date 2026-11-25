#!/usr/bin/env bash
# Standalone, Go-toolchain-free proof that isolation genuinely blocks
# traffic and fully restores it — the required "prove it" deliverable,
# runnable by anyone with kubectl/curl against the already-deployed service
# (see manifests/k8s-sentinel/deploy.yaml). Mirrors the exact assertions in
# internal/netpol/integration_test.go, just against the real in-cluster
# Deployment instead of a local httptest server, and without needing Go.
#
# Usage: scripts/verify-isolation.sh
# Requires: kind-k8s-sentinel cluster up, manifests/samples/ and
# manifests/k8s-sentinel/deploy.yaml applied, kubectl on PATH.
set -euo pipefail

CONTEXT="kind-k8s-sentinel"
NAMESPACE="k8s-sentinel-system"
SERVICE="k8s-sentinel"
LOCAL_PORT=18080
FAILURES=0

log()  { echo "==> $*"; }
pass() { echo "  [PASS] $*"; }
fail() { echo "  [FAIL] $*"; FAILURES=$((FAILURES + 1)); }

kctl() { kubectl --context "$CONTEXT" "$@"; }

gateway_pod() {
	kctl -n tenant-a get pod -l app=gateway -o jsonpath='{.items[0].metadata.name}'
}

curl_from_gateway() {
	local target="$1"
	kctl -n tenant-a exec "$(gateway_pod)" -- \
		curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$target" 2>/dev/null || true
}

assert_reachable() {
	local label="$1" target="$2" code
	code="$(curl_from_gateway "$target")"
	if [[ "$code" == "200" ]]; then pass "$label: $target -> $code"; else fail "$label: expected 200 from $target, got '$code'"; fi
}

assert_blocked() {
	local label="$1" target="$2" code
	code="$(curl_from_gateway "$target")"
	if [[ "$code" != "200" ]]; then pass "$label: $target blocked (got '$code')"; else fail "$label: expected $target to be blocked, got 200"; fi
}

assert_dns_resolves() {
	local hostname="$1"
	if kctl -n tenant-a exec "$(gateway_pod)" -- nslookup "$hostname" >/dev/null 2>&1; then
		pass "DNS resolves: $hostname"
	else
		fail "DNS resolution failed for $hostname"
	fi
}

assert_external_reachable() {
	assert_reachable "external egress" "https://example.com"
}

PORT_FORWARD_PID=""
ISOLATION_ID=""

cleanup() {
	if [[ -n "$ISOLATION_ID" ]]; then
		curl -s -X DELETE "http://localhost:${LOCAL_PORT}/isolate/${ISOLATION_ID}" >/dev/null 2>&1 || true
	fi
	if [[ -n "$PORT_FORWARD_PID" ]] && kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then
		# kubectl port-forward doesn't always exit promptly on SIGTERM while
		# a stream is active — give it a moment, then force it so this
		# script never leaves an orphaned port-forward behind (found by
		# running this script for real: PPID 1, reparented to init).
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
# Deliberately NOT using the kctl() helper here: backgrounding a function
# call captures the wrapper subshell's PID in $!, not kubectl's own PID —
# killing that subshell orphans kubectl instead of stopping it (found by
# actually running this script: the port-forward process survived with
# PPID 1). Calling kubectl directly keeps $! pointing at the real process.
kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward "svc/${SERVICE}" "${LOCAL_PORT}:8080" >/tmp/k8s-sentinel-port-forward.log 2>&1 &
PORT_FORWARD_PID=$!

for _ in $(seq 1 20); do
	if curl -s -o /dev/null "http://localhost:${LOCAL_PORT}/healthz"; then break; fi
	sleep 0.5
done

log "1. Baseline (pre-isolation): everything should be reachable"
assert_reachable "baseline: dashboard" "dashboard.tenant-b.svc.cluster.local"
assert_reachable "baseline: widget (same ns as target)" "widget.tenant-b.svc.cluster.local"
assert_reachable "baseline: widget (different ns)" "widget.tenant-c.svc.cluster.local"

log "2. Isolating tenant-a/gateway <-> tenant-b/dashboard"
RESPONSE="$(curl -s -X POST "http://localhost:${LOCAL_PORT}/isolate" \
	-H "Content-Type: application/json" \
	-d '{"a":{"namespace":"tenant-a","selector":{"app":"gateway"}},"b":{"namespace":"tenant-b","selector":{"app":"dashboard"}}}')"
ISOLATION_ID="$(echo "$RESPONSE" | grep -o '"id":"[a-f0-9]*"' | cut -d'"' -f4)"
if [[ -z "$ISOLATION_ID" ]]; then
	fail "POST /isolate did not return an id: $RESPONSE"
else
	pass "isolation created, id=$ISOLATION_ID"
fi

log "   (waiting for Calico to actually program the policy)"
sleep 3

log "3. While isolated: target pair blocked, everything else still works"
assert_blocked "isolated: dashboard" "dashboard.tenant-b.svc.cluster.local"
assert_reachable "isolated: widget (same ns as target, must stay reachable)" "widget.tenant-b.svc.cluster.local"
assert_reachable "isolated: widget (different ns, must stay reachable)" "widget.tenant-c.svc.cluster.local"
assert_dns_resolves "dashboard.tenant-b.svc.cluster.local"
assert_external_reachable

log "4. Restoring (DELETE /isolate/${ISOLATION_ID})"
DELETE_CODE="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "http://localhost:${LOCAL_PORT}/isolate/${ISOLATION_ID}")"
if [[ "$DELETE_CODE" == "204" ]]; then pass "isolation deleted (204)"; else fail "expected 204 deleting isolation, got $DELETE_CODE"; fi
ISOLATION_ID="" # already deleted — don't let cleanup() try again
sleep 2

log "5. After restore: previously-blocked pair reachable again"
assert_reachable "restored: dashboard" "dashboard.tenant-b.svc.cluster.local"

log "6. No leftover NetworkPolicy objects anywhere in the cluster"
LEFTOVER="$(kctl get networkpolicies -A -l k8s-sentinel.io/managed-by=k8s-sentinel -o name | wc -l | tr -d ' ')"
if [[ "$LEFTOVER" == "0" ]]; then pass "0 managed NetworkPolicy objects remain"; else fail "$LEFTOVER managed NetworkPolicy object(s) still exist"; fi

echo ""
if [[ "$FAILURES" -eq 0 ]]; then
	echo "RESULT: PASS — isolation blocks only the intended pair and restores cleanly."
	exit 0
else
	echo "RESULT: FAIL — ${FAILURES} assertion(s) failed, see [FAIL] lines above."
	exit 1
fi
