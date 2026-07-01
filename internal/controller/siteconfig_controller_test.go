// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/config"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
	netboxfake "github.com/smeltry-io/smeltry-operator/internal/netbox/fake"
)

func newSiteConfigScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	must(t, portalv1alpha1.AddToScheme(s))
	return s
}

func newSiteConfigReconciler(t *testing.T, nb *netboxfake.Client, site *portalv1alpha1.SiteConfig) *SiteConfigReconciler {
	t.Helper()
	s := newSiteConfigScheme(t)
	return &SiteConfigReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(site).
			WithStatusSubresource(&portalv1alpha1.SiteConfig{}).
			Build(),
		NetboxHolder: config.NewNetboxHolder(nb),
	}
}

func newSiteConfig(name string) *portalv1alpha1.SiteConfig {
	return &portalv1alpha1.SiteConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "portal-system",
		},
		Spec: portalv1alpha1.SiteConfigSpec{
			Netbox: portalv1alpha1.SiteNetboxConfig{
				SiteSlug:           name,
				ProvisioningPrefix: "10.0.1.0/24",
			},
			Network: portalv1alpha1.SiteNetworkConfig{
				ProvisioningCIDR: "10.0.1.0/24",
				ManagementCIDR:   "10.0.0.0/24",
			},
			Cilium: portalv1alpha1.SiteCiliumConfig{L2PoolName: "pool"},
			DNS:    portalv1alpha1.SiteDNSConfig{Zone: "infra.example.com"},
			OIDC: portalv1alpha1.SiteOIDCConfig{
				IssuerURL: "https://auth.example.com/",
				ClientID:  "smeltry-cli",
			},
		},
	}
}

func makeDevice(id int, siteSlug, model string, tags ...string) netbox.Device {
	d := netbox.Device{ID: id}
	d.Status.Value = netbox.DeviceStatusActive
	d.Site.Slug = siteSlug
	d.DeviceType.Model = model
	for _, tag := range tags {
		d.Tags = append(d.Tags, struct {
			Slug string `json:"slug"`
		}{Slug: tag})
	}
	return d
}

// ── Story — MachineClasses populated from Netbox ───────────────────────────

func TestSiteConfigReconciler_SetsMachineClassesStatus(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{
		makeDevice(1, "paris-dc1", "gpu-large", "gpu"),
		makeDevice(2, "paris-dc1", "gpu-large", "gpu"),
		makeDevice(3, "paris-dc1", "standard"),
		makeDevice(4, "lyon-dc2", "standard"), // different site — must be excluded
	}

	site := newSiteConfig("paris-dc1")
	r := newSiteConfigReconciler(t, nb, site)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a RequeueAfter for periodic polling")
	}

	var sc portalv1alpha1.SiteConfig
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"}, &sc); err != nil {
		t.Fatalf("Get SiteConfig: %v", err)
	}

	if len(sc.Status.MachineClasses) != 2 {
		t.Fatalf("expected 2 MachineClassSummary entries, got %d: %v", len(sc.Status.MachineClasses), sc.Status.MachineClasses)
	}

	byClass := make(map[string]portalv1alpha1.MachineClassSummary)
	for _, mc := range sc.Status.MachineClasses {
		byClass[mc.MachineClass] = mc
	}

	gpuLarge := byClass["gpu-large"]
	if gpuLarge.AvailableCount != 2 {
		t.Errorf("gpu-large AvailableCount = %d, want 2", gpuLarge.AvailableCount)
	}
	if !containsString(gpuLarge.Tags, "gpu") {
		t.Errorf("gpu-large Tags = %v, want to contain 'gpu'", gpuLarge.Tags)
	}

	standard := byClass["standard"]
	if standard.AvailableCount != 1 {
		t.Errorf("standard AvailableCount = %d, want 1", standard.AvailableCount)
	}
}

func TestSiteConfigReconciler_ExcludesAssignedDevices(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{
		makeDevice(1, "paris-dc1", "gpu-large", "gpu"),
		func() netbox.Device {
			// device 2 is assigned to a tenant — not available
			d := makeDevice(2, "paris-dc1", "gpu-large", "gpu")
			d.Tenant = &struct {
				Slug string `json:"slug"`
			}{Slug: "acme"}
			return d
		}(),
	}

	site := newSiteConfig("paris-dc1")
	r := newSiteConfigReconciler(t, nb, site)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	var sc portalv1alpha1.SiteConfig
	must(t, r.Client.Get(context.Background(), types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"}, &sc))

	if len(sc.Status.MachineClasses) != 1 {
		t.Fatalf("expected 1 class, got %d", len(sc.Status.MachineClasses))
	}
	if sc.Status.MachineClasses[0].AvailableCount != 1 {
		t.Errorf("AvailableCount = %d, want 1 (assigned device excluded)", sc.Status.MachineClasses[0].AvailableCount)
	}
}

func TestSiteConfigReconciler_EmptyMachineClassesWhenNoDevices(t *testing.T) {
	nb := netboxfake.New()
	// no devices for lyon-dc2

	site := newSiteConfig("lyon-dc2")
	r := newSiteConfigReconciler(t, nb, site)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "lyon-dc2", Namespace: "portal-system"},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	var sc portalv1alpha1.SiteConfig
	must(t, r.Client.Get(context.Background(), types.NamespacedName{Name: "lyon-dc2", Namespace: "portal-system"}, &sc))

	if sc.Status.MachineClasses == nil {
		t.Error("expected non-nil MachineClasses (empty slice), got nil")
	}
	if len(sc.Status.MachineClasses) != 0 {
		t.Errorf("expected 0 entries, got %d", len(sc.Status.MachineClasses))
	}
}

func TestSiteConfigReconciler_SkipsDevicesWithoutModel(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{
		makeDevice(1, "paris-dc1", "gpu-large", "gpu"),
		func() netbox.Device {
			// device with no device type configured in Netbox
			d := makeDevice(2, "paris-dc1", "")
			d.DeviceType.Model = ""
			return d
		}(),
	}

	site := newSiteConfig("paris-dc1")
	r := newSiteConfigReconciler(t, nb, site)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	var sc portalv1alpha1.SiteConfig
	must(t, r.Client.Get(context.Background(), types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"}, &sc))

	if len(sc.Status.MachineClasses) != 1 {
		t.Fatalf("expected 1 class (device without model skipped), got %d: %v", len(sc.Status.MachineClasses), sc.Status.MachineClasses)
	}
	if sc.Status.MachineClasses[0].MachineClass != "gpu-large" {
		t.Errorf("expected class 'gpu-large', got %q", sc.Status.MachineClasses[0].MachineClass)
	}
}

func TestSiteConfigReconciler_RequeuesOnNetboxError(t *testing.T) {
	nb := netboxfake.New()
	nb.ListDevicesBySiteErr = fmt.Errorf("netbox unavailable")

	site := newSiteConfig("paris-dc1")
	r := newSiteConfigReconciler(t, nb, site)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"},
	})
	if err != nil {
		t.Fatalf("expected no error returned (poll-based retry), got: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected RequeueAfter to be set so reconcile is retried")
	}

	// Status must not have been updated — MachineClasses stays nil (never synced).
	var sc portalv1alpha1.SiteConfig
	must(t, r.Client.Get(context.Background(), types.NamespacedName{Name: "paris-dc1", Namespace: "portal-system"}, &sc))
	if sc.Status.MachineClasses != nil {
		t.Errorf("MachineClasses should be nil when sync failed, got %v", sc.Status.MachineClasses)
	}
}
