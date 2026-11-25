// Package netpol builds the NetworkPolicy objects that implement "isolate
// workload A from workload B, without breaking anything else" — see
// README.md "Key design decisions" for the full reasoning behind the
// approach used here.
package netpol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// ManagedByLabel marks every NetworkPolicy this tool creates, so
	// "list active isolations" and "turn off" can find exactly our own
	// objects by label selector, without touching anything we didn't
	// create (see README's "State tracking for 'undo'" decision).
	ManagedByLabel = "k8s-sentinel.io/managed-by"
	ManagedByValue = "k8s-sentinel"

	// IsolationIDLabel ties the two NetworkPolicy objects (one per
	// namespace) belonging to a single isolation request together.
	IsolationIDLabel = "k8s-sentinel.io/isolation-id"

	// namespaceNameLabel is the label every namespace gets automatically
	// since Kubernetes v1.21, set to the namespace's own name. It's what
	// lets us write "all namespaces except this one" using a standard
	// NotIn match expression instead of a CNI-specific negation feature.
	namespaceNameLabel = "kubernetes.io/metadata.name"

	// kubeSystemNamespace is where CoreDNS lives in a stock cluster.
	kubeSystemNamespace = "kube-system"
)

// WorkloadRef identifies one side of an isolation request: a namespace plus
// the label selector that picks out the specific workload within it.
type WorkloadRef struct {
	Namespace string            `json:"namespace"`
	Selector  map[string]string `json:"selector"`
}

// Request is the isolate/clear request body — deliberately named to mirror
// the example request shape (`isolate: {a: {...}, b: {...}}`).
type Request struct {
	A WorkloadRef `json:"a"`
	B WorkloadRef `json:"b"`
}

// ID computes a deterministic identifier for a request. It's symmetric
// (isolate(A, B) and isolate(B, A) produce the same ID) and stable across
// repeated calls, which is what makes POST /isolate idempotent — issuing
// the same request twice updates the same two NetworkPolicy objects
// instead of creating duplicates.
func (r Request) ID() string {
	refs := []WorkloadRef{r.A, r.B}
	sort.Slice(refs, func(i, j int) bool {
		return canonicalRef(refs[i]) < canonicalRef(refs[j])
	})
	canon := canonicalRef(refs[0]) + "|" + canonicalRef(refs[1])
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:])[:12]
}

func canonicalRef(w WorkloadRef) string {
	keys := make([]string, 0, len(w.Selector))
	for k := range w.Selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sortedSelector := make(map[string]string, len(w.Selector))
	for _, k := range keys {
		sortedSelector[k] = w.Selector[k]
	}
	b, _ := json.Marshal(sortedSelector)
	return w.Namespace + ":" + string(b)
}

// Policies returns the two NetworkPolicy objects (one living in A's
// namespace, one in B's namespace) that together block traffic in both
// directions between A and B while preserving everything else — including
// DNS and external egress, which would otherwise break the instant either
// policy attaches (see README's "Preserving DNS and external egress"
// decision).
func Policies(req Request) []*networkingv1.NetworkPolicy {
	id := req.ID()
	return []*networkingv1.NetworkPolicy{
		policyFor(id, req.A, req.B),
		policyFor(id, req.B, req.A),
	}
}

// policyFor builds the policy that lives in self's namespace, selecting
// self's pods, and excluding other from both its allowed ingress and egress.
func policyFor(isolationID string, self, other WorkloadRef) *networkingv1.NetworkPolicy {
	notOther := allExcept(other)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("k8s-sentinel-isolate-%s", isolationID),
			Namespace: self.Namespace,
			Labels: map[string]string{
				ManagedByLabel:   ManagedByValue,
				IsolationIDLabel: isolationID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: self.Selector},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: notOther},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{To: notOther},
				dnsEgressRule(),
				externalEgressRule(),
			},
		},
	}
}

// allExcept returns a peer list meaning "every pod, in every namespace,
// except the one identified by other" — using the NotIn trick described in
// README.md rather than any CNI-specific deny/negation feature, so the
// resulting NetworkPolicy stays portable to any CNI that enforces the
// standard API.
func allExcept(other WorkloadRef) []networkingv1.NetworkPolicyPeer {
	return []networkingv1.NetworkPolicyPeer{
		// Everything in every namespace OTHER than other's namespace.
		{
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      namespaceNameLabel,
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{other.Namespace},
					},
				},
			},
		},
		// Within other's own namespace specifically, everything EXCEPT
		// other's own selector — so siblings in the same namespace as
		// the isolated peer stay reachable.
		{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{namespaceNameLabel: other.Namespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: notInExpressions(other.Selector),
			},
		},
	}
}

// notInExpressions turns a selector like {"app": "dashboard"} into
// match expressions meaning "not this." For a single-key selector (the
// case our worked example uses, and the only case our sample
// workloads exercise) this is exact. For a multi-key selector it's
// intentionally over-exclusive rather than under-exclusive: a pod sharing
// even one label key/value with `other` gets treated as "possibly other"
// and excluded from the allow-list — the safer failure direction for an
// isolation/containment tool. Documented as a known limitation in
// README.md rather than silently assumed correct.
func notInExpressions(selector map[string]string) []metav1.LabelSelectorRequirement {
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	exprs := make([]metav1.LabelSelectorRequirement, 0, len(keys))
	for _, k := range keys {
		exprs = append(exprs, metav1.LabelSelectorRequirement{
			Key:      k,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   []string{selector[k]},
		})
	}
	return exprs
}

// dnsEgressRule always allows egress to kube-system on the standard DNS
// ports. Without this, the instant a pod is selected by an Egress-type
// NetworkPolicy it can no longer resolve any hostname at all — including
// the hostnames of the workloads it's still supposed to be able to reach.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	dnsPort := intstr.FromInt(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{namespaceNameLabel: kubeSystemNamespace},
				},
			},
		},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &dnsPort},
			{Protocol: &tcp, Port: &dnsPort},
		},
	}
}

// externalEgressRule always allows egress to the public internet. Same
// reasoning as dnsEgressRule: an Egress-type NetworkPolicy with no ipBlock
// rule would silently cut off all external connectivity as a side effect
// of isolating two in-cluster workloads from each other.
func externalEgressRule() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
		},
	}
}
