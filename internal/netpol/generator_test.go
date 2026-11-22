package netpol

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

func sampleRequest() Request {
	return Request{
		A: WorkloadRef{Namespace: "tenant-a", Selector: map[string]string{"app": "gateway"}},
		B: WorkloadRef{Namespace: "tenant-b", Selector: map[string]string{"app": "dashboard"}},
	}
}

func TestID_IsSymmetric(t *testing.T) {
	req := sampleRequest()
	swapped := Request{A: req.B, B: req.A}

	if req.ID() != swapped.ID() {
		t.Fatalf("expected isolate(A,B) and isolate(B,A) to produce the same ID, got %q vs %q", req.ID(), swapped.ID())
	}
}

func TestID_IsStable(t *testing.T) {
	req := sampleRequest()
	if req.ID() != sampleRequest().ID() {
		t.Fatalf("expected the same request to produce the same ID every time")
	}
}

func TestID_DiffersForDifferentRequests(t *testing.T) {
	req := sampleRequest()
	other := Request{
		A: WorkloadRef{Namespace: "tenant-a", Selector: map[string]string{"app": "gateway"}},
		B: WorkloadRef{Namespace: "tenant-c", Selector: map[string]string{"app": "widget"}},
	}
	if req.ID() == other.ID() {
		t.Fatalf("expected different isolation pairs to produce different IDs")
	}
}

func TestPolicies_ReturnsOnePerNamespace(t *testing.T) {
	policies := Policies(sampleRequest())

	if len(policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(policies))
	}
	if policies[0].Namespace != "tenant-a" {
		t.Errorf("expected first policy in tenant-a, got %s", policies[0].Namespace)
	}
	if policies[1].Namespace != "tenant-b" {
		t.Errorf("expected second policy in tenant-b, got %s", policies[1].Namespace)
	}
}

func TestPolicies_SelectOnlyTheirOwnWorkload(t *testing.T) {
	policies := Policies(sampleRequest())

	aPolicy := policies[0]
	if got := aPolicy.Spec.PodSelector.MatchLabels["app"]; got != "gateway" {
		t.Errorf("tenant-a's policy should select app=gateway, got %q", got)
	}

	bPolicy := policies[1]
	if got := bPolicy.Spec.PodSelector.MatchLabels["app"]; got != "dashboard" {
		t.Errorf("tenant-b's policy should select app=dashboard, got %q", got)
	}
}

func TestPolicies_BothDirectionsPresent(t *testing.T) {
	for _, p := range Policies(sampleRequest()) {
		hasIngress, hasEgress := false, false
		for _, pt := range p.Spec.PolicyTypes {
			switch pt {
			case networkingv1.PolicyTypeIngress:
				hasIngress = true
			case networkingv1.PolicyTypeEgress:
				hasEgress = true
			}
		}
		if !hasIngress || !hasEgress {
			t.Errorf("policy %s/%s should declare both Ingress and Egress policyTypes, got %v", p.Namespace, p.Name, p.Spec.PolicyTypes)
		}
	}
}

// TestPolicies_ExcludesOnlyTheOtherNamespace is the test I'd flag as the
// trickiest/easiest-to-get-wrong one in this package: it asserts the
// generated egress rule's first peer excludes B's namespace specifically
// (via the auto-injected kubernetes.io/metadata.name label), not a
// hardcoded/wrong namespace, and not every namespace. Getting this peer
// wrong would silently either block everything (if too broad) or isolate
// nothing (if the NotIn value is wrong) while the API object still looks
// superficially correct.
func TestPolicies_ExcludesOnlyTheOtherNamespace(t *testing.T) {
	policies := Policies(sampleRequest())
	aPolicy := policies[0] // lives in tenant-a, should exclude tenant-b

	egressToOthers := aPolicy.Spec.Egress[0] // index 0 = the "allow all except B" rule; 1=DNS, 2=external
	peer := egressToOthers.To[0]
	if peer.NamespaceSelector == nil {
		t.Fatalf("expected the first egress peer to use a namespaceSelector")
	}
	exprs := peer.NamespaceSelector.MatchExpressions
	if len(exprs) != 1 {
		t.Fatalf("expected exactly one match expression, got %d", len(exprs))
	}
	if exprs[0].Key != "kubernetes.io/metadata.name" {
		t.Errorf("expected the namespace exclusion to key off kubernetes.io/metadata.name, got %q", exprs[0].Key)
	}
	if exprs[0].Operator != "NotIn" {
		t.Errorf("expected NotIn operator, got %q", exprs[0].Operator)
	}
	if len(exprs[0].Values) != 1 || exprs[0].Values[0] != "tenant-b" {
		t.Errorf("expected the excluded namespace to be exactly tenant-b, got %v", exprs[0].Values)
	}
}

func TestPolicies_IncludeDNSAndExternalBaselineRules(t *testing.T) {
	for _, p := range Policies(sampleRequest()) {
		if len(p.Spec.Egress) != 3 {
			t.Fatalf("expected 3 egress rules (exclude-other, DNS, external), got %d", len(p.Spec.Egress))
		}

		dnsRule := p.Spec.Egress[1]
		if len(dnsRule.Ports) != 2 {
			t.Errorf("expected DNS rule to allow exactly 2 ports (UDP+TCP 53), got %d", len(dnsRule.Ports))
		}

		externalRule := p.Spec.Egress[2]
		if len(externalRule.To) != 1 || externalRule.To[0].IPBlock == nil {
			t.Fatalf("expected the external rule to be an ipBlock peer")
		}
		if externalRule.To[0].IPBlock.CIDR != "0.0.0.0/0" {
			t.Errorf("expected external rule CIDR 0.0.0.0/0, got %q", externalRule.To[0].IPBlock.CIDR)
		}
	}
}

func TestPolicies_AreLabeledForOwnershipTracking(t *testing.T) {
	id := sampleRequest().ID()
	for _, p := range Policies(sampleRequest()) {
		if p.Labels[ManagedByLabel] != ManagedByValue {
			t.Errorf("expected %s=%s label, got %q", ManagedByLabel, ManagedByValue, p.Labels[ManagedByLabel])
		}
		if p.Labels[IsolationIDLabel] != id {
			t.Errorf("expected isolation ID label %q, got %q", id, p.Labels[IsolationIDLabel])
		}
	}
}
