package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	driftv1alpha1 "github.com/Orhevba/gitops-drift-detector/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := driftv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestRestConfigFor_DefaultsToManagerConfig(t *testing.T) {
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	managerCfg := &rest.Config{Host: "https://manager-cluster.example"}
	r := &WatchedRepoReconciler{Client: fakeClient, RestConfig: managerCfg}

	cfg, err := r.restConfigFor(context.Background(), &driftv1alpha1.WatchedRepo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != managerCfg {
		t.Error("expected the manager's own config when KubeconfigSecretRef is nil")
	}
}

func TestRestConfigFor_MissingSecret(t *testing.T) {
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &WatchedRepoReconciler{Client: fakeClient, RestConfig: &rest.Config{}}

	wr := &driftv1alpha1.WatchedRepo{}
	wr.Namespace = "drift-system"
	wr.Spec.KubeconfigSecretRef = &driftv1alpha1.SecretKeyRef{Name: "missing-secret"}

	if _, err := r.restConfigFor(context.Background(), wr); err == nil {
		t.Fatal("expected an error for a missing secret, got nil")
	}
}

func TestRestConfigFor_MissingKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-kubeconfig", Namespace: "drift-system"},
		Data:       map[string][]byte{"wrong-key": []byte("irrelevant")},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	r := &WatchedRepoReconciler{Client: fakeClient, RestConfig: &rest.Config{}}

	wr := &driftv1alpha1.WatchedRepo{}
	wr.Namespace = "drift-system"
	wr.Spec.KubeconfigSecretRef = &driftv1alpha1.SecretKeyRef{Name: "cluster-kubeconfig"}

	if _, err := r.restConfigFor(context.Background(), wr); err == nil {
		t.Fatal("expected an error for a missing key, got nil")
	}
}

func TestRestConfigFor_ParsesValidKubeconfig(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
  - name: remote
    cluster:
      server: https://remote-cluster.example:6443
      insecure-skip-tls-verify: true
contexts:
  - name: remote
    context:
      cluster: remote
      user: remote
current-context: remote
users:
  - name: remote
    user:
      token: fake-token
`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-kubeconfig", Namespace: "drift-system"},
		Data:       map[string][]byte{"kubeconfig": []byte(kubeconfig)},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	r := &WatchedRepoReconciler{Client: fakeClient, RestConfig: &rest.Config{}}

	wr := &driftv1alpha1.WatchedRepo{}
	wr.Namespace = "drift-system"
	wr.Spec.KubeconfigSecretRef = &driftv1alpha1.SecretKeyRef{Name: "cluster-kubeconfig"}

	cfg, err := r.restConfigFor(context.Background(), wr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "https://remote-cluster.example:6443"; cfg.Host != want {
		t.Errorf("Host = %q, want %q", cfg.Host, want)
	}
}

func TestRestConfigFor_DefaultsKeyToKubeconfig(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-kubeconfig", Namespace: "drift-system"},
		Data: map[string][]byte{"kubeconfig": []byte(`apiVersion: v1
kind: Config
clusters:
  - name: remote
    cluster:
      server: https://remote-cluster.example:6443
contexts:
  - name: remote
    context:
      cluster: remote
      user: remote
current-context: remote
users:
  - name: remote
    user:
      token: fake-token
`)},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	r := &WatchedRepoReconciler{Client: fakeClient, RestConfig: &rest.Config{}}

	wr := &driftv1alpha1.WatchedRepo{}
	wr.Namespace = "drift-system"
	// Key intentionally left unset — should default to "kubeconfig".
	wr.Spec.KubeconfigSecretRef = &driftv1alpha1.SecretKeyRef{Name: "cluster-kubeconfig"}

	if _, err := r.restConfigFor(context.Background(), wr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
