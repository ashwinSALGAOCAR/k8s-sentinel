package harden

import corev1 "k8s.io/api/core/v1"

// ContainerGap describes exactly what one container is missing relative to
// the hardening baseline. Kept granular (separate flags per concern,
// specific security-context field names) rather than a single boolean so
// the dry-run response can tell an SRE precisely what would change, not
// just that something would.
type ContainerGap struct {
	ContainerName string

	MissingRequests bool
	MissingLimits   bool

	// SecurityContextGaps lists the specific fields that don't meet the
	// baseline, e.g. "runAsNonRoot", "allowPrivilegeEscalation". Empty
	// means the container's securityContext is already fully hardened.
	SecurityContextGaps []string
}

// HasGaps reports whether this container needs any change at all.
func (g ContainerGap) HasGaps() bool {
	return g.MissingRequests || g.MissingLimits || len(g.SecurityContextGaps) > 0
}

// Plan is the full set of gaps across every container in a pod template.
type Plan struct {
	ContainerGaps []ContainerGap
}

// NeedsChanges reports whether applying this plan would change anything —
// used to decide whether an already-hardened workload is a true no-op.
func (p Plan) NeedsChanges() bool {
	for _, g := range p.ContainerGaps {
		if g.HasGaps() {
			return true
		}
	}
	return false
}

// Detect inspects a pod template and returns what's missing, per
// container, relative to the hardening baseline. Pure function — no
// cluster calls, nothing mutated.
func Detect(template *corev1.PodTemplateSpec) Plan {
	gaps := make([]ContainerGap, 0, len(template.Spec.Containers))
	for _, c := range template.Spec.Containers {
		gap := ContainerGap{
			ContainerName:       c.Name,
			MissingRequests:     c.Resources.Requests == nil,
			MissingLimits:       c.Resources.Limits == nil,
			SecurityContextGaps: securityContextGaps(c.SecurityContext),
		}
		if gap.HasGaps() {
			gaps = append(gaps, gap)
		}
	}
	return Plan{ContainerGaps: gaps}
}

// securityContextGaps compares sc against the hardening baseline and
// returns the names of fields that fall short. A nil securityContext
// means every field is missing.
func securityContextGaps(sc *corev1.SecurityContext) []string {
	if sc == nil {
		return []string{"runAsNonRoot", "allowPrivilegeEscalation", "readOnlyRootFilesystem", "capabilities.drop=ALL", "seccompProfile"}
	}

	var gaps []string
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		gaps = append(gaps, "runAsNonRoot")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		gaps = append(gaps, "allowPrivilegeEscalation")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		gaps = append(gaps, "readOnlyRootFilesystem")
	}
	if sc.Capabilities == nil || !dropsAll(sc.Capabilities.Drop) {
		gaps = append(gaps, "capabilities.drop=ALL")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		gaps = append(gaps, "seccompProfile")
	}
	return gaps
}

func dropsAll(caps []corev1.Capability) bool {
	for _, c := range caps {
		if c == "ALL" {
			return true
		}
	}
	return false
}
