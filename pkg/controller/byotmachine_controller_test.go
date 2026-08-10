package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func objectKey(name string, namespace string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: namespace}
}

func clusterKey(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}

func newByotMachine(name string, namespace string, publicIP string) *infrav1.ByotMachine {
	return &infrav1.ByotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "test-cluster",
			},
		},
		Spec: infrav1.ByotMachineSpec{
			PublicIP: publicIP,
		},
	}
}

func TestByotMachineReconcileAddsFinalizer(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newByotMachine("test-machine", "default", "203.0.113.10")

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine).
		WithStatusSubresource(byotMachine).
		Build()

	reconciler := NewByotMachineReconciler(client)

	// No Machine owner yet: the reconciler adds the finalizer and requeues.
	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Equal(t, requeueAfterBootstrap, result.RequeueAfter)

	updated := &infrav1.ByotMachine{}

	err = client.Get(t.Context(), clusterKey(byotMachine), updated)
	require.NoError(t, err)
	assert.Contains(t, updated.Finalizers, byotMachineFinalizer)
	assert.False(t, updated.Status.Ready)
	assert.Nil(t, updated.Spec.ProviderID)
}

func newAdoptedByotMachine(name string, namespace string, publicIP string, configHash string) *infrav1.ByotMachine {
	machine := newByotMachine(name, namespace, publicIP)
	providerID := infrav1.ProviderIDPrefix + publicIP
	machine.Spec.ProviderID = &providerID
	machine.Status.Ready = true
	machine.Status.LastAppliedConfigSHA = configHash
	machine.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       name,
			UID:        "machine-uid",
		},
	}

	return machine
}

func newOwningMachine(name string, namespace string, dataSecretName string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       "machine-uid",
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "test-cluster",
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "test-cluster",
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: &dataSecretName,
			},
		},
	}
}

func newBootstrapSecret(name string, namespace string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			bootstrapSecretDataKey: data,
		},
	}
}

func sha256HexOf(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestByotMachineReconcileNoOpWhenConfigUnchanged(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	configData := []byte("cluster:\n  controlPlane: {}\n")
	currentHash := sha256HexOf(configData)

	byotMachine := newAdoptedByotMachine("test-machine", "default", "203.0.113.10", currentHash)
	machine := newOwningMachine("test-machine", "default", "test-bootstrap")
	secret := newBootstrapSecret("test-bootstrap", "default", configData)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, machine, secret).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)
}

func TestByotMachineReconcileRequeuesWhenClusterTalosConfigMissing(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newAdoptedByotMachine("test-machine", "default", "203.0.113.10", "stale-hash")
	machine := newOwningMachine("test-machine", "default", "test-bootstrap")
	secret := newBootstrapSecret("test-bootstrap", "default", []byte("new-config"))

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, machine, secret).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)

	// Config changed; machine is already configured, so it needs the cluster
	// talosconfig, which does not exist yet: expect a requeue.
	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Equal(t, requeueAfterBootstrap, result.RequeueAfter)
}

func TestByotMachineReconcileDeleteIsNoOp(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newByotMachine("test-machine", "default", "203.0.113.10")
	byotMachine.Finalizers = []string{byotMachineFinalizer}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine).
		WithStatusSubresource(byotMachine).
		Build()

	reconciler := NewByotMachineReconciler(client)

	err = client.Delete(t.Context(), byotMachine)
	require.NoError(t, err)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), result.RequeueAfter)

	// The finalizer is removed without touching the host, so the object is gone.
	deleted := &infrav1.ByotMachine{}
	getErr := client.Get(t.Context(), clusterKey(byotMachine), deleted)
	assert.Error(t, getErr)
}
