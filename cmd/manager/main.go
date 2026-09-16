// gitops-drift-detector (manager) runs the drift check continuously inside
// the cluster, reconciling WatchedRepo custom resources.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	driftv1alpha1 "github.com/tygacookie/gitops-drift-detector/api/v1alpha1"
	"github.com/tygacookie/gitops-drift-detector/internal/controller"
	"github.com/tygacookie/gitops-drift-detector/internal/notify"
)

func main() {
	var metricsAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
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

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
