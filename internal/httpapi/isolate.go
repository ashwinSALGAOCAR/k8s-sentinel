package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/ashwinSALGAOCAR/k8s-sentinel/internal/netpol"
)

// protectedNamespaces can never be a target of isolation, matching
// README.md's "Namespaces the tool will never touch" decision. Kept as a
// package-level var (not const — Go doesn't allow const slices) so it's
// easy to find and extend.
var protectedNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
	"calico-system":   true,
	"tigera-operator": true,
}

type isolateResponse struct {
	ID       string   `json:"id"`
	Warnings []string `json:"warnings,omitempty"`
}

type isolateListEntry struct {
	ID string             `json:"id"`
	A  netpol.WorkloadRef `json:"a"`
	B  netpol.WorkloadRef `json:"b"`
}

// handleIsolateCreate implements POST /isolate.
func (s *Server) handleIsolateCreate(w http.ResponseWriter, r *http.Request) {
	var req netpol.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	if err := validateWorkloadRefs(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	var warnings []string
	for _, ref := range []netpol.WorkloadRef{req.A, req.B} {
		conflicts, err := s.conflictingPolicies(ctx, ref.Namespace)
		if err != nil {
			s.logger.Error("conflict check failed", "namespace", ref.Namespace, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to check for pre-existing NetworkPolicies")
			return
		}
		if len(conflicts) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"namespace %q already has %d pre-existing NetworkPolicy object(s) not managed by k8s-sentinel; "+
					"since NetworkPolicy rules are additive, those may grant broader access than this isolation intends (see README.md)",
				ref.Namespace, len(conflicts)))
		}
	}

	for _, policy := range netpol.Policies(req) {
		existing, err := s.clientset.NetworkingV1().NetworkPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if _, err := s.clientset.NetworkingV1().NetworkPolicies(policy.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
				s.logger.Error("failed to create NetworkPolicy", "namespace", policy.Namespace, "name", policy.Name, "error", err)
				writeError(w, http.StatusInternalServerError, "failed to create NetworkPolicy")
				return
			}
		case err != nil:
			s.logger.Error("failed to check for existing NetworkPolicy", "namespace", policy.Namespace, "name", policy.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to check for existing NetworkPolicy")
			return
		default:
			// Idempotency: the same isolation request issued twice should
			// update the existing objects, not create duplicates or error.
			policy.ResourceVersion = existing.ResourceVersion
			if _, err := s.clientset.NetworkingV1().NetworkPolicies(policy.Namespace).Update(ctx, policy, metav1.UpdateOptions{}); err != nil {
				s.logger.Error("failed to update NetworkPolicy", "namespace", policy.Namespace, "name", policy.Name, "error", err)
				writeError(w, http.StatusInternalServerError, "failed to update NetworkPolicy")
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, isolateResponse{ID: req.ID(), Warnings: warnings})
}

// handleIsolateDelete implements DELETE /isolate/{id} — restores prior
// connectivity by deleting only the NetworkPolicy objects this tool
// created for that specific isolation, found by label rather than by
// remembering namespaces anywhere (see README's "State tracking for
// 'undo'" decision: Kubernetes itself is the source of truth, not an
// in-memory map).
func (s *Server) handleIsolateDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	policies, err := s.listManagedPolicies(ctx, id)
	if err != nil {
		s.logger.Error("failed to list NetworkPolicies for deletion", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list NetworkPolicies")
		return
	}
	if len(policies) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no active isolation found with id %q", id))
		return
	}

	for _, p := range policies {
		if err := s.clientset.NetworkingV1().NetworkPolicies(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			s.logger.Error("failed to delete NetworkPolicy", "namespace", p.Namespace, "name", p.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to delete NetworkPolicy")
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleIsolateList implements GET /isolate — reads live cluster state,
// not memory, for the same reason as the delete handler above.
func (s *Server) handleIsolateList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.listManagedPolicies(ctx, "")
	if err != nil {
		s.logger.Error("failed to list NetworkPolicies", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list NetworkPolicies")
		return
	}

	byID := map[string][]netpol.WorkloadRef{}
	for _, p := range all {
		id := p.Labels[netpol.IsolationIDLabel]
		byID[id] = append(byID[id], netpol.WorkloadRef{Namespace: p.Namespace, Selector: p.Spec.PodSelector.MatchLabels})
	}

	entries := make([]isolateListEntry, 0, len(byID))
	for id, refs := range byID {
		entry := isolateListEntry{ID: id}
		if len(refs) > 0 {
			entry.A = refs[0]
		}
		if len(refs) > 1 {
			entry.B = refs[1]
		}
		entries = append(entries, entry)
	}

	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) listManagedPolicies(ctx context.Context, isolationID string) ([]*networkingv1.NetworkPolicy, error) {
	selector := labels.Set{netpol.ManagedByLabel: netpol.ManagedByValue}
	if isolationID != "" {
		selector[netpol.IsolationIDLabel] = isolationID
	}

	list, err := s.clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: selector.AsSelector().String(),
	})
	if err != nil {
		return nil, err
	}

	out := make([]*networkingv1.NetworkPolicy, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// conflictingPolicies returns pre-existing NetworkPolicy objects in the
// given namespace that this tool doesn't own. This is a deliberately
// simple, conservative heuristic — not full selector-overlap analysis
// (determining whether two arbitrary label selectors could ever match the
// same pod is a much harder problem) — see README's "Pre-existing
// conflicting NetworkPolicies" decision for why this honest approximation
// was chosen over either silently ignoring the risk or implementing full
// policy reconciliation.
func (s *Server) conflictingPolicies(ctx context.Context, namespace string) ([]*networkingv1.NetworkPolicy, error) {
	list, err := s.clientset.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var foreign []*networkingv1.NetworkPolicy
	for i := range list.Items {
		p := &list.Items[i]
		if p.Labels[netpol.ManagedByLabel] == netpol.ManagedByValue {
			continue // ours — not a conflict, possibly this very request's own policy on an update
		}
		foreign = append(foreign, p)
	}
	return foreign, nil
}

func validateWorkloadRefs(req netpol.Request) error {
	for _, ref := range []netpol.WorkloadRef{req.A, req.B} {
		if ref.Namespace == "" {
			return errors.New("both a.namespace and b.namespace are required")
		}
		if len(ref.Selector) == 0 {
			return fmt.Errorf("selector is required for namespace %q", ref.Namespace)
		}
		if protectedNamespaces[ref.Namespace] {
			return fmt.Errorf("namespace %q is protected and cannot be isolated", ref.Namespace)
		}
	}
	if req.A.Namespace == req.B.Namespace && labels.Set(req.A.Selector).AsSelector().String() == labels.Set(req.B.Selector).AsSelector().String() {
		return errors.New("a and b must refer to different workloads")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
