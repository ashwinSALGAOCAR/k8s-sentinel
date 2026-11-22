package harden

import corev1 "k8s.io/api/core/v1"

// Patch returns a deep copy of template with every gap from plan filled
// in. Pure function: doesn't touch the cluster, doesn't mutate the input —
// the caller (internal/httpapi/harden.go) is responsible for actually
// sending the result via client-go's Update().
//
// Only the fields that were actually detected as missing get touched —
// e.g. a container that already has a correct readOnlyRootFilesystem but
// is missing capabilities.drop gets exactly that one field filled in,
// not its whole securityContext replaced wholesale. This avoids
// clobbering anything a team intentionally set beyond the baseline
// (extra dropped capabilities, a custom runAsUser, etc.).
func Patch(template *corev1.PodTemplateSpec, plan Plan, resources ResourceDefaults) *corev1.PodTemplateSpec {
	patched := template.DeepCopy()

	gapsByContainer := make(map[string]ContainerGap, len(plan.ContainerGaps))
	for _, g := range plan.ContainerGaps {
		gapsByContainer[g.ContainerName] = g
	}

	for i := range patched.Spec.Containers {
		c := &patched.Spec.Containers[i]
		gap, ok := gapsByContainer[c.Name]
		if !ok {
			continue // this container had no gaps; leave it untouched
		}

		if gap.MissingRequests {
			if c.Resources.Requests == nil {
				c.Resources.Requests = corev1.ResourceList{}
			}
			c.Resources.Requests[corev1.ResourceCPU] = resources.RequestCPU
			c.Resources.Requests[corev1.ResourceMemory] = resources.RequestMemory
		}
		if gap.MissingLimits {
			if c.Resources.Limits == nil {
				c.Resources.Limits = corev1.ResourceList{}
			}
			c.Resources.Limits[corev1.ResourceCPU] = resources.LimitCPU
			c.Resources.Limits[corev1.ResourceMemory] = resources.LimitMemory
		}

		if len(gap.SecurityContextGaps) > 0 {
			applySecurityContextGaps(c, gap.SecurityContextGaps)
		}
	}

	return patched
}

// applySecurityContextGaps fills in exactly the named gaps on c's
// securityContext, creating it (and its nested structs) only as needed.
func applySecurityContextGaps(c *corev1.Container, gaps []string) {
	if c.SecurityContext == nil {
		c.SecurityContext = &corev1.SecurityContext{}
	}
	baseline := hardenedSecurityContext()

	for _, gap := range gaps {
		switch gap {
		case "runAsNonRoot":
			c.SecurityContext.RunAsNonRoot = baseline.RunAsNonRoot
		case "allowPrivilegeEscalation":
			c.SecurityContext.AllowPrivilegeEscalation = baseline.AllowPrivilegeEscalation
		case "readOnlyRootFilesystem":
			c.SecurityContext.ReadOnlyRootFilesystem = baseline.ReadOnlyRootFilesystem
		case "capabilities.drop=ALL":
			if c.SecurityContext.Capabilities == nil {
				c.SecurityContext.Capabilities = &corev1.Capabilities{}
			}
			if !dropsAll(c.SecurityContext.Capabilities.Drop) {
				c.SecurityContext.Capabilities.Drop = append(c.SecurityContext.Capabilities.Drop, "ALL")
			}
		case "seccompProfile":
			c.SecurityContext.SeccompProfile = baseline.SeccompProfile
		}
	}
}
