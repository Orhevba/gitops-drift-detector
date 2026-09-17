// gitops-drift-detector (manager) runs the drift check continuously inside
// the cluster, reconciling WatchedRepo custom resources.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	driftv1alpha1 "github.com/Orhevba/gitops-drift-detector/api/v1alpha1"
	"github.com/Orhevba/gitops-drift-detector/internal/controller"
	"github.com/Orhevba/gitops-drift-detector/internal/dashboard"
	"github.com/Orhevba/gitops-drift-detector/internal/notify"
)

func main() {
	var metricsAddr, dashboardAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&dashboardAddr, "dashboard-bind-address", ":8090", "address the web dashboard binds to")
	flag.Parse()

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(driftv1alpha1.AddToScheme(scheme))

	cfg := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	notifier := notify.FromEnv()
	if _, ok := notifier.(notify.NoopNotifier); ok {
		log.Info("TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID not set, drift notifications are disabled")
	}

	if err := (&controller.WatchedRepoReconciler{
		Client:     mgr.GetClient(),
		RestConfig: cfg,
		Notifier:   notifier,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := mgr.Add(ctrlmanager.RunnableFunc(func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              dashboardAddr,
			Handler:           dashboard.NewHandler(mgr.GetClient()),
			ReadHeaderTimeout: 5 * time.Second, // mitigate slow-header (Slowloris-style) requests
		}
		// #nosec G118 -- context.Background() below is deliberate: ctx is
		// already cancelled by the time this goroutine wakes up (that's
		// what <-ctx.Done() waits for), so a context derived from it would
		// be cancelled immediately too, defeating the shutdown grace period.
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		log.Info("starting dashboard", "address", dashboardAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})); err != nil {
		log.Error(err, "unable to add dashboard")
		os.Exit(1)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
