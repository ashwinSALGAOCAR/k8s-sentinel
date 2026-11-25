package harden

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podTemplate(containers ...corev1.Container) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

func hardenedContainer(name string) corev1.Container {
	return corev1.Container{
		Name: name,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
		SecurityContext: hardenedSecurityContext(),
	}
}

// --- Detect: the four cases workload-hardening detection centers on ---

func TestDetect_AlreadyHardened_IsNoOp(t *testing.T) {
	template := podTemplate(hardenedContainer("app"))
	plan := Detect(template)

	if plan.NeedsChanges() {
		t.Fatalf("expected an already-hardened container to need no changes, got %+v", plan)
	}
}

func TestDetect_MissingResourcesOnly(t *testing.T) {
	c := hardenedContainer("app")
	c.Resources = corev1.ResourceRequirements{} // strip resources, leave securityContext hardened
	plan := Detect(podTemplate(c))

	if len(plan.ContainerGaps) != 1 {
		t.Fatalf("expected exactly 1 container gap, got %d", len(plan.ContainerGaps))
	}
	gap := plan.ContainerGaps[0]
	if !gap.MissingRequests || !gap.MissingLimits {
		t.Errorf("expected both requests and limits flagged missing, got %+v", gap)
	}
	if len(gap.SecurityContextGaps) != 0 {
		t.Errorf("expected no securityContext gaps since it was already hardened, got %v", gap.SecurityContextGaps)
	}
}

func TestDetect_MissingSecurityContextOnly(t *testing.T) {
	c := hardenedContainer("app")
	c.SecurityContext = nil // strip securityContext, leave resources set
	plan := Detect(podTemplate(c))

	if len(plan.ContainerGaps) != 1 {
		t.Fatalf("expected exactly 1 container gap, got %d", len(plan.ContainerGaps))
	}
	gap := plan.ContainerGaps[0]
	if gap.MissingRequests || gap.MissingLimits {
		t.Errorf("expected resources to already be fine, got %+v", gap)
	}
	if len(gap.SecurityContextGaps) != 5 {
		t.Errorf("expected all 5 securityContext fields flagged for a nil securityContext, got %v", gap.SecurityContextGaps)
	}
}

func TestDetect_BothMissing(t *testing.T) {
	c := corev1.Container{Name: "app"} // nothing set at all
	plan := Detect(podTemplate(c))

	if len(plan.ContainerGaps) != 1 {
		t.Fatalf("expected exactly 1 container gap, got %d", len(plan.ContainerGaps))
	}
	gap := plan.ContainerGaps[0]
	if !gap.MissingRequests || !gap.MissingLimits {
		t.Errorf("expected both requests and limits flagged missing, got %+v", gap)
	}
	if len(gap.SecurityContextGaps) != 5 {
		t.Errorf("expected all 5 securityContext fields flagged, got %v", gap.SecurityContextGaps)
	}
}

// TestDetect_PartialSecurityContext is the trickiest detector case: a
// securityContext that's present but only partially meets the baseline
// (e.g. runAsNonRoot is set correctly but capabilities.drop isn't) must
// report only the fields that are actually wrong, not all-or-nothing —
// getting this wrong either silently under-reports real gaps or needlessly
// flags fields that are already correct.
func TestDetect_PartialSecurityContext(t *testing.T) {
	c := hardenedContainer("app")
	c.SecurityContext.Capabilities = nil // only this one field regresses

	plan := Detect(podTemplate(c))
	if len(plan.ContainerGaps) != 1 {
		t.Fatalf("expected exactly 1 container gap, got %d", len(plan.ContainerGaps))
	}
	gaps := plan.ContainerGaps[0].SecurityContextGaps
	if len(gaps) != 1 || gaps[0] != "capabilities.drop=ALL" {
		t.Errorf("expected exactly one gap (capabilities.drop=ALL), got %v", gaps)
	}
}

func TestDetect_MultipleContainers_OnlyFlagsTheOneWithGaps(t *testing.T) {
	good := hardenedContainer("sidecar")
	bad := corev1.Container{Name: "app"}

	plan := Detect(podTemplate(good, bad))
	if len(plan.ContainerGaps) != 1 {
		t.Fatalf("expected exactly 1 flagged container, got %d", len(plan.ContainerGaps))
	}
	if plan.ContainerGaps[0].ContainerName != "app" {
		t.Errorf("expected the flagged container to be 'app', got %q", plan.ContainerGaps[0].ContainerName)
	}
}

// --- Patch: applying the plan should fill in exactly what was missing ---

func TestPatch_FillsOnlyTheDetectedGaps(t *testing.T) {
	c := hardenedContainer("app")
	c.SecurityContext.Capabilities = nil // same partial regression as above
	template := podTemplate(c)

	plan := Detect(template)
	patched := Patch(template, plan, DefaultResourceDefaults())

	got := patched.Spec.Containers[0]
	if got.SecurityContext.RunAsNonRoot == nil || !*got.SecurityContext.RunAsNonRoot {
		t.Error("expected runAsNonRoot to remain true (it was never broken)")
	}
	if got.SecurityContext.Capabilities == nil || !dropsAll(got.SecurityContext.Capabilities.Drop) {
		t.Error("expected capabilities.drop=ALL to be filled in")
	}
}

func TestPatch_AlreadyHardened_ProducesIdenticalResult(t *testing.T) {
	template := podTemplate(hardenedContainer("app"))
	plan := Detect(template)
	patched := Patch(template, plan, DefaultResourceDefaults())

	if !plan.NeedsChanges() && len(patched.Spec.Containers[0].SecurityContext.Capabilities.Drop) != 1 {
		t.Errorf("expected an already-hardened container to be left exactly as-is")
	}
}

func TestPatch_DoesNotMutateTheOriginalTemplate(t *testing.T) {
	c := corev1.Container{Name: "app"}
	template := podTemplate(c)
	plan := Detect(template)

	_ = Patch(template, plan, DefaultResourceDefaults())

	if template.Spec.Containers[0].SecurityContext != nil {
		t.Fatal("Patch must not mutate its input template — original container's securityContext should still be nil")
	}
}

func TestPatch_UsesProvidedResourceOverrides(t *testing.T) {
	c := corev1.Container{Name: "app"}
	template := podTemplate(c)
	plan := Detect(template)

	overrides := ResourceDefaults{
		RequestCPU:    resource.MustParse("250m"),
		RequestMemory: resource.MustParse("256Mi"),
		LimitCPU:      resource.MustParse("1"),
		LimitMemory:   resource.MustParse("1Gi"),
	}
	patched := Patch(template, plan, overrides)

	gotCPU := patched.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if gotCPU.String() != "250m" {
		t.Errorf("expected overridden request CPU 250m, got %s", gotCPU.String())
	}
}
