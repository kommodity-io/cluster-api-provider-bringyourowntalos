package controller

import (
	"context"
	"fmt"

	corev1api "k8s.io/api/core/v1"
	policyv1api "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1typed "k8s.io/client-go/kubernetes/typed/core/v1"
	policyv1typed "k8s.io/client-go/kubernetes/typed/policy/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// workloadClientset is the subset of the Kubernetes clientset used by the
// byot deletion path to drain and delete a workload Node. It is an interface
// so the node-cleanup logic is unit-testable with a fake clientset.
type workloadClientset interface {
	Nodes() workloadNodeInterface
	Pods(namespace string) workloadPodInterface
	Evictions(namespace string) workloadEvictionInterface
}

type workloadNodeInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1api.NodeList, error)
	Patch(
		ctx context.Context,
		name string,
		pt types.PatchType,
		data []byte,
		opts metav1.PatchOptions,
	) (*corev1api.Node, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

type workloadPodInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1api.PodList, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

type workloadEvictionInterface interface {
	Evict(ctx context.Context, eviction *policyv1api.Eviction) error
}

// realWorkloadClientset adapts a kubernetes.Interface to workloadClientset.
type realWorkloadClientset struct{ inner kubernetes.Interface }

func newWorkloadClientset(kubeconfigBytes []byte) (workloadClientset, error) {
	cfg, err := clientcmd.NewClientConfigFromBytes(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	restConfig, err := cfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build rest config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubernetes clientset: %w", err)
	}

	return &realWorkloadClientset{inner: clientset}, nil
}

func (c *realWorkloadClientset) Nodes() workloadNodeInterface {
	return &realNodeInterface{inner: c.inner.CoreV1().Nodes()}
}

func (c *realWorkloadClientset) Pods(namespace string) workloadPodInterface {
	return &realPodInterface{inner: c.inner.CoreV1().Pods(namespace)}
}

func (c *realWorkloadClientset) Evictions(namespace string) workloadEvictionInterface {
	return &realEvictionInterface{inner: c.inner.PolicyV1().Evictions(namespace)}
}

type realNodeInterface struct{ inner corev1typed.NodeInterface }

func (n *realNodeInterface) List(
	ctx context.Context,
	opts metav1.ListOptions,
) (*corev1api.NodeList, error) {
	return n.inner.List(ctx, opts)
}

func (n *realNodeInterface) Patch(
	ctx context.Context,
	name string,
	pt types.PatchType,
	data []byte,
	opts metav1.PatchOptions,
) (*corev1api.Node, error) {
	return n.inner.Patch(ctx, name, pt, data, opts)
}

func (n *realNodeInterface) Delete(
	ctx context.Context,
	name string,
	opts metav1.DeleteOptions,
) error {
	return n.inner.Delete(ctx, name, opts)
}

type realPodInterface struct{ inner corev1typed.PodInterface }

func (p *realPodInterface) List(
	ctx context.Context,
	opts metav1.ListOptions,
) (*corev1api.PodList, error) {
	return p.inner.List(ctx, opts)
}

func (p *realPodInterface) Delete(
	ctx context.Context,
	name string,
	opts metav1.DeleteOptions,
) error {
	return p.inner.Delete(ctx, name, opts)
}

type realEvictionInterface struct{ inner policyv1typed.EvictionInterface }

func (e *realEvictionInterface) Evict(
	ctx context.Context,
	eviction *policyv1api.Eviction,
) error {
	return e.inner.Evict(ctx, eviction)
}
