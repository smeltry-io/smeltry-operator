// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/config"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

const siteConfigPollInterval = 5 * time.Minute

// SiteConfigReconciler syncs machine availability from Netbox into SiteConfig.status.machineClasses.
type SiteConfigReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	NetboxHolder *config.NetboxHolder
}

func (r *SiteConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sc portalv1alpha1.SiteConfig
	if err := r.Get(ctx, req.NamespacedName, &sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	nb := r.NetboxHolder.Get()
	siteSlug := sc.Spec.Netbox.SiteSlug

	devices, err := nb.ListDevicesBySite(ctx, siteSlug)
	if err != nil {
		logger.Error(err, "failed to list devices from Netbox", "site", siteSlug)
		return ctrl.Result{RequeueAfter: siteConfigPollInterval}, err
	}

	sc.Status.MachineClasses = aggregateMachineClasses(devices)
	now := metav1.Now()
	sc.Status.LastMachineSync = &now

	if err := r.Status().Update(ctx, &sc); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: siteConfigPollInterval}, nil
}

// aggregateMachineClasses groups active devices by model and computes per-class summaries.
// Only devices without a tenant assignment count as available.
func aggregateMachineClasses(devices []netbox.Device) []portalv1alpha1.MachineClassSummary {
	type entry struct {
		available int
		tagSet    map[string]struct{}
	}
	classes := make(map[string]*entry)

	for _, d := range devices {
		e, ok := classes[d.DeviceType.Model]
		if !ok {
			e = &entry{tagSet: make(map[string]struct{})}
			classes[d.DeviceType.Model] = e
		}
		if d.Tenant == nil {
			e.available++
		}
		for _, t := range d.Tags {
			e.tagSet[t.Slug] = struct{}{}
		}
	}

	summaries := make([]portalv1alpha1.MachineClassSummary, 0, len(classes))
	for model, e := range classes {
		tags := make([]string, 0, len(e.tagSet))
		for slug := range e.tagSet {
			tags = append(tags, slug)
		}
		sort.Strings(tags)

		summaries = append(summaries, portalv1alpha1.MachineClassSummary{
			MachineClass:   model,
			AvailableCount: e.available,
			Tags:           tags,
		})
	}

	// Stable ordering by machineClass name.
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].MachineClass < summaries[j].MachineClass
	})

	return summaries
}

func (r *SiteConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&portalv1alpha1.SiteConfig{}).
		Complete(r)
}
