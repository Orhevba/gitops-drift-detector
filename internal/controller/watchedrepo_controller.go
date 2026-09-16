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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	driftv1alpha1 "github.com/tygacookie/gitops-drift-detector/api/v1alpha1"
	"github.com/tygacookie/gitops-drift-detector/internal/drift"
)

const defaultPollInterval = 5 * time.Minute

// WatchedRepoReconciler reconciles a WatchedRepo object.
type WatchedRepoReconciler struct {
	client.Client
	RestConfig *rest.Config
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

	dir, cleanup, err := cloneRepo(wr.Spec.RepoURL, wr.Spec.Branch)
	if err != nil {
		return r.recordError(ctx, &wr, interval, fmt.Errorf("git clone failed: %w", err))
	}
	defer cleanup()

	manifestsPath := filepath.Join(dir, wr.Spec.Path)
	result, err := drift.Check(ctx, r.RestConfig, manifestsPath, wr.Spec.Namespace, wr.Spec.Ignore)
	if err != nil {
		return r.recordError(ctx, &wr, interval, err)
	}

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
	return ctrl.Result{RequeueAfter: interval}, nil
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&driftv1alpha1.WatchedRepo{}).
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
