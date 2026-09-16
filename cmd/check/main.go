// gitops-drift-detector (check) is a one-shot CLI that compares a directory
// of Kubernetes manifests against a live cluster. For continuous checking,
// see cmd/manager, which runs the same logic as an in-cluster controller.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tygacookie/gitops-drift-detector/internal/drift"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
)

func main() {
	manifestsDir := flag.String("manifests", "", "directory of YAML manifests (the desired state)")
	namespace := flag.String("namespace", "", "namespace to check against")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG env or ~/.kube/config)")
	ignoreFlag := flag.String("ignore", "", "comma-separated Kind/name entries to exclude from orphan detection, e.g. \"Secret/some-webhook-cert\"")
	flag.Parse()

	if *manifestsDir == "" || *namespace == "" {
		fmt.Fprintln(os.Stderr, "usage: check -manifests <dir> -namespace <ns> [-kubeconfig <path>] [-ignore <Kind/name,...>]")
		os.Exit(2)
	}

	var ignore []string
	for _, entry := range strings.Split(*ignoreFlag, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			ignore = append(ignore, entry)
		}
	}

	config, err := buildKubeConfig(*kubeconfig)
	if err != nil {
		fatal("failed to load kube config: %v", err)
	}

	result, err := drift.Check(context.Background(), config, *manifestsDir, *namespace, ignore)
	if err != nil {
		fatal("%v", err)
	}

	for _, e := range result.Entries {
		switch e.Status {
		case drift.StatusInSync:
			fmt.Printf("%s[IN SYNC]%s %s/%s\n", colorGreen, colorReset, e.Kind, e.Name)
		case drift.StatusMissing:
			fmt.Printf("%s[MISSING]%s %s/%s is in Git but not deployed\n", colorRed, colorReset, e.Kind, e.Name)
		case drift.StatusOrphan:
			fmt.Printf("%s[ORPHAN]%s %s/%s is in the cluster but not in Git\n", colorRed, colorReset, e.Kind, e.Name)
		case drift.StatusDrifted:
			fmt.Printf("%s[DRIFTED]%s %s/%s\n    %s\n", colorRed, colorReset, e.Kind, e.Name, e.Detail)
		case drift.StatusError, drift.StatusSkip:
			fmt.Printf("%s[%s]%s %s/%s: %s\n", colorYellow, e.Status, colorReset, e.Kind, e.Name, e.Detail)
		}
	}

	if result.HasDrift() {
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

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
