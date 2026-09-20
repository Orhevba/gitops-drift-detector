package drift

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestLoadManifests_PlainMultiDocFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "demo.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-app
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-config
`)

	objs, err := loadManifests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objs))
	}
	if objs[0].GetKind() != "Deployment" || objs[0].GetName() != "demo-app" {
		t.Errorf("unexpected first object: %s/%s", objs[0].GetKind(), objs[0].GetName())
	}
	if objs[1].GetKind() != "ConfigMap" || objs[1].GetName() != "demo-config" {
		t.Errorf("unexpected second object: %s/%s", objs[1].GetKind(), objs[1].GetName())
	}
}

func TestLoadManifests_MultipleFilesSkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	writeFile(t, filepath.Join(dir, "b.yml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n")
	writeFile(t, filepath.Join(dir, "ignore-me.txt"), "not yaml")

	objs, err := loadManifests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects (non-YAML file should be skipped), got %d", len(objs))
	}
}

func TestLoadManifests_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	objs, err := loadManifests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("expected 0 objects, got %d", len(objs))
	}
}

func TestLoadManifests_Kustomize(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "resources:\n  - deployment.yaml\n")
	writeFile(t, filepath.Join(dir, "deployment.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: demo-app
  template:
    metadata:
      labels:
        app: demo-app
    spec:
      containers:
        - name: demo-app
          image: nginx:1.25
`)

	objs, err := loadManifests(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("expected 1 object, got %d", len(objs))
	}
	if objs[0].GetKind() != "Deployment" || objs[0].GetName() != "demo-app" {
		t.Errorf("unexpected object: %s/%s", objs[0].GetKind(), objs[0].GetName())
	}
}

func TestLoadManifests_KustomizeOverlayPatchIsApplied(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	overlay := filepath.Join(dir, "overlay")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(base, "kustomization.yaml"), "resources:\n  - deployment.yaml\n")
	writeFile(t, filepath.Join(base, "deployment.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: demo-app
  template:
    metadata:
      labels:
        app: demo-app
    spec:
      containers:
        - name: demo-app
          image: nginx:1.25
`)
	writeFile(t, filepath.Join(overlay, "kustomization.yaml"), `resources:
  - ../base
replicas:
  - name: demo-app
    count: 3
`)

	objs, err := loadManifests(overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("expected 1 object, got %d", len(objs))
	}

	replicas, found, err := unstructured.NestedInt64(objs[0].Object, "spec", "replicas")
	if err != nil {
		t.Fatalf("unexpected error reading spec.replicas: %v", err)
	}
	if !found {
		t.Fatal("expected spec.replicas to be set")
	}
	if replicas != 3 {
		t.Errorf("replicas = %d, want 3 (overlay patch should have overridden the base's 1)", replicas)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func TestOrphanScanApplies_OnlyToNamespacedKinds(t *testing.T) {
	if !orphanScanApplies(meta.RESTScopeNamespace) {
		t.Error("namespaced kinds (Deployment, Service...) must be scanned for orphans")
	}
	if orphanScanApplies(meta.RESTScopeRoot) {
		t.Error("cluster-scoped kinds (Namespace...) must not be: every other namespace would look like an orphan")
	}
}
