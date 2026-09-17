// Package controller reconciles WatchedRepo resources: for each one, it
// clones the referenced Git repo, runs the same drift.Check used by the
// CLI, and writes the result to the resource's status.
package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	driftv1alpha1 "github.com/Orhevba/gitops-drift-detector/api/v1alpha1"
	"github.com/Orhevba/gitops-drift-detector/internal/drift"
	"github.com/Orhevba/gitops-drift-detector/internal/notify"
)

const defaultPollInterval = 5 * time.Minute

// WatchedRepoReconciler reconciles a WatchedRepo object.
type WatchedRepoReconciler struct {
	client.Client
	RestConfig *rest.Config
	Notifier   notify.Notifier
}

func (r *WatchedRepoReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var wr driftv1alpha1.WatchedRepo
	if err := r.Get(ctx, req.NamespacedName, &wr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval := defaultPollInterval
	if wr.Spec.PollInterval != "" {
		if d, err := time.ParseDuration(wr.Spec.PollInterval); err == nil {
			interval = d
		} else {
			log.Error(err, "invalid pollInterval, using default", "value", wr.Spec.PollInterval)
		}
	}

	targetConfig, err := r.restConfigFor(ctx, &wr)
	if err != nil {
		return r.recordError(ctx, &wr, interval, fmt.Errorf("resolving target cluster failed: %w", err))
	}

	dir, cleanup, err := cloneRepo(wr.Spec.RepoURL, wr.Spec.Branch)
	if err != nil {
		return r.recordError(ctx, &wr, interval, fmt.Errorf("git clone failed: %w", err))
	}
	defer cleanup()

	manifestsPath := filepath.Join(dir, wr.Spec.Path)
	result, err := drift.Check(ctx, targetConfig, manifestsPath, wr.Spec.Namespace, wr.Spec.Ignore)
	if err != nil {
		return r.recordError(ctx, &wr, interval, err)
	}

	// observed is what was actually found THIS reconcile, before any
	// remediation — used below to detect a real transition. Comparing
	// only the before/after *persisted* status would miss a drift that
	// gets auto-fixed within the same reconcile it's detected in (the
	// "drifted" moment would never be observed as a change from what was
	// last written to status).
	observed := result

	if wr.Spec.AutoRemediate && result.HasDrift() {
		remediation, remErr := drift.Remediate(ctx, targetConfig, manifestsPath, wr.Spec.Namespace, wr.Spec.Ignore,
			drift.RemediateOptions{PruneOrphans: wr.Spec.PruneOrphans})
		if remErr != nil {
			log.Error(remErr, "remediation failed", "name", wr.Name)
		} else {
			log.Info("remediated WatchedRepo", "name", wr.Name,
				"created", remediation.Created, "fixed", remediation.Fixed, "pruned", remediation.Pruned)
			if remediation.HasErrors() {
				log.Info("remediation had per-resource errors", "name", wr.Name, "errors", remediation.Errors)
			}
		}

		// Re-check rather than assume remediation succeeded — a patch can
		// be accepted by the API server without producing the expected
		// result (e.g. an immutable field), so status should reflect
		// reality, not intent.
		result, err = drift.Check(ctx, targetConfig, manifestsPath, wr.Spec.Namespace, wr.Spec.Ignore)
		if err != nil {
			return r.recordError(ctx, &wr, interval, err)
		}
	}

	// Only notify on a change of state, not on every poll — otherwise a
	// long-standing drift would re-alert every pollInterval forever. A
	// LastChecked of zero means this is the resource's first ever check,
	// which also shouldn't fire a notification (there's nothing to compare
	// against yet).
	isFirstCheck := wr.Status.LastChecked.IsZero()
	wasInSync := wr.Status.InSync
	observedInSync := !observed.HasDrift()

	wr.Status.LastChecked = metav1.Now()
	wr.Status.Error = ""
	wr.Status.InSync = !result.HasDrift()
	wr.Status.Drift = nil
	for _, e := range result.Entries {
		if e.Status == drift.StatusInSync {
			continue
		}
		wr.Status.Drift = append(wr.Status.Drift, driftv1alpha1.DriftEntry{
			Kind: e.Kind, Name: e.Name, Status: string(e.Status), Detail: e.Detail,
		})
	}
	wr.Status.DriftCount = len(wr.Status.Drift)

	if err := r.Status().Update(ctx, &wr); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("checked WatchedRepo", "name", wr.Name, "inSync", wr.Status.InSync, "driftCount", wr.Status.DriftCount)

	if !isFirstCheck && observedInSync != wasInSync {
		if err := r.Notifier.Notify(ctx, transitionMessage(&wr, observed)); err != nil {
			log.Error(err, "failed to send drift notification")
		}
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

// transitionMessage describes a sync-state transition. observed is what
// drift.Check found THIS reconcile before any remediation ran, so the
// message reflects what actually happened even when auto-remediation fixed
// it before the persisted status ever recorded it as broken.
func transitionMessage(wr *driftv1alpha1.WatchedRepo, observed drift.Result) string {
	if !observed.HasDrift() {
		return fmt.Sprintf("✅ WatchedRepo %s/%s is back in sync.", wr.Namespace, wr.Name)
	}

	var b strings.Builder
	if wr.Status.InSync {
		fmt.Fprintf(&b, "🔧 WatchedRepo %s/%s drifted and was automatically fixed:\n", wr.Namespace, wr.Name)
	} else {
		fmt.Fprintf(&b, "⚠️ WatchedRepo %s/%s drifted (%d resource(s)):\n", wr.Namespace, wr.Name, wr.Status.DriftCount)
	}
	for _, e := range observed.Entries {
		if e.Status == drift.StatusInSync {
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s/%s\n", e.Status, e.Kind, e.Name)
	}
	return b.String()
}

// restConfigFor returns the cluster to check for wr: the manager's own
// cluster by default, or a different one if wr.Spec.KubeconfigSecretRef
// points at a Secret holding a kubeconfig for it.
func (r *WatchedRepoReconciler) restConfigFor(ctx context.Context, wr *driftv1alpha1.WatchedRepo) (*rest.Config, error) {
	ref := wr.Spec.KubeconfigSecretRef
	if ref == nil {
		return r.RestConfig, nil
	}

	key := ref.Key
	if key == "" {
		key = "kubeconfig"
	}

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: wr.Namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig secret %q: %w", ref.Name, err)
	}
	data, ok := secret.Data[key]
	if !ok {
		return nil, fmt.Errorf("secret %q has no key %q", ref.Name, key)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig from secret %q: %w", ref.Name, err)
	}
	return cfg, nil
}

func (r *WatchedRepoReconciler) recordError(ctx context.Context, wr *driftv1alpha1.WatchedRepo, interval time.Duration, checkErr error) (ctrl.Result, error) {
	wr.Status.LastChecked = metav1.Now()
	wr.Status.Error = checkErr.Error()
	if err := r.Status().Update(ctx, wr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *WatchedRepoReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only re-reconcile on watch events when .spec actually changed (i.e.
	// .metadata.generation bumped). Without this, our own periodic
	// .status.lastChecked write would fire a watch event that immediately
	// triggers another reconcile, tightening the loop far below
	// pollInterval. Status updates don't bump generation because the CRD
	// has the status subresource enabled — the scheduled RequeueAfter
	// still drives normal periodic checks regardless of this filter.
	return ctrl.NewControllerManagedBy(mgr).
		For(&driftv1alpha1.WatchedRepo{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// cloneRepo shallow-clones repoURL (optionally at branch) into a temp
// directory and returns it along with a cleanup function to remove it.
func cloneRepo(repoURL, branch string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "watchedrepo-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repoURL, dir)

	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("%s: %w", string(out), err)
	}
	return dir, cleanup, nil
}
