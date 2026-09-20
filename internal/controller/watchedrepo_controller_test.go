package controller

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestGitTokenFor_NoRefMeansPublicRepo(t *testing.T) {
	r := &WatchedRepoReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	token, err := r.gitTokenFor(context.Background(), &driftv1alpha1.WatchedRepo{})
	if err != nil || token != "" {
		t.Fatalf("want no token and no error, got %q, %v", token, err)
	}
}

func TestGitTokenFor_ReadsSecretAndTrimsNewline(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git", Namespace: "drift-system"},
		Data:       map[string][]byte{"token": []byte("tok123\n")},
	}
	r := &WatchedRepoReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()}
	wr := &driftv1alpha1.WatchedRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "drift-system"},
		Spec:       driftv1alpha1.WatchedRepoSpec{GitCredentialsSecretRef: &driftv1alpha1.SecretKeyRef{Name: "git"}},
	}
	token, err := r.gitTokenFor(context.Background(), wr)
	if err != nil || token != "tok123" {
		t.Fatalf("want tok123, got %q, %v", token, err)
	}
}

func TestGitTokenFor_Failures(t *testing.T) {
	ns := "drift-system"
	secrets := []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "nokey", Namespace: ns}, Data: map[string][]byte{"other": []byte("x")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "blank", Namespace: ns}, Data: map[string][]byte{"token": []byte(" \n")}},
		// a Secret with the right name but in ANOTHER namespace must not be used
		{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"}, Data: map[string][]byte{"token": []byte("x")}},
	}
	b := fake.NewClientBuilder().WithScheme(testScheme(t))
	for _, s := range secrets {
		b = b.WithObjects(s)
	}
	r := &WatchedRepoReconciler{Client: b.Build()}
	for _, name := range []string{"missing", "nokey", "blank", "elsewhere"} {
		wr := &driftv1alpha1.WatchedRepo{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns},
			Spec:       driftv1alpha1.WatchedRepoSpec{GitCredentialsSecretRef: &driftv1alpha1.SecretKeyRef{Name: name}},
		}
		if token, err := r.gitTokenFor(context.Background(), wr); err == nil || token != "" {
			t.Errorf("secret %q: want an error and no token, got %q, %v", name, token, err)
		}
	}
}

func TestGitAuthEnv_RefusesPlainHTTP(t *testing.T) {
	if _, err := gitAuthEnv("http://github.com/o/r.git", "tok"); err == nil {
		t.Fatal("a token must never be sent over plain http")
	}
	if _, err := gitAuthEnv("git@github.com:o/r.git", "tok"); err == nil {
		t.Fatal("only https URLs are supported with a token")
	}
}

func TestGitAuthEnv_TokenOnlyInEnvAndScopedToRepo(t *testing.T) {
	env, err := gitAuthEnv("https://github.com/o/r.git", "tok")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_KEY_0=http.https://github.com/o/r.git.extraHeader") {
		t.Errorf("header must be scoped to the repo URL, got:\n%s", joined)
	}
	if !strings.Contains(joined, "Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))) {
		t.Errorf("missing basic auth header, got:\n%s", joined)
	}
}

func TestScrubToken(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:SECRET"))
	out := scrubToken("fatal: bad SECRET and Authorization: Basic "+basic, "SECRET")
	if strings.Contains(out, "SECRET") || strings.Contains(out, basic) {
		t.Errorf("token leaked: %q", out)
	}
	if got := scrubToken("nothing here", ""); got != "nothing here" {
		t.Errorf("empty token must leave output alone, got %q", got)
	}
}

// The real check: run an actual `git clone` against a fake HTTPS server and see
// exactly what git sends. The clone itself fails (it isn't a real repo) - what
// matters is the request.
func TestCloneRepo_SendsTokenOnlyWhenGiven_AndNeverLeaksIt(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	url := srv.URL + "/o/r.git"

	// with a token: git offers it, and the error text must not contain it
	_, _, err := cloneRepo(url, "", "SECRETTOKEN")
	if err == nil {
		t.Fatal("expected the clone to fail")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:SECRETTOKEN"))
	mu.Lock()
	sawToken := false
	for _, a := range auths {
		if a == want {
			sawToken = true
		}
	}
	mu.Unlock()
	if !sawToken {
		t.Errorf("git never sent the token; saw %q", auths)
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("x-access-token:SECRETTOKEN"))) {
		t.Errorf("token leaked into the error: %v", err)
	}

	// without a token: no Authorization header at all, and it fails fast
	mu.Lock()
	auths = nil
	mu.Unlock()
	if _, _, err := cloneRepo(url, "", ""); err == nil {
		t.Fatal("expected the clone to fail")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range auths {
		if a != "" {
			t.Errorf("no token was configured but git sent Authorization %q", a)
		}
	}
}
