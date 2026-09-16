// gitops-drift-detector compares a directory of Kubernetes manifests (the
// "desired state" from Git) against what is actually running in a cluster,
// and reports any drift.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
)

// defaultIgnoredOrphans lists resources Kubernetes itself injects into every
// namespace. Nobody puts these in Git, so they'd otherwise show up as a
// false-positive orphan in every single run.
var defaultIgnoredOrphans = map[string]bool{
	"ConfigMap/kube-root-ca.crt": true,
}

func main() {
	manifestsDir := flag.String("manifests", "", "directory of YAML manifests (the desired state)")
	namespace := flag.String("namespace", "", "namespace to check against")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG env or ~/.kube/config)")
	ignoreFlag := flag.String("ignore", "", "comma-separated Kind/name entries to exclude from orphan detection, e.g. \"Secret/some-webhook-cert\"")
	flag.Parse()

	ignoredOrphans := map[string]bool{}
	for k := range defaultIgnoredOrphans {
		ignoredOrphans[k] = true
	}
	for _, entry := range strings.Split(*ignoreFlag, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			ignoredOrphans[entry] = true
		}
	}

	if *manifestsDir == "" || *namespace == "" {
		fmt.Fprintln(os.Stderr, "usage: gitops-drift-detector -manifests <dir> -namespace <ns> [-kubeconfig <path>]")
		os.Exit(2)
	}

	desired, err := loadManifests(*manifestsDir)
	if err != nil {
		fatal("failed to load manifests: %v", err)
	}
	if len(desired) == 0 {
		fatal("no manifests found in %s", *manifestsDir)
	}

	config, err := buildKubeConfig(*kubeconfig)
	if err != nil {
		fatal("failed to load kube config: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fatal("failed to build dynamic client: %v", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		fatal("failed to build discovery client: %v", err)
	}
	groupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		fatal("failed to fetch API resources: %v", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	driftFound := false
	seenByKind := map[schema.GroupVersionKind]map[string]bool{}

	for _, obj := range desired {
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			fmt.Printf("%s[SKIP]%s %s: no REST mapping found (%v)\n", colorYellow, colorReset, describe(obj), err)
			continue
		}

		var ri dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ri = dynClient.Resource(mapping.Resource).Namespace(*namespace)
		} else {
			ri = dynClient.Resource(mapping.Resource)
		}

		if seenByKind[gvk] == nil {
			seenByKind[gvk] = map[string]bool{}
		}
		seenByKind[gvk][obj.GetName()] = true

		live, err := ri.Get(context.Background(), obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fmt.Printf("%s[MISSING]%s %s is in Git but not deployed\n", colorRed, colorReset, describe(obj))
			driftFound = true
			continue
		}
		if err != nil {
			fmt.Printf("%s[ERROR]%s %s: %v\n", colorYellow, colorReset, describe(obj), err)
			continue
		}

		diffs := diffDesiredVsLive(obj.Object, live.Object, "")
		if len(diffs) == 0 {
			fmt.Printf("%s[IN SYNC]%s %s\n", colorGreen, colorReset, describe(obj))
			continue
		}

		driftFound = true
		fmt.Printf("%s[DRIFTED]%s %s\n", colorRed, colorReset, describe(obj))
		for _, d := range diffs {
			fmt.Printf("    %s: git=%v cluster=%v\n", d.path, d.desired, d.live)
		}
	}

	// Orphan check: only for kinds we actually saw in the manifests dir,
	// so we never need a hardcoded list of "interesting" resource types.
	for gvk, applied := range seenByKind {
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			continue
		}
		var ri dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ri = dynClient.Resource(mapping.Resource).Namespace(*namespace)
		} else {
			ri = dynClient.Resource(mapping.Resource)
		}
		list, err := ri.List(context.Background(), metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, item := range list.Items {
			key := fmt.Sprintf("%s/%s", gvk.Kind, item.GetName())
			if ignoredOrphans[key] {
				continue
			}
			if !applied[item.GetName()] {
				driftFound = true
				fmt.Printf("%s[ORPHAN]%s %s is in the cluster but not in Git\n",
					colorRed, colorReset, key)
			}
		}
	}

	if driftFound {
		os.Exit(1)
	}
	fmt.Printf("%sno drift detected%s\n", colorGreen, colorReset)
}

func buildKubeConfig(explicitPath string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if explicitPath != "" {
		rules.ExplicitPath = explicitPath
	}
	overrides := &clientcmd.ConfigOverrides{}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func loadManifests(dir string) ([]*unstructured.Unstructured, error) {
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

		f, err := os.Open(path)
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
		default:
			if fmt.Sprintf("%v", dv) != fmt.Sprintf("%v", lv) {
				diffs = append(diffs, fieldDiff{path: path, desired: dv, live: lv})
			}
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

func describe(obj *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
