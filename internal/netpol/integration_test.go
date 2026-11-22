//go:build integration

// Package netpol_test holds integration tests that require a real
// Kubernetes cluster with NetworkPolicy actually enforced (i.e. our kind
// cluster with Calico installed — see scripts/setup-cluster.sh). They are
// built with the "integration" tag specifically so `go test ./...` (the
// default, fast path) never silently fails or hangs on someone's machine
// that doesn't have a cluster up; run them explicitly with:
//
//	go test -tags=integration ./... -run TestIsolation -v
//
// This is the "tricky" test called out in README.md: it can't be faked
// with client-go's fake clientset, because the thing actually under test
// is whether traffic is *really* blocked at the network layer by Calico —
// a fake clientset would happily let you "create" a NetworkPolicy object
// with no CNI underneath actually doing anything with it, and the test
// would pass while proving nothing.
package netpol_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/httpapi"
	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/k8sclient"
	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/netpol"
)

// TestIsolation_BlocksOnlyTheIntendedPairAndRestoresCleanly is the
// end-to-end test: isolate tenant-a/gateway from tenant-b/dashboard,
// confirm the targeted pair is blocked while two bystanders (one in the
// SAME namespace as the blocked target, one in a DIFFERENT namespace
// entirely), external egress, and DNS all keep working — then clear the
// isolation and confirm tenant-b/dashboard is reachable again.
//
// Requires: the kind-k8s-sentinel cluster up (scripts/setup-cluster.sh)
// with the sample workloads applied (manifests/samples/), and kubectl on
// PATH pointed at that context.
func TestIsolation_BlocksOnlyTheIntendedPairAndRestoresCleanly(t *testing.T) {
	clientset, err := k8sclient.New("")
	if err != nil {
		t.Fatalf("building kubernetes client: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(discardLogger(), clientset))
	// Registered via t.Cleanup (not `defer`) and BEFORE the isolation
	// safety-net cleanup below, specifically so ordering works out
	// correctly: t.Cleanup funcs run in LIFO order, so this runs SECOND
	// (after the isolation is deleted), not first. A plain `defer
	// srv.Close()` here would run before any t.Cleanup callback and shut
	// the test server down before the safety-net delete could reach it.
	t.Cleanup(srv.Close)

	req := netpol.Request{
		A: netpol.WorkloadRef{Namespace: "tenant-a", Selector: map[string]string{"app": "gateway"}},
		B: netpol.WorkloadRef{Namespace: "tenant-b", Selector: map[string]string{"app": "dashboard"}},
	}

	// 1. Baseline: everything reachable before isolation exists.
	assertReachable(t, "baseline: dashboard", "dashboard.tenant-b.svc.cluster.local")
	assertReachable(t, "baseline: widget (same ns as target)", "widget.tenant-b.svc.cluster.local")
	assertReachable(t, "baseline: widget (different ns)", "widget.tenant-c.svc.cluster.local")

	// 2. Apply isolation.
	id := postIsolate(t, srv.URL, req)
	t.Cleanup(func() { deleteIsolate(t, srv.URL, id) }) // restore even if the test fails partway through

	// Give Calico a moment to actually program the policy — NetworkPolicy
	// enforcement is eventually consistent, not synchronous with the API
	// call returning.
	time.Sleep(3 * time.Second)

	// 3. The targeted pair must be blocked...
	assertBlocked(t, "isolated: dashboard", "dashboard.tenant-b.svc.cluster.local")

	// ...but everything else must still work. This is the part that's
	// easy to get wrong: a naive "deny traffic to B" policy can
	// accidentally also break DNS, external egress, or sibling workloads
	// that merely share B's namespace — see README.md's "Preserving DNS
	// and external egress" and "selector-specificity" discussion.
	assertReachable(t, "isolated: widget (same ns as target, must stay reachable)", "widget.tenant-b.svc.cluster.local")
	assertReachable(t, "isolated: widget (different ns, must stay reachable)", "widget.tenant-c.svc.cluster.local")
	assertDNSResolves(t, "dashboard.tenant-b.svc.cluster.local")
	assertExternalReachable(t)

	// 4. Clear the isolation and confirm full restoration.
	deleteIsolate(t, srv.URL, id)
	time.Sleep(2 * time.Second)
	assertReachable(t, "restored: dashboard", "dashboard.tenant-b.svc.cluster.local")
}

func postIsolate(t *testing.T, baseURL string, req netpol.Request) string {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(baseURL+"/isolate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /isolate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /isolate: expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding /isolate response: %v", err)
	}
	return out.ID
}

func deleteIsolate(t *testing.T, baseURL, id string) {
	t.Helper()
	httpReq, _ := http.NewRequest(http.MethodDelete, baseURL+"/isolate/"+id, nil)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("DELETE /isolate/%s: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE /isolate/%s: expected 204 or 404, got %d", id, resp.StatusCode)
	}
}

// assertReachable/assertBlocked exec into the gateway pod and curl the
// target, exactly mirroring the manual verification done during
// development — now codified so it's re-runnable, not something only
// proven once by hand.

func assertReachable(t *testing.T, label, target string) {
	t.Helper()
	code := curlFromGateway(t, target)
	if code != "200" {
		t.Errorf("%s: expected HTTP 200 from %s, got %q", label, target, code)
	}
}

func assertBlocked(t *testing.T, label, target string) {
	t.Helper()
	code := curlFromGateway(t, target)
	if code == "200" {
		t.Errorf("%s: expected %s to be blocked, but got HTTP 200", label, target)
	}
}

func assertExternalReachable(t *testing.T) {
	t.Helper()
	code := curlFromGateway(t, "https://example.com")
	if code != "200" {
		t.Errorf("external egress: expected HTTP 200, got %q", code)
	}
}

func assertDNSResolves(t *testing.T, hostname string) {
	t.Helper()
	gwPod := gatewayPodName(t)
	out, err := exec.Command("kubectl", "--context", "kind-k8s-sentinel", "-n", "tenant-a", "exec", gwPod, "--",
		"nslookup", hostname).CombinedOutput()
	if err != nil {
		t.Errorf("DNS resolution for %s failed: %v\n%s", hostname, err, out)
	}
}

func curlFromGateway(t *testing.T, target string) string {
	t.Helper()
	gwPod := gatewayPodName(t)
	out, err := exec.Command("kubectl", "--context", "kind-k8s-sentinel", "-n", "tenant-a", "exec", gwPod, "--",
		"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5", target).CombinedOutput()
	// A blocked connection legitimately exits non-zero (curl's own
	// timeout/connection-refused exit codes) with empty/partial stdout —
	// that's an expected outcome for assertBlocked, not a test
	// infrastructure failure, so we don't fail the command here.
	_ = err
	return strings.TrimSpace(string(out))
}

func gatewayPodName(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("kubectl", "--context", "kind-k8s-sentinel", "-n", "tenant-a", "get", "pod",
		"-l", "app=gateway", "-o", "jsonpath={.items[0].metadata.name}").Output()
	if err != nil {
		t.Fatalf("looking up gateway pod: %v", err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		t.Fatal("no gateway pod found in tenant-a — is manifests/samples/ applied?")
	}
	return name
}

// discardLogger keeps the server's own log lines out of `go test` output;
// re-point at os.Stderr locally if you need to debug a failing run.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
