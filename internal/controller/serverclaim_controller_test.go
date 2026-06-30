// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/config"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
	netboxfake "github.com/smeltry-io/smeltry-operator/internal/netbox/fake"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

func newServerClaimScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	must(t, corev1.AddToScheme(s))
	must(t, batchv1.AddToScheme(s))
	must(t, portalv1alpha1.AddToScheme(s))
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("scheme setup: %v", err)
	}
}

func newSCReconciler(t *testing.T, nb *netboxfake.Client, objs ...client.Object) *ServerClaimReconciler {
	t.Helper()
	s := newServerClaimScheme(t)
	return &ServerClaimReconciler{
		Client:          fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&portalv1alpha1.ServerClaim{}).Build(),
		Scheme:          s,
		NetboxHolder:    config.NewNetboxHolder(nb),
		NetboxURL:       "http://netbox.test",
		NetboxToken:     "testtoken",
		MachinecfgImage: "ghcr.io/smeltry-io/machinecfg:test",
	}
}

func defaultSiteConfig() *portalv1alpha1.SiteConfig {
	return &portalv1alpha1.SiteConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "paris-dc1",
			Namespace: portalSystemNamespace,
		},
		Spec: portalv1alpha1.SiteConfigSpec{
			Netbox: portalv1alpha1.SiteNetboxConfig{
				SiteSlug:           "paris-dc1",
				ProvisioningPrefix: "10.0.1.0/24",
				IPAMTags:           []string{"smeltry"},
			},
			DNS: portalv1alpha1.SiteDNSConfig{Zone: "infra.example.com"},
		},
	}
}

func defaultDevice() netbox.Device {
	d := netbox.Device{ID: 10, Name: "srv-01"}
	d.Status.Value = netbox.DeviceStatusActive
	d.DeviceType.Model = "standard"
	return d
}

func reconcileSC(t *testing.T, r *ServerClaimReconciler, sc *portalv1alpha1.ServerClaim) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: sc.Name, Namespace: sc.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	return res
}

func getServerClaim(t *testing.T, r *ServerClaimReconciler, sc *portalv1alpha1.ServerClaim) *portalv1alpha1.ServerClaim {
	t.Helper()
	got := &portalv1alpha1.ServerClaim{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: sc.Name, Namespace: sc.Namespace}, got); err != nil {
		t.Fatalf("Get ServerClaim: %v", err)
	}
	return got
}

// newSC builds a ServerClaim with the protection finalizer already present so
// that reconcile calls go directly to business logic (finalizer setup is trivial
// and not the focus of these tests).
func newSC(name, namespace, site string) *portalv1alpha1.ServerClaim {
	return &portalv1alpha1.ServerClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Finalizers: []string{serverClaimFinalizer},
		},
		Spec: portalv1alpha1.ServerClaimSpec{
			MachineClass: "standard",
			Site:         site,
			OS:           "flatcar",
		},
	}
}

// ── Story 3.1 — Validation : SiteConfig manquant → Failed ────────────────────

func TestServerClaim_MissingSiteConfig_Fails(t *testing.T) {
	nb := netboxfake.New()
	sc := newSC("srv-01", "tenant-acme", "missing-site")
	r := newSCReconciler(t, nb, sc)

	reconcileSC(t, r, sc)

	got := getServerClaim(t, r, sc)
	if got.Status.Phase != portalv1alpha1.ServerClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ── Story 3.1 — Validation : aucune machine disponible → Failed ───────────────

func TestServerClaim_NoAvailableMachine_Fails(t *testing.T) {
	nb := netboxfake.New() // no devices
	sc := newSC("srv-02", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc)

	got := getServerClaim(t, r, sc)
	if got.Status.Phase != portalv1alpha1.ServerClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ── Story 3.1 — Validation réussie → phase Provisioning ──────────────────────

func TestServerClaim_ValidationPass_MovesToProvisioning(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-03", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc)

	got := getServerClaim(t, r, sc)
	if got.Status.Phase != portalv1alpha1.ServerClaimPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
}

// ── Story 3.1 — IP allouée dans Netbox IPAM ───────────────────────────────────

func TestServerClaim_AllocatesIPInNetbox(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-04", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate → Provisioning
	reconcileSC(t, r, sc) // provision: IP + machine + Job

	got := getServerClaim(t, r, sc)
	if got.Status.ServerIP == "" {
		t.Error("expected ServerIP to be set after IP allocation")
	}
	if got.Status.NetboxIPAMID == 0 {
		t.Error("expected NetboxIPAMID to be set")
	}
	if len(nb.AllocatedIPs) == 0 {
		t.Error("expected AllocateIP to have been called on the Netbox client")
	}
}

// ── Story 3.1 — Machine allouée dans Netbox (status=staged) ──────────────────

func TestServerClaim_AllocatesMachineInNetbox(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-05", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate
	reconcileSC(t, r, sc) // provision

	got := getServerClaim(t, r, sc)
	if got.Status.AllocatedMachineID == 0 {
		t.Error("expected AllocatedMachineID to be set")
	}
	if nb.DeviceStatuses[10] != netbox.DeviceStatusStaged {
		t.Errorf("device status = %q, want staged", nb.DeviceStatuses[10])
	}
	if nb.DeviceTenants[10] != "acme" {
		t.Errorf("device tenant = %q, want acme", nb.DeviceTenants[10])
	}
}

// ── Story 3.1 — Job machinecfg créé ──────────────────────────────────────────

func TestServerClaim_CreatesMachinecfgJob(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-06", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate
	reconcileSC(t, r, sc) // provision

	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "srv-06-machinecfg", Namespace: "tenant-acme",
	}, job); err != nil {
		t.Fatalf("machinecfg Job not found: %v", err)
	}
	if job.Spec.Template.Spec.Containers[0].Image != "ghcr.io/smeltry-io/machinecfg:test" {
		t.Errorf("container image = %q", job.Spec.Template.Spec.Containers[0].Image)
	}
}

// ── Story 3.4 — phase Ready quand Job complété ────────────────────────────────

func TestServerClaim_JobComplete_MovesToReady(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-07", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate
	reconcileSC(t, r, sc) // provision: IP + machine + Job created

	// Simulate Job completion
	now := metav1.Now()
	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "srv-07-machinecfg", Namespace: "tenant-acme",
	}, job); err != nil {
		t.Fatalf("Job not found: %v", err)
	}
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("update job status: %v", err)
	}

	reconcileSC(t, r, sc) // detects completion → Ready

	got := getServerClaim(t, r, sc)
	if got.Status.Phase != portalv1alpha1.ServerClaimPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}

// ── Story 3.1 — Job échoué → Failed ──────────────────────────────────────────

func TestServerClaim_JobFailed_MovesToFailed(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-08", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate
	reconcileSC(t, r, sc) // provision

	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "srv-08-machinecfg", Namespace: "tenant-acme",
	}, job); err != nil {
		t.Fatalf("Job not found: %v", err)
	}
	job.Status.Failed = 1
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("update job status: %v", err)
	}

	reconcileSC(t, r, sc) // detects failure → Failed

	got := getServerClaim(t, r, sc)
	if got.Status.Phase != portalv1alpha1.ServerClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ── Finalizer — IP et machine libérées à la suppression ──────────────────────

func TestServerClaim_Finalizer_ReleasesResources(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-09", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate
	reconcileSC(t, r, sc) // provision

	// Trigger deletion
	if err := r.Delete(context.Background(), sc); err != nil {
		t.Fatalf("delete ServerClaim: %v", err)
	}
	reconcileSC(t, r, sc) // finalizer runs

	if len(nb.ReleasedIPs) == 0 {
		t.Error("expected ReleaseIP to have been called")
	}
	if nb.DeviceStatuses[10] != netbox.DeviceStatusActive {
		t.Errorf("device status after release = %q, want active", nb.DeviceStatuses[10])
	}

	gone := &portalv1alpha1.ServerClaim{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "srv-09", Namespace: "tenant-acme"}, gone); !errors.IsNotFound(err) {
		t.Errorf("expected ServerClaim to be deleted, got: %v", err)
	}
}

// ── Story 10.1 — AuditEvent emission from ServerClaimReconciler ──────────────

func newSCReconcilerWithAudit(t *testing.T, nb *netboxfake.Client, objs ...client.Object) *ServerClaimReconciler {
	t.Helper()
	s := newServerClaimScheme(t)
	return &ServerClaimReconciler{
		Client:          fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&portalv1alpha1.ServerClaim{}).Build(),
		Scheme:          s,
		NetboxHolder:    config.NewNetboxHolder(nb),
		NetboxURL:       "http://netbox.test",
		NetboxToken:     "testtoken",
		MachinecfgImage: "ghcr.io/smeltry-io/machinecfg:test",
		DefaultAuditTTL: "720h",
	}
}

// Story 10.1 — A PhaseChanged AuditEvent is emitted when Pending transitions to Provisioning.
func TestServerClaim_EmitsAuditEvent_OnPhaseTransition(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-audit-phase", "tenant-acme", "paris-dc1")
	r := newSCReconcilerWithAudit(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate → Provisioning

	list := &portalv1alpha1.AuditEventList{}
	if err := r.List(context.Background(), list, client.InNamespace("tenant-acme")); err != nil {
		t.Fatalf("List AuditEvents: %v", err)
	}
	found := false
	for _, ev := range list.Items {
		if ev.Spec.Type == portalv1alpha1.AuditTypePhaseChanged &&
			ev.Spec.ResourceName == "srv-audit-phase" &&
			ev.Spec.NewPhase == string(portalv1alpha1.ServerClaimPhaseProvisioning) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected PhaseChanged AuditEvent for srv-audit-phase → Provisioning, got %d events", len(list.Items))
	}
}

// Story 10.1 — A MachineAllocated AuditEvent is emitted when a machine is staged in Netbox.
func TestServerClaim_EmitsAuditEvent_OnMachineAllocation(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-audit-machine", "tenant-acme", "paris-dc1")
	r := newSCReconcilerWithAudit(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate → Provisioning
	reconcileSC(t, r, sc) // provision: IP + machine + Job

	list := &portalv1alpha1.AuditEventList{}
	if err := r.List(context.Background(), list, client.InNamespace("tenant-acme")); err != nil {
		t.Fatalf("List AuditEvents: %v", err)
	}
	found := false
	for _, ev := range list.Items {
		if ev.Spec.Type == portalv1alpha1.AuditTypeMachineAllocated &&
			ev.Spec.ResourceName == "srv-audit-machine" &&
			ev.Spec.MachineID == 10 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected MachineAllocated AuditEvent for srv-audit-machine (machineID=10), got %d events", len(list.Items))
	}
}

// Story 10.1 — A ServerDeleted AuditEvent is emitted when the finalizer runs.
func TestServerClaim_EmitsAuditEvent_OnDeletion(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-audit-delete", "tenant-acme", "paris-dc1")
	r := newSCReconcilerWithAudit(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate
	reconcileSC(t, r, sc) // provision

	if err := r.Delete(context.Background(), sc); err != nil {
		t.Fatalf("delete ServerClaim: %v", err)
	}
	reconcileSC(t, r, sc) // finalizer → ServerDeleted

	list := &portalv1alpha1.AuditEventList{}
	if err := r.List(context.Background(), list, client.InNamespace("tenant-acme")); err != nil {
		t.Fatalf("List AuditEvents: %v", err)
	}
	found := false
	for _, ev := range list.Items {
		if ev.Spec.Type == portalv1alpha1.AuditTypeServerDeleted && ev.Spec.ResourceName == "srv-audit-delete" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ServerDeleted AuditEvent for srv-audit-delete, got %d events", len(list.Items))
	}
}

// ── Idempotence — double réconciliation sans effet de bord ───────────────────

func TestServerClaim_Idempotent_NoDoubleAlloc(t *testing.T) {
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{defaultDevice()}
	sc := newSC("srv-10", "tenant-acme", "paris-dc1")
	r := newSCReconciler(t, nb, sc, defaultSiteConfig())

	reconcileSC(t, r, sc) // validate
	reconcileSC(t, r, sc) // provision (IP + machine + Job)
	reconcileSC(t, r, sc) // re-enter Provisioning — must not allocate a second IP

	if len(nb.AllocatedIPs) != 1 {
		t.Errorf("AllocateIP called %d times, want exactly 1", len(nb.AllocatedIPs))
	}
}
