// Package drift contains the core comparison logic shared by the CLI
// (cmd/check) and the in-cluster controller (internal/controller): given a
// directory of manifests and a target namespace, work out what's missing,
// drifted, or orphaned relative to a live cluster.
package drift

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	sigsyaml "sigs.k8s.io/yaml"
)

type Status string

const (
	StatusInSync  Status = "IN SYNC"
	StatusMissing Status = "MISSING"
	StatusDrifted Status = "DRIFTED"
	StatusOrphan  Status = "ORPHAN"
	StatusError   Status = "ERROR"
	StatusSkip    Status = "SKIP"
)

// Entry describes the result of checking one resource.
type Entry struct {
	Kind   string
	Name   string
	Status Status
	// Detail holds field-level diff info for StatusDrifted, or an error
	// message for StatusError/StatusSkip. Empty otherwise.
	Detail string
}

type Result struct {
	Entries []Entry
}

// HasDrift reports whether anything in the result needs attention.
func (r Result) HasDrift() bool {
	for _, e := range r.Entries {
		if e.Status != StatusInSync {
			return true
		}
	}
	return false
}

// defaultIgnoredOrphans lists resources Kubernetes itself injects into every
// namespace. Nobody puts these in Git, so they'd otherwise show up as a
// false-positive orphan on every single check.
var defaultIgnoredOrphans = map[string]bool{
	"ConfigMap/kube-root-ca.crt": true,
}

// Check compares the manifests in manifestsDir against live state in
// namespace, using cfg to talk to the cluster. extraIgnore entries are
// "Kind/name" strings excluded from orphan detection in addition to the
// built-in defaults.
func Check(ctx context.Context, cfg *rest.Config, manifestsDir, namespace string, extraIgnore []string) (Result, error) {
	var result Result

	ignored := map[string]bool{}
	for k := range defaultIgnoredOrphans {
		ignored[k] = true
	}
	for _, entry := range extraIgnore {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			ignored[entry] = true
		}
	}

	desired, err := loadManifests(manifestsDir)
	if err != nil {
		return result, fmt.Errorf("failed to load manifests: %w", err)
	}
	if len(desired) == 0 {
		return result, fmt.Errorf("no manifests found in %s", manifestsDir)
	}

	c, err := newClients(cfg)
	if err != nil {
		return result, err
	}

	seenByKind := map[schema.GroupVersionKind]map[string]bool{}

	for _, obj := range desired {
		gvk := obj.GroupVersionKind()
		mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			result.Entries = append(result.Entries, Entry{
				Kind: gvk.Kind, Name: obj.GetName(), Status: StatusSkip,
				Detail: fmt.Sprintf("no REST mapping found: %v", err),
			})
			continue
		}

		ri := resourceInterface(c.dyn, mapping, namespace)

		if seenByKind[gvk] == nil {
			seenByKind[gvk] = map[string]bool{}
		}
		seenByKind[gvk][obj.GetName()] = true

		live, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			result.Entries = append(result.Entries, Entry{
				Kind: gvk.Kind, Name: obj.GetName(), Status: StatusMissing,
			})
			continue
		}
		if err != nil {
			result.Entries = append(result.Entries, Entry{
				Kind: gvk.Kind, Name: obj.GetName(), Status: StatusError, Detail: err.Error(),
			})
			continue
		}

		diffs := diffDesiredVsLive(obj.Object, live.Object, "")
		if len(diffs) == 0 {
			result.Entries = append(result.Entries, Entry{
				Kind: gvk.Kind, Name: obj.GetName(), Status: StatusInSync,
			})
			continue
		}

		var detail strings.Builder
		for i, d := range diffs {
			if i > 0 {
				detail.WriteString("; ")
			}
			fmt.Fprintf(&detail, "%s: git=%v cluster=%v", d.path, d.desired, d.live)
		}
		result.Entries = append(result.Entries, Entry{
			Kind: gvk.Kind, Name: obj.GetName(), Status: StatusDrifted, Detail: detail.String(),
		})
	}

	// Orphan check: only for kinds we actually saw in the manifests dir, so
	// we never need a hardcoded list of "interesting" resource types.
	for gvk, applied := range seenByKind {
		mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			continue
		}
		ri := resourceInterface(c.dyn, mapping, namespace)

		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, item := range list.Items {
			key := fmt.Sprintf("%s/%s", gvk.Kind, item.GetName())
			if ignored[key] || applied[item.GetName()] {
				continue
			}
			result.Entries = append(result.Entries, Entry{
				Kind: gvk.Kind, Name: item.GetName(), Status: StatusOrphan,
			})
		}
	}

	return result, nil
}

// clients bundles the cluster clients Check and Remediate both need.
type clients struct {
	dyn    dynamic.Interface
	mapper meta.RESTMapper
}

func newClients(cfg *rest.Config) (*clients, error) {
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to build dynamic client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to build discovery client: %w", err)
	}
	groupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch API resources: %w", err)
	}
	return &clients{dyn: dynClient, mapper: restmapper.NewDiscoveryRESTMapper(groupResources)}, nil
}

// fieldManager identifies this tool's writes in server-side apply, so
// `kubectl get -o yaml --show-managed-fields` shows what it owns.
const fieldManager = "gitops-drift-detector"

// RemediationResult summarizes the actions Remediate took.
type RemediationResult struct {
	Created int
	Fixed   int
	Pruned  int
	Errors  []string
}

func (r RemediationResult) HasErrors() bool { return len(r.Errors) > 0 }

// RemediateOptions controls how far Remediate is allowed to go.
type RemediateOptions struct {
	// PruneOrphans additionally deletes cluster resources that aren't
	// declared in Git. Off by default — deletion is destructive, and a
	// resource merely being "not in this manifest set" isn't strong
	// enough evidence that it's safe to remove.
	PruneOrphans bool
}

// Remediate applies the manifests in manifestsDir to namespace: creating
// resources that are missing, and patching drifted ones back to match Git
// via server-side apply (so it only touches the fields it manages, rather
// than clobbering fields the cluster/other tools own). Callers should
// re-run Check afterward to get the true resulting state rather than
// trusting this call's outcome, since a patch can be accepted by the API
// server without producing the expected result (e.g. an immutable field).
func Remediate(ctx context.Context, cfg *rest.Config, manifestsDir, namespace string, extraIgnore []string, opts RemediateOptions) (RemediationResult, error) {
	var result RemediationResult

	ignored := map[string]bool{}
	for k := range defaultIgnoredOrphans {
		ignored[k] = true
	}
	for _, entry := range extraIgnore {
		if entry = strings.TrimSpace(entry); entry != "" {
			ignored[entry] = true
		}
	}

	desired, err := loadManifests(manifestsDir)
	if err != nil {
		return result, fmt.Errorf("failed to load manifests: %w", err)
	}
	if len(desired) == 0 {
		return result, fmt.Errorf("no manifests found in %s", manifestsDir)
	}

	c, err := newClients(cfg)
	if err != nil {
		return result, err
	}

	seenByKind := map[schema.GroupVersionKind]map[string]bool{}

	for _, obj := range desired {
		gvk := obj.GroupVersionKind()
		mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			continue
		}
		ri := resourceInterface(c.dyn, mapping, namespace)

		if seenByKind[gvk] == nil {
			seenByKind[gvk] = map[string]bool{}
		}
		seenByKind[gvk][obj.GetName()] = true

		live, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
		notFound := apierrors.IsNotFound(err)
		if err != nil && !notFound {
			result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: %v", gvk.Kind, obj.GetName(), err))
			continue
		}

		needsApply := notFound
		if !notFound {
			needsApply = len(diffDesiredVsLive(obj.Object, live.Object, "")) > 0
		}
		if !needsApply {
			continue
		}

		data, err := json.Marshal(obj.Object)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: %v", gvk.Kind, obj.GetName(), err))
			continue
		}
		force := true
		_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: apply failed: %v", gvk.Kind, obj.GetName(), err))
			continue
		}
		if notFound {
			result.Created++
		} else {
			result.Fixed++
		}
	}

	if !opts.PruneOrphans {
		return result, nil
	}

	for gvk, applied := range seenByKind {
		mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			continue
		}
		ri := resourceInterface(c.dyn, mapping, namespace)

		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, item := range list.Items {
			key := fmt.Sprintf("%s/%s", gvk.Kind, item.GetName())
			if ignored[key] || applied[item.GetName()] {
				continue
			}
			if err := ri.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: prune failed: %v", gvk.Kind, item.GetName(), err))
				continue
			}
			result.Pruned++
		}
	}

	return result, nil
}

func resourceInterface(dynClient dynamic.Interface, mapping *meta.RESTMapping, namespace string) dynamic.ResourceInterface {
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return dynClient.Resource(mapping.Resource).Namespace(namespace)
	}
	return dynClient.Resource(mapping.Resource)
}

// loadManifests renders manifests from dir. If dir contains a Kustomize
// entry point (kustomization.yaml/.yml), it's built with the Kustomize
// engine; otherwise every plain YAML file in the directory is parsed as-is.
func loadManifests(dir string) ([]*unstructured.Unstructured, error) {
	if hasKustomization(dir) {
		return loadKustomizeManifests(dir)
	}
	return loadPlainManifests(dir)
}

func hasKustomization(dir string) bool {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func loadKustomizeManifests(dir string) ([]*unstructured.Unstructured, error) {
	fSys := filesys.MakeFsOnDisk()
	result, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fSys, dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build failed: %w", err)
	}

	var objs []*unstructured.Unstructured
	for _, res := range result.Resources() {
		raw, err := res.AsYAML()
		if err != nil {
			return nil, fmt.Errorf("failed to render %s: %w", res.GetName(), err)
		}
		obj := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(raw, obj); err != nil {
			return nil, fmt.Errorf("failed to parse rendered %s: %w", res.GetName(), err)
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

func loadPlainManifests(dir string) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		// path comes from walking a directory this tool was explicitly
		// configured to read (-manifests flag, or a WatchedRepo's own
		// spec.path in a repo it was pointed at) — not an untrusted path
		// from a network-facing input. Whoever controls that input already
		// controls what manifests get applied to the cluster, which is a
		// far more direct capability than reading arbitrary files here.
		f, err := os.Open(path) // #nosec G304,G122 -- see comment above
		if err != nil {
			return err
		}
		defer f.Close()

		decoder := k8syaml.NewYAMLReader(bufio.NewReader(f))
		for {
			raw, err := decoder.Read()
			if err != nil {
				break
			}
			if len(strings.TrimSpace(string(raw))) == 0 {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := sigsyaml.Unmarshal(raw, obj); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if obj.GetKind() == "" {
				continue
			}
			objs = append(objs, obj)
		}
		return nil
	})

	return objs, err
}

type fieldDiff struct {
	path    string
	desired interface{}
	live    interface{}
}

// diffDesiredVsLive walks only the fields present in `desired` and compares
// them against `live`. Fields the cluster adds on its own (status, defaults,
// etc.) are intentionally ignored — Git is the source of truth, not a mirror
// of everything the API server fills in.
func diffDesiredVsLive(desired, live map[string]interface{}, prefix string) []fieldDiff {
	var diffs []fieldDiff
	for k, dv := range desired {
		if isIgnoredField(prefix, k) {
			continue
		}
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		lv, ok := live[k]
		if !ok {
			diffs = append(diffs, fieldDiff{path: path, desired: dv, live: nil})
			continue
		}
		switch dvTyped := dv.(type) {
		case map[string]interface{}:
			lvTyped, ok := lv.(map[string]interface{})
			if !ok {
				diffs = append(diffs, fieldDiff{path: path, desired: dv, live: lv})
				continue
			}
			diffs = append(diffs, diffDesiredVsLive(dvTyped, lvTyped, path)...)
		case []interface{}:
			lvTyped, ok := lv.([]interface{})
			if !ok || len(dvTyped) != len(lvTyped) {
				diffs = append(diffs, fieldDiff{path: path, desired: dv, live: lv})
				continue
			}
			diffs = append(diffs, diffList(dvTyped, lvTyped, path)...)
		default:
			if fmt.Sprintf("%v", dv) != fmt.Sprintf("%v", lv) {
				diffs = append(diffs, fieldDiff{path: path, desired: dv, live: lv})
			}
		}
	}
	return diffs
}

// diffList compares list elements index-by-index (Kubernetes doesn't
// reorder list fields like containers/ports/volumes). Elements that are
// themselves objects (e.g. containers) are diffed with the same
// subset-of-desired-fields semantics as diffDesiredVsLive, so cluster-added
// defaults inside list items (imagePullPolicy, resources, etc.) don't count
// as drift either.
func diffList(desired, live []interface{}, prefix string) []fieldDiff {
	var diffs []fieldDiff
	for i := range desired {
		path := fmt.Sprintf("%s[%d]", prefix, i)
		dMap, dIsMap := desired[i].(map[string]interface{})
		lMap, lIsMap := live[i].(map[string]interface{})
		if dIsMap && lIsMap {
			diffs = append(diffs, diffDesiredVsLive(dMap, lMap, path)...)
			continue
		}
		if fmt.Sprintf("%v", desired[i]) != fmt.Sprintf("%v", live[i]) {
			diffs = append(diffs, fieldDiff{path: path, desired: desired[i], live: live[i]})
		}
	}
	return diffs
}

func isIgnoredField(prefix, key string) bool {
	if prefix == "metadata" {
		switch key {
		case "resourceVersion", "uid", "generation", "creationTimestamp",
			"managedFields", "selfLink", "annotations":
			return true
		}
	}
	if prefix == "" && key == "status" {
		return true
	}
	return false
}
