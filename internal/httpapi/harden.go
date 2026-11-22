package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/harden"
)

type hardenRequest struct {
	Namespaces []string                 `json:"namespaces"`
	Apply      bool                     `json:"apply"`
	Resources  *harden.ResourceDefaults `json:"resources,omitempty"` // nil = use DefaultResourceDefaults()
}

type hardenWorkloadResult struct {
	Kind      string      `json:"kind"`
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Plan      harden.Plan `json:"plan"`
	Applied   bool        `json:"applied"`
}

type hardenResponse struct {
	Results []hardenWorkloadResult `json:"results"`
}

// workloadHandle abstracts over Deployment/StatefulSet/DaemonSet — the
// three Kubernetes API types don't share a Go interface for their
// .Spec.Template field, so this is a small, explicit adapter rather than
// triplicating the detect/patch/apply logic three times. Only the
// *listing* step differs per type; everything after that is identical.
type workloadHandle struct {
	kind      string
	namespace string
	name      string
	template  *corev1.PodTemplateSpec
	update    func(ctx context.Context, patched *corev1.PodTemplateSpec) error
}

// handleHardenCreate implements POST /harden.
func (s *Server) handleHardenCreate(w http.ResponseWriter, r *http.Request) {
	var req hardenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if len(req.Namespaces) == 0 {
		writeError(w, http.StatusBadRequest, "at least one namespace is required")
		return
	}
	for _, ns := range req.Namespaces {
		if protectedNamespaces[ns] {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("namespace %q is protected and cannot be hardened", ns))
			return
		}
	}

	resources := harden.DefaultResourceDefaults()
	if req.Resources != nil {
		resources = *req.Resources
	}

	ctx := r.Context()
	var results []hardenWorkloadResult
	for _, ns := range req.Namespaces {
		handles, err := s.listWorkloads(ctx, ns)
		if err != nil {
			s.logger.Error("failed to list workloads for hardening", "namespace", ns, "error", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list workloads in namespace %q", ns))
			return
		}

		for _, h := range handles {
			plan := harden.Detect(h.template)
			result := hardenWorkloadResult{Kind: h.kind, Namespace: h.namespace, Name: h.name, Plan: plan}

			if !plan.NeedsChanges() {
				results = append(results, result)
				continue
			}

			if req.Apply {
				patched := harden.Patch(h.template, plan, resources)
				if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					return h.update(ctx, patched)
				}); err != nil {
					s.logger.Error("failed to apply hardening patch", "kind", h.kind, "namespace", h.namespace, "name", h.name, "error", err)
					writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to patch %s/%s/%s", h.kind, h.namespace, h.name))
					return
				}
				result.Applied = true
			}
			results = append(results, result)
		}
	}

	writeJSON(w, http.StatusOK, hardenResponse{Results: results})
}

// listWorkloads gathers every Deployment, StatefulSet, and DaemonSet in ns
// into a uniform slice of handles.
func (s *Server) listWorkloads(ctx context.Context, ns string) ([]workloadHandle, error) {
	var handles []workloadHandle

	deployments, err := s.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		handles = append(handles, workloadHandle{
			kind: "Deployment", namespace: d.Namespace, name: d.Name,
			template: &d.Spec.Template,
			update: func(ctx context.Context, patched *corev1.PodTemplateSpec) error {
				d.Spec.Template = *patched
				_, err := s.clientset.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{})
				return err
			},
		})
	}

	statefulSets, err := s.clientset.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range statefulSets.Items {
		ss := &statefulSets.Items[i]
		handles = append(handles, workloadHandle{
			kind: "StatefulSet", namespace: ss.Namespace, name: ss.Name,
			template: &ss.Spec.Template,
			update: func(ctx context.Context, patched *corev1.PodTemplateSpec) error {
				ss.Spec.Template = *patched
				_, err := s.clientset.AppsV1().StatefulSets(ns).Update(ctx, ss, metav1.UpdateOptions{})
				return err
			},
		})
	}

	daemonSets, err := s.clientset.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing daemonsets: %w", err)
	}
	for i := range daemonSets.Items {
		ds := &daemonSets.Items[i]
		handles = append(handles, workloadHandle{
			kind: "DaemonSet", namespace: ds.Namespace, name: ds.Name,
			template: &ds.Spec.Template,
			update: func(ctx context.Context, patched *corev1.PodTemplateSpec) error {
				ds.Spec.Template = *patched
				_, err := s.clientset.AppsV1().DaemonSets(ns).Update(ctx, ds, metav1.UpdateOptions{})
				return err
			},
		})
	}

	return handles, nil
}
