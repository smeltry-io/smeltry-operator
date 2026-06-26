// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"runtime"
	"time"

	_ "go.uber.org/automaxprocs"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	internalconfig "github.com/smeltry-io/smeltry-operator/internal/config"
	"github.com/smeltry-io/smeltry-operator/internal/controller"
	"github.com/smeltry-io/smeltry-operator/internal/metrics"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
	internalrbac "github.com/smeltry-io/smeltry-operator/internal/rbac"
	"github.com/smeltry-io/smeltry-operator/internal/telemetry"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

var scheme = k8sruntime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(portalv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr        string
		probeAddr          string
		leaderElect        bool
		logLevel           string
		configMapName      string
		netboxSecretName   string
		netboxPollInterval time.Duration
		machinecfgImage    string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"Address the probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"Enable leader election for high availability.")
	flag.StringVar(&logLevel, "log-level", "info",
		"Log level: debug|info|warn|error.")
	flag.StringVar(&configMapName, "config-map", "smeltry-operator-config",
		"Name of the ConfigMap holding operator configuration (netbox.url, otel.endpoint…).")
	flag.StringVar(&netboxSecretName, "netbox-secret", "smeltry-operator-netbox",
		"Name of the Secret holding netbox.token.")
	flag.DurationVar(&netboxPollInterval, "netbox-poll-interval", 5*time.Minute,
		"How often to poll Netbox for tenant changes.")
	flag.StringVar(&machinecfgImage, "machinecfg-image", "ghcr.io/smeltry-io/machinecfg:latest",
		"Container image for the machinecfg Job.")
	flag.Parse()

	// ── Logging (slog JSON) ────────────────────────────────────────────────
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		level = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	// Bridge controller-runtime to slog via logr.
	ctrl.SetLogger(logr.FromSlogHandler(handler))
	setupLog := ctrl.Log.WithName("setup")

	// ── Prometheus custom metrics ──────────────────────────────────────────
	metrics.Register()

	// ── Namespace (Downward API) ───────────────────────────────────────────
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "smeltry-system"
	}

	// ── Manager ───────────────────────────────────────────────────────────
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

	// ── Read initial config from ConfigMap + Secret ────────────────────────
	ctx := context.Background()
	k8sClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create bootstrap k8s client")
		os.Exit(1)
	}

	cm := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, cm); err != nil {
		setupLog.Error(err, "unable to read ConfigMap", "name", configMapName)
		os.Exit(1)
	}

	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: netboxSecretName, Namespace: namespace}, secret); err != nil {
		setupLog.Error(err, "unable to read Secret", "name", netboxSecretName)
		os.Exit(1)
	}

	netboxURL := cm.Data["netbox.url"]
	netboxToken := string(secret.Data["netbox.token"])
	if netboxURL == "" || netboxToken == "" {
		setupLog.Error(nil, "netbox.url (ConfigMap) and netbox.token (Secret) are required")
		os.Exit(1)
	}

	// ── OpenTelemetry (optional) ───────────────────────────────────────────
	otelEndpoint := cm.Data["otel.endpoint"]
	otelShutdown, err := telemetry.Setup(ctx, otelEndpoint)
	if err != nil {
		setupLog.Error(err, "unable to setup OpenTelemetry")
		os.Exit(1)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	// ── NetboxHolder ───────────────────────────────────────────────────────
	nbHolder := internalconfig.NewNetboxHolder(netbox.NewClient(netboxURL, netboxToken))

	// ── MaxConcurrentReconciles (autotune) ────────────────────────────────
	maxWorkers := max(1, runtime.GOMAXPROCS(0))

	// ── Cluster-scoped RBAC (idempotent, runs at every startup) ──────────
	if err := internalrbac.EnsureClusterRBAC(ctx, k8sClient); err != nil {
		setupLog.Error(err, "unable to ensure cluster RBAC")
		os.Exit(1)
	}

	// ── Controllers ───────────────────────────────────────────────────────
	if err := (&controller.ClusterClaimReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		NetboxHolder: nbHolder,
		NetboxToken:  netboxToken,
		NetboxURL:    netboxURL,
		MachinecfgImage: machinecfgImage,
	}).SetupWithManagerOptions(mgr, maxWorkers); err != nil {
		setupLog.Error(err, "unable to create ClusterClaim controller")
		os.Exit(1)
	}

	if err := (&controller.NetboxTenantReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		NetboxHolder: nbHolder,
		PollInterval: netboxPollInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create NetboxTenant controller")
		os.Exit(1)
	}

	if err := (&internalconfig.ConfigReconciler{
		Client:        mgr.GetClient(),
		Holder:        nbHolder,
		ConfigMapName: configMapName,
		SecretName:    netboxSecretName,
		Namespace:     namespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Config controller")
		os.Exit(1)
	}

	// ── Health checks ──────────────────────────────────────────────────────
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
		"maxWorkers", maxWorkers,
		"otelEnabled", otelEndpoint != "",
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
