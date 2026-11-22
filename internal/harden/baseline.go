// Package harden computes what's missing from a workload's pod template
// relative to the hardening baseline, and produces a patched version that
// fills the gaps. See README.md "What counts as 'hardened' securityContext"
// and "Default requests/limits vs. report-only" for the reasoning behind
// the specific baseline and defaults used here.
//
// Like internal/netpol, this package is deliberately pure: it only
// operates on *corev1.PodTemplateSpec (the one field Deployment,
// StatefulSet, and DaemonSet all happen to share, even though the
// Kubernetes API types don't expose a common interface for it) and never
// touches a cluster. internal/httpapi/harden.go is what actually lists
// workloads and applies the patch.
package harden

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ResourceDefaults are only used when a caller opts into apply=true and
// doesn't override them in the request — see README's "report-only by
// default" decision.
type ResourceDefaults struct {
	RequestCPU    resource.Quantity
	RequestMemory resource.Quantity
	LimitCPU      resource.Quantity
	LimitMemory   resource.Quantity
}

// DefaultResourceDefaults returns the conservative baseline used unless a
// request explicitly overrides it.
func DefaultResourceDefaults() ResourceDefaults {
	return ResourceDefaults{
		RequestCPU:    resource.MustParse("100m"),
		RequestMemory: resource.MustParse("128Mi"),
		LimitCPU:      resource.MustParse("500m"),
		LimitMemory:   resource.MustParse("512Mi"),
	}
}

// boolPtr/stringPtr-style helpers for building the baseline SecurityContext.
func ptrBool(b bool) *bool { return &b }

// hardenedSecurityContext returns a complete, fresh securityContext meeting
// the hardening baseline. Used as the source of truth both for detecting
// gaps and for filling them in.
//
// Deliberately does NOT set RunAsUser. Forcing a fixed UID would make
// RunAsNonRoot "succeed" even on images that were never built to run as
// non-root, hiding the real problem instead of surfacing it. Without
// RunAsUser, kubelet fails loudly (CreateContainerConfigError) on images
// whose declared user is root/non-numeric — see README "Known limitations":
// this is intentional fail-closed behavior, confirmed live against
// nicolaka/netshoot.
func hardenedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptrBool(true),
		AllowPrivilegeEscalation: ptrBool(false),
		ReadOnlyRootFilesystem:   ptrBool(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
