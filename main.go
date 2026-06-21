package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/controller"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(portalv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		leaderElect          bool
		netboxURL            string
		netboxToken          string
		netboxPollInterval   time.Duration
		machinecfgImage      string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election for high availability.")
	flag.StringVar(&netboxURL, "netbox-url", "",
		"Netbox base URL (e.g. https://netbox.example.com). Env: NETBOX_URL")
	flag.StringVar(&netboxToken, "netbox-token", "",
		"Netbox API token. Env: NETBOX_TOKEN")
	flag.DurationVar(&netboxPollInterval, "netbox-poll-interval", 5*time.Minute,
		"How often to poll Netbox for tenant changes.")
	flag.StringVar(&machinecfgImage, "machinecfg-image", "ghcr.io/smeltry-io/machinecfg:latest",
		"Container image for the machinecfg Job.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Prefer environment variables over flags for secrets.
	if v := os.Getenv("NETBOX_URL"); v != "" {
		netboxURL = v
	}
	if v := os.Getenv("NETBOX_TOKEN"); v != "" {
		netboxToken = v
	}
	if netboxURL == "" || netboxToken == "" {
		setupLog.Error(nil, "NETBOX_URL and NETBOX_TOKEN are required")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "smeltry-operator.portal.smeltry.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	nb := netbox.NewClient(netboxURL, netboxToken)

	if err := (&controller.ClusterClaimReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		NetboxClient: nb,
		NetboxToken:  netboxToken,
		NetboxURL:    netboxURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create ClusterClaim controller")
		os.Exit(1)
	}

	if err := (&controller.NetboxTenantReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		NetboxClient: nb,
		PollInterval: netboxPollInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create NetboxTenant controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting smeltry-operator",
		"netboxURL", netboxURL,
		"pollInterval", netboxPollInterval,
		"machinecfgImage", machinecfgImage,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
