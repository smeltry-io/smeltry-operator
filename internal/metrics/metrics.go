// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ClusterClaimPhase tracks the number of ClusterClaims currently in each phase.
	ClusterClaimPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "smeltry",
		Subsystem: "clusterclaim",
		Name:      "phase_total",
		Help:      "Number of ClusterClaims currently in each lifecycle phase.",
	}, []string{"phase"})

	// NetboxRequestDuration measures the latency of outbound Netbox API calls.
	NetboxRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "smeltry",
		Subsystem: "netbox",
		Name:      "request_duration_seconds",
		Help:      "Duration of outbound Netbox REST API calls.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "path"})

	// NetboxRequestErrors counts failed Netbox API calls.
	NetboxRequestErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "smeltry",
		Subsystem: "netbox",
		Name:      "request_errors_total",
		Help:      "Total number of failed Netbox REST API calls.",
	}, []string{"method", "path"})
)

// Register adds all custom metrics to the controller-runtime registry so they
// are exposed alongside the standard controller-runtime metrics on /metrics.
func Register() {
	ctrlmetrics.Registry.MustRegister(
		ClusterClaimPhase,
		NetboxRequestDuration,
		NetboxRequestErrors,
	)
}
