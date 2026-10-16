package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
)

// envtestTestEnv is the envtest control plane shared across all integration
// tests in this package. Started once in TestMain.
//
//nolint:gochecknoglobals // TestMain-held singleton, not a package-level mutable global.
var envtestTestEnv *envtest.Environment

// envtestK8sClient is a direct client to the envtest API server, usable for
// fixture setup and assertions outside the controller manager.
//
//nolint:gochecknoglobals // TestMain-held singleton, not a package-level mutable global.
var envtestK8sClient client.Client

// envtestCfg is the rest.Config from the envtest API server.
//
//nolint:gochecknoglobals // TestMain-held singleton, not a package-level mutable global.
var envtestCfg *rest.Config

func TestMain(m *testing.M) {
	if os.Getenv("SKIP_ENVTEST") != "" {
		os.Exit(m.Run())
	}

	crdPath := filepath.Join("..", "..", "config", "crd")

	envtestTestEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
		Scheme:                envtestScheme(),
	}

	cfg, err := envtestTestEnv.Start()
	if err != nil {
		if isEnvtestAssetsMissing(err) {
			envtestTestEnv = nil

			fmt.Fprintf(os.Stderr, "skipping envtest integration tests: %v\n", err)

			os.Exit(m.Run())
		}

		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)

		os.Exit(1)
	}

	envtestCfg = cfg

	envtestK8sClient, err = client.New(cfg, client.Options{Scheme: envtestTestEnv.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create envtest client: %v\n", err)

		_ = envtestTestEnv.Stop()

		os.Exit(1)
	}

	code := m.Run()

	err = envtestTestEnv.Stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
	}

	os.Exit(code)
}

// envtestScheme builds the runtime scheme with all CRDs registered: byot
// infra types, CAPI core types, Kubernetes core types, and apiextensions
// types needed by envtest itself.
func envtestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()

	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	must(infrav1.AddToScheme(scheme))
	must(clusterv1.AddToScheme(scheme))
	must(corev1.AddToScheme(scheme))
	must(apiextensionsv1.AddToScheme(scheme))

	return scheme
}

// isEnvtestAssetsMissing reports whether the error indicates missing
// kubebuilder/envtest binaries. Lets unit tests proceed on machines without
// setup-envtest installed.
func isEnvtestAssetsMissing(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "unable to find required binaries") ||
		strings.Contains(msg, "KUBEBUILDER_ASSETS") ||
		strings.Contains(msg, "no such file or directory")
}

// startManager launches a controller manager with the given reconcilers
// registered against the envtest API server. Returns the manager and a
// stop function.
func startManager(
	t *testing.T,
	register func(ctrl.Manager) error,
) (ctrl.Manager, func()) {
	t.Helper()

	mgr, err := ctrl.NewManager(envtestCfg, ctrl.Options{
		Scheme:  envtestTestEnv.Scheme,
		Metrics: server.Options{BindAddress: "0"},
		WebhookServer: ctrlwebhook.NewServer(
			ctrlwebhook.Options{Port: 0},
		),
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	err = register(mgr)
	if err != nil {
		t.Fatalf("failed to register controllers: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		err := mgr.Start(ctx)
		if err != nil {
			t.Errorf("manager stopped with error: %v", err)
		}
	}()

	return mgr, func() {
		cancel()

		time.Sleep(100 * time.Millisecond)
	}
}

// newNamespace creates a corev1 Namespace in the envtest cluster and
// returns its name.
func newNamespace(t *testing.T, name string) string {
	t.Helper()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}

	err := envtestK8sClient.Create(context.Background(), ns)
	if err != nil {
		t.Fatalf("failed to create namespace %s: %v", name, err)
	}

	return name
}

// randomSuffix returns a short hex suffix for unique test resource names.
func randomSuffix() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
