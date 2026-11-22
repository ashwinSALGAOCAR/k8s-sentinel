// Package k8sclient builds a Kubernetes clientset that works both inside a
// cluster (the normal runtime case, via the in-cluster service account) and
// outside one (local development, via the caller's kubeconfig).
package k8sclient

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// New returns a Kubernetes clientset. It tries in-cluster configuration
// first (the service account token/CA mounted into every pod), and falls
// back to a kubeconfig file for local development against a cluster such as
// kind.
//
// kubeconfigOverride, if non-empty, takes precedence over both the
// KUBECONFIG environment variable and the default ~/.kube/config path —
// useful for tests that need to target a specific cluster explicitly.
func New(kubeconfigOverride string) (*kubernetes.Clientset, error) {
	cfg, err := restConfig(kubeconfigOverride)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes rest config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes clientset: %w", err)
	}
	return clientset, nil
}

func restConfig(kubeconfigOverride string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	kubeconfigPath := kubeconfigOverride
	if kubeconfigPath == "" {
		kubeconfigPath = os.Getenv("KUBECONFIG")
	}
	if kubeconfigPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("not running in-cluster and could not determine home directory for default kubeconfig: %w", err)
		}
		kubeconfigPath = filepath.Join(home, ".kube", "config")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig from %q: %w", kubeconfigPath, err)
	}
	return cfg, nil
}
