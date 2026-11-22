package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/httpapi"
)

// unhardenedDeployment builds a Deployment with no resource
// requests/limits and no securityContext — the "both missing" case.
func unhardenedDeployment(namespace, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
				},
			},
		},
	}
}

func newTestServer(objects ...runtime.Object) (*httptest.Server, *fake.Clientset) {
	clientset := fake.NewSimpleClientset(objects...)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpapi.New(logger, clientset))
	return srv, clientset
}

func mustQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

func postHarden(t *testing.T, baseURL string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(baseURL+"/harden", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /harden: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHarden_DryRunByDefault_DoesNotMutate(t *testing.T) {
	srv, clientset := newTestServer(unhardenedDeployment("team-a", "web"))
	defer srv.Close()

	status, body := postHarden(t, srv.URL, map[string]any{"namespaces": []string{"team-a"}})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", status, body)
	}

	results := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	first := results[0].(map[string]any)
	if applied, _ := first["applied"].(bool); applied {
		t.Error("expected applied=false by default (report-only)")
	}

	got, err := clientset.AppsV1().Deployments("team-a").Get(t.Context(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("fetching deployment: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].SecurityContext != nil {
		t.Error("dry-run must not have mutated the actual object in the cluster")
	}
}

func TestHarden_Apply_PatchesTheDeployment(t *testing.T) {
	srv, clientset := newTestServer(unhardenedDeployment("team-a", "web"))
	defer srv.Close()

	status, _ := postHarden(t, srv.URL, map[string]any{"namespaces": []string{"team-a"}, "apply": true})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	got, err := clientset.AppsV1().Deployments("team-a").Get(t.Context(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("fetching deployment: %v", err)
	}
	sc := got.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("expected apply=true to actually patch the Deployment's securityContext")
	}
	if got.Spec.Template.Spec.Containers[0].Resources.Requests == nil {
		t.Error("expected apply=true to actually patch the Deployment's resource requests")
	}
}

func TestHarden_RejectsProtectedNamespace(t *testing.T) {
	srv, _ := newTestServer()
	defer srv.Close()

	status, body := postHarden(t, srv.URL, map[string]any{"namespaces": []string{"kube-system"}})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a protected namespace, got %d: %v", status, body)
	}
}

func TestHarden_AlreadyHardenedWorkload_ReportsNoChangesAndIsNotTouched(t *testing.T) {
	d := unhardenedDeployment("team-a", "already-good")
	trueVal, falseVal := true, false
	d.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		RunAsNonRoot:             &trueVal,
		AllowPrivilegeEscalation: &falseVal,
		ReadOnlyRootFilesystem:   &trueVal,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	d.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{"cpu": mustQuantity("100m")},
		Limits:   corev1.ResourceList{"cpu": mustQuantity("500m")},
	}

	srv, _ := newTestServer(d)
	defer srv.Close()

	status, body := postHarden(t, srv.URL, map[string]any{"namespaces": []string{"team-a"}, "apply": true})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", status, body)
	}

	results := body["results"].([]any)
	first := results[0].(map[string]any)
	if applied, _ := first["applied"].(bool); applied {
		t.Error("an already-hardened workload should never be reported as applied, since nothing needed to change")
	}
}
