// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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

// ── Scheme ────────────────────────────────────────────────────────────────────

func newClusterClaimScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	must(t, corev1.AddToScheme(s))
	must(t, batchv1.AddToScheme(s))
	must(t, rbacv1.AddToScheme(s))
	must(t, portalv1alpha1.AddToScheme(s))
	return s
}

// ── Test helpers ──────────────────────────────────────────────────────────────

func newCCReconciler(t *testing.T, nb *netboxfake.Client, objs ...client.Object) *ClusterClaimReconciler {
	t.Helper()
	s := newClusterClaimScheme(t)
	return &ClusterClaimReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(objs...).
			WithStatusSubresource(&portalv1alpha1.ClusterClaim{}).
			Build(),
		Scheme:          s,
		NetboxHolder:    config.NewNetboxHolder(nb),
		NetboxURL:       "http://netbox.test",
		NetboxToken:     "testtoken",
		MachinecfgImage: "ghcr.io/smeltry-io/machinecfg:test",
	}
}

// newCC builds a ClusterClaim with the protection finalizer pre-seeded so that
// reconcile calls go directly to business logic.
func newCC(name, namespace string) *portalv1alpha1.ClusterClaim {
	return &portalv1alpha1.ClusterClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Finalizers: []string{clusterClaimFinalizer},
		},
		Spec: portalv1alpha1.ClusterClaimSpec{
			MachineClass: "standard",
			MachineCount: 2,
			Site:         "paris-dc1",
			AddonProfile: "base",
		},
	}
}

func reconcileCC(t *testing.T, r *ClusterClaimReconciler, cc *portalv1alpha1.ClusterClaim) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: cc.Name, Namespace: cc.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	return res
}

func getCC(t *testing.T, r *ClusterClaimReconciler, cc *portalv1alpha1.ClusterClaim) *portalv1alpha1.ClusterClaim {
	t.Helper()
	got := &portalv1alpha1.ClusterClaim{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: cc.Name, Namespace: cc.Namespace}, got); err != nil {
		t.Fatalf("Get ClusterClaim: %v", err)
	}
	return got
}

func defaultAddonProfile() *portalv1alpha1.AddonProfile {
	return &portalv1alpha1.AddonProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "base",
			Namespace: portalSystemNamespace,
		},
		Spec: portalv1alpha1.AddonProfileSpec{
			Components: []portalv1alpha1.AddonComponent{
				{Name: "cilium", Required: true, Order: 1, HelmRef: portalv1alpha1.HelmRef{RepoURL: "https://helm.cilium.io", ChartName: "cilium", ChartVersion: "1.16.0"}},
			},
		},
	}
}

func defaultDevices(count int) []netbox.Device {
	var out []netbox.Device
	for i := 0; i < count; i++ {
		d := netbox.Device{ID: 100 + i, Name: fmt.Sprintf("node-%02d", i)}
		d.Status.Value = netbox.DeviceStatusActive
		d.DeviceType.Model = "standard"
		out = append(out, d)
	}
	return out
}

// ── Validation tests (phase="" → stepValidate) ────────────────────────────────

func TestClusterClaim_MissingAddonProfile_Fails(t *testing.T) {
	cc := newCC("ml-train", "tenant-acme")
	r := newCCReconciler(t, netboxfake.New(), cc, defaultSiteConfig())

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestClusterClaim_MissingSiteConfig_Fails(t *testing.T) {
	cc := newCC("ml-train", "tenant-acme")
	r := newCCReconciler(t, netboxfake.New(), cc, defaultAddonProfile())

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestClusterClaim_InsufficientMachines_Fails(t *testing.T) {
	cc := newCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	// only 1 machine but MachineCount=2
	nb.Devices = defaultDevices(1)
	r := newCCReconciler(t, nb, cc, defaultAddonProfile(), defaultSiteConfig())

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestClusterClaim_TagConstraint_Fails(t *testing.T) {
	ap := defaultAddonProfile()
	ap.Spec.MachineConstraints = &portalv1alpha1.MachineConstraints{
		RequiredTags: []string{"gpu"},
	}
	cc := newCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = defaultDevices(2) // no "gpu" tag
	r := newCCReconciler(t, nb, cc, ap, defaultSiteConfig())

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestClusterClaim_ValidationPasses_MovesToProvisioning(t *testing.T) {
	cc := newCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = defaultDevices(3)
	r := newCCReconciler(t, nb, cc, defaultAddonProfile(), defaultSiteConfig())

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning", got.Status.Phase)
	}
}

// ── Provisioning tests (phase=Provisioning) ───────────────────────────────────

func newProvisioningCC(name, namespace string) *portalv1alpha1.ClusterClaim {
	cc := newCC(name, namespace)
	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseProvisioning
	return cc
}

func TestClusterClaim_AllocatesControlPlaneIP(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = defaultDevices(3)
	r := newCCReconciler(t, nb, cc, defaultSiteConfig())

	reconcileCC(t, r, cc)

	if len(nb.AllocatedIPs) == 0 {
		t.Fatal("expected at least one IP to be allocated in Netbox")
	}
	got := getCC(t, r, cc)
	if got.Status.ControlPlaneIP == "" {
		t.Error("status.controlPlaneIP should be set after first reconcile")
	}
}

func TestClusterClaim_AllocatesWebhookIP(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = defaultDevices(3)
	r := newCCReconciler(t, nb, cc, defaultSiteConfig())

	// Two reconcile passes: first sets CP IP, second sets webhook IP.
	// getCC between passes is required to refresh the resourceVersion so the
	// second status update does not conflict with the first.
	reconcileCC(t, r, cc)
	cc = getCC(t, r, cc)
	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.WebhookIP == "" {
		t.Error("status.webhookIP should be set after second reconcile")
	}
	if len(nb.AllocatedIPs) < 2 {
		t.Errorf("expected 2 IPs allocated, got %d", len(nb.AllocatedIPs))
	}
}

func TestClusterClaim_AllocatesMachinesInNetbox(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	// Pre-seed both IPs so provisioning goes straight to machine allocation.
	cc.Status.ControlPlaneIP = "10.0.1.1"
	cc.Status.WebhookIP = "10.0.1.2"
	cc.Status.NetboxIPAMIDs = []int{1, 2}

	nb := netboxfake.New()
	nb.Devices = defaultDevices(3)
	r := newCCReconciler(t, nb, cc, defaultSiteConfig())

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if len(got.Status.AllocatedMachineIDs) != 2 {
		t.Errorf("AllocatedMachineIDs = %v, want 2 entries", got.Status.AllocatedMachineIDs)
	}
	// Verify Netbox device statuses updated.
	for _, id := range got.Status.AllocatedMachineIDs {
		if nb.DeviceStatuses[id] != netbox.DeviceStatusStaged {
			t.Errorf("device %d status = %q, want staged", id, nb.DeviceStatuses[id])
		}
	}
}

func TestClusterClaim_CreatesMachinecfgJob(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	cc.Status.ControlPlaneIP = "10.0.1.1"
	cc.Status.WebhookIP = "10.0.1.2"
	cc.Status.NetboxIPAMIDs = []int{1, 2}
	cc.Status.AllocatedMachineIDs = []int{100, 101}

	nb := netboxfake.New()
	r := newCCReconciler(t, nb, cc, defaultSiteConfig())

	reconcileCC(t, r, cc)

	job := &batchv1.Job{}
	err := r.Get(context.Background(), types.NamespacedName{
		Name:      "ml-train-machinecfg",
		Namespace: "tenant-acme",
	}, job)
	if err != nil {
		t.Fatalf("machinecfg Job not created: %v", err)
	}
}

func TestClusterClaim_WaitsForJobCompletion(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	cc.Status.ControlPlaneIP = "10.0.1.1"
	cc.Status.WebhookIP = "10.0.1.2"
	cc.Status.NetboxIPAMIDs = []int{1, 2}
	cc.Status.AllocatedMachineIDs = []int{100, 101}

	// Pre-create an incomplete job.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-train-machinecfg", Namespace: "tenant-acme"},
	}
	nb := netboxfake.New()
	r := newCCReconciler(t, nb, cc, defaultSiteConfig(), job)

	res := reconcileCC(t, r, cc)

	if res.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 while waiting for job")
	}
	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseProvisioning {
		t.Errorf("phase = %q, want Provisioning while job is running", got.Status.Phase)
	}
}

func TestClusterClaim_JobFailed_MovesToFailed(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	cc.Status.ControlPlaneIP = "10.0.1.1"
	cc.Status.WebhookIP = "10.0.1.2"
	cc.Status.NetboxIPAMIDs = []int{1, 2}
	cc.Status.AllocatedMachineIDs = []int{100, 101}

	now := metav1.Now()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-train-machinecfg", Namespace: "tenant-acme"},
		Status: batchv1.JobStatus{
			CompletionTime: &now,
			Failed:         1,
		},
	}
	nb := netboxfake.New()
	r := newCCReconciler(t, nb, cc, defaultSiteConfig(), job)

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ── Kubeconfig exposure (phase=AddonsReady) ───────────────────────────────────

func newAddonsReadyCC(name, namespace string) *portalv1alpha1.ClusterClaim {
	cc := newCC(name, namespace)
	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseAddonsReady
	return cc
}

func clusterUserRole(namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-user",
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get"},
				ResourceNames: []string{},
			},
		},
	}
}

func TestClusterClaim_StepExposeKubeconfig_MovesToReady(t *testing.T) {
	cc := newAddonsReadyCC("ml-train", "tenant-acme")
	role := clusterUserRole("tenant-acme")
	r := newCCReconciler(t, netboxfake.New(), cc, role)

	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.KubeconfigSecret != "ml-train-kubeconfig" {
		t.Errorf("KubeconfigSecret = %q, want ml-train-kubeconfig", got.Status.KubeconfigSecret)
	}

	updatedRole := &rbacv1.Role{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "cluster-user", Namespace: "tenant-acme"}, updatedRole); err != nil {
		t.Fatalf("Get Role: %v", err)
	}
	found := false
	for _, rule := range updatedRole.Rules {
		if containsString(rule.ResourceNames, "ml-train-kubeconfig") {
			found = true
		}
	}
	if !found {
		t.Error("cluster-user Role should contain ml-train-kubeconfig in resourceNames")
	}
}

func TestClusterClaim_StepExposeKubeconfig_MissingRole_Requeues(t *testing.T) {
	cc := newAddonsReadyCC("ml-train", "tenant-acme")
	// No cluster-user role pre-created.
	r := newCCReconciler(t, netboxfake.New(), cc)

	res := reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseAddonsReady {
		t.Errorf("phase = %q, want AddonsReady while role is missing", got.Status.Phase)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 while waiting for role")
	}
}

// ── Finalizer / deletion ──────────────────────────────────────────────────────

func deletedCC(name, namespace string) *portalv1alpha1.ClusterClaim {
	cc := newCC(name, namespace)
	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseReady
	cc.Status.NetboxIPAMIDs = []int{10, 11}
	cc.Status.AllocatedMachineIDs = []int{100, 101}
	cc.Status.KubeconfigSecret = name + "-kubeconfig"
	now := metav1.Now()
	cc.DeletionTimestamp = &now
	return cc
}

func TestClusterClaim_Finalizer_ReleasesIPs(t *testing.T) {
	cc := deletedCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	// The finalizer calls both ReleaseIP and SetDeviceStatus in sequence.
	// Devices 100/101 must exist so SetDeviceStatus does not error out before
	// we can verify that IPs 10/11 (from NetboxIPAMIDs) were released.
	nb.Devices = defaultDevices(2)
	nb.Devices[0].ID = 100
	nb.Devices[1].ID = 101
	r := newCCReconciler(t, nb, cc)

	reconcileCC(t, r, cc)

	if len(nb.ReleasedIPs) != 2 {
		t.Errorf("ReleasedIPs = %v, want [10, 11]", nb.ReleasedIPs)
	}
	for _, id := range []int{10, 11} {
		found := false
		for _, rid := range nb.ReleasedIPs {
			if rid == id {
				found = true
			}
		}
		if !found {
			t.Errorf("IP ID %d not released", id)
		}
	}
}

func TestClusterClaim_Finalizer_ReleasesMachines(t *testing.T) {
	cc := deletedCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = []netbox.Device{
		func() netbox.Device {
			d := netbox.Device{ID: 100}
			d.Status.Value = netbox.DeviceStatusStaged
			d.DeviceType.Model = "standard"
			return d
		}(),
		func() netbox.Device {
			d := netbox.Device{ID: 101}
			d.Status.Value = netbox.DeviceStatusStaged
			d.DeviceType.Model = "standard"
			return d
		}(),
	}
	r := newCCReconciler(t, nb, cc)

	reconcileCC(t, r, cc)

	for _, id := range []int{100, 101} {
		if nb.DeviceStatuses[id] != netbox.DeviceStatusActive {
			t.Errorf("device %d status = %q, want active after finalizer", id, nb.DeviceStatuses[id])
		}
	}
}

func TestClusterClaim_Finalizer_RemovesKubeconfigFromRole(t *testing.T) {
	cc := deletedCC("ml-train", "tenant-acme")
	role := clusterUserRole("tenant-acme")
	role.Rules[0].ResourceNames = []string{"ml-train-kubeconfig", "other-kubeconfig"}

	nb := netboxfake.New()
	nb.Devices = defaultDevices(2)
	nb.Devices[0].ID = 100
	nb.Devices[1].ID = 101
	r := newCCReconciler(t, nb, cc, role)

	reconcileCC(t, r, cc)

	updatedRole := &rbacv1.Role{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "cluster-user", Namespace: "tenant-acme"}, updatedRole); err != nil {
		t.Fatalf("Get Role: %v", err)
	}
	for _, rule := range updatedRole.Rules {
		if containsString(rule.ResourceNames, "ml-train-kubeconfig") {
			t.Error("ml-train-kubeconfig should have been removed from role after finalizer")
		}
	}
}

func TestClusterClaim_Finalizer_RemovesProtectionFinalizer(t *testing.T) {
	cc := deletedCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = defaultDevices(2)
	nb.Devices[0].ID = 100
	nb.Devices[1].ID = 101
	r := newCCReconciler(t, nb, cc)

	reconcileCC(t, r, cc)

	// The fake client garbage-collects the object once its finalizer list is empty
	// and a DeletionTimestamp is set. Both outcomes are valid: the object is gone
	// (not-found) or still present without the protection finalizer.
	got := &portalv1alpha1.ClusterClaim{}
	err := r.Get(context.Background(), types.NamespacedName{Name: cc.Name, Namespace: cc.Namespace}, got)
	if k8serrors.IsNotFound(err) {
		return // object fully cleaned up — finalizer was removed
	}
	if err != nil {
		t.Fatalf("unexpected Get error: %v", err)
	}
	for _, f := range got.Finalizers {
		if f == clusterClaimFinalizer {
			t.Error("protection finalizer should have been removed after cleanup")
		}
	}
}

// NOT TESTED: stepWatchAddons (phase ClusterReady → AddonsReady).
// This step creates HelmRelease objects (addons.stackhpc.com/v1alpha1, unstructured)
// and polls their status.ready field. Testing it requires either registering the
// capi-addon-provider CRD scheme in the fake client or introducing a seam to inject
// ready status — both add significant complexity. Covered by e2e / manual validation.

// NOT TESTED: OSD disk collection (status.OSDDevices).
// stepProvision calls ListOSDDisks for each allocated machine and stores the result
// in status.OSDDevices, which is then injected as inline Helm values into the
// rook-ceph HelmRelease. The fake Netbox client supports OSDDisks via its OSDDisks
// map — a dedicated test should seed nb.OSDDisks and assert status.OSDDevices after
// reconcileCC. Tracked as a follow-up to the stepWatchAddons testing story.

// ── Ready phase: scale up / scale down / grace period ────────────────────────

// newReadyCC creates a ClusterClaim in phase Ready with pre-allocated machines.
// machineIDs must be coherent with spec.machineCount.
func newReadyCC(name, namespace string, machineIDs []int) *portalv1alpha1.ClusterClaim {
	cc := newCC(name, namespace)
	cc.Spec.MachineCount = len(machineIDs)
	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseReady
	cc.Status.ControlPlaneIP = "10.0.1.1"
	cc.Status.WebhookIP = "10.0.1.2"
	cc.Status.NetboxIPAMIDs = []int{1, 2}
	cc.Status.AllocatedMachineIDs = machineIDs
	cc.Status.KubeconfigSecret = name + "-kubeconfig"
	return cc
}

func TestClusterClaim_ScaleUp_AllocatesNewMachines(t *testing.T) {
	// Cluster starts with 2 machines; user requests 3.
	cc := newReadyCC("ml-train", "tenant-acme", []int{100, 101})
	cc.Spec.MachineCount = 3 // desired: 1 more than allocated

	nb := netboxfake.New()
	// A third machine is available for allocation.
	extra := netbox.Device{ID: 102}
	extra.Status.Value = netbox.DeviceStatusActive
	extra.DeviceType.Model = "standard"
	nb.Devices = []netbox.Device{extra}

	r := newCCReconciler(t, nb, cc, defaultSiteConfig())
	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if len(got.Status.AllocatedMachineIDs) != 3 {
		t.Errorf("AllocatedMachineIDs = %v, want 3 entries after scale up", got.Status.AllocatedMachineIDs)
	}
	if nb.DeviceStatuses[102] != netbox.DeviceStatusStaged {
		t.Errorf("new machine status = %q, want staged", nb.DeviceStatuses[102])
	}
}

func TestClusterClaim_ScaleUp_InsufficientMachines_SetsCondition(t *testing.T) {
	// Cluster has 2 machines; user wants 4, but only 1 extra is available.
	cc := newReadyCC("ml-train", "tenant-acme", []int{100, 101})
	cc.Spec.MachineCount = 4

	nb := netboxfake.New()
	extra := netbox.Device{ID: 102}
	extra.Status.Value = netbox.DeviceStatusActive
	extra.DeviceType.Model = "standard"
	nb.Devices = []netbox.Device{extra} // only 1 available, need 2 more

	r := newCCReconciler(t, nb, cc, defaultSiteConfig())
	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if got.Status.Phase != portalv1alpha1.ClusterClaimPhaseReady {
		t.Errorf("phase = %q, want Ready (still waiting for machines)", got.Status.Phase)
	}
	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Type == "ScaleUpBlocked" {
			found = true
		}
	}
	if !found {
		t.Error("expected condition ScaleUpBlocked to be set")
	}
}

func TestClusterClaim_ScaleDown_WithoutCeph_ReleasesMachines(t *testing.T) {
	// Cluster has 3 machines; user reduces to 2.
	cc := newReadyCC("ml-train", "tenant-acme", []int{100, 101, 102})
	cc.Spec.MachineCount = 2

	nb := netboxfake.New()
	for _, id := range []int{100, 101, 102} {
		d := netbox.Device{ID: id}
		d.Status.Value = netbox.DeviceStatusStaged
		d.DeviceType.Model = "standard"
		nb.Devices = append(nb.Devices, d)
	}

	// AddonProfile without rook-ceph → scale down allowed.
	r := newCCReconciler(t, nb, cc, defaultSiteConfig(), defaultAddonProfile())
	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if len(got.Status.AllocatedMachineIDs) != 2 {
		t.Errorf("AllocatedMachineIDs = %v, want 2 entries after scale down", got.Status.AllocatedMachineIDs)
	}
	if nb.DeviceStatuses[102] != netbox.DeviceStatusActive {
		t.Errorf("released machine status = %q, want active", nb.DeviceStatuses[102])
	}
}

func TestClusterClaim_ScaleDown_WithCeph_SetsBlockedCondition(t *testing.T) {
	cc := newReadyCC("ml-train", "tenant-acme", []int{100, 101, 102})
	cc.Spec.MachineCount = 2

	// AddonProfile includes rook-ceph — scale down must be blocked.
	ap := defaultAddonProfile()
	ap.Spec.Components = append(ap.Spec.Components, portalv1alpha1.AddonComponent{
		Name: "rook-ceph", Required: false, Order: 3,
	})

	nb := netboxfake.New()
	r := newCCReconciler(t, nb, cc, defaultSiteConfig(), ap)
	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Type == "ScaleDownBlocked" {
			found = true
		}
	}
	if !found {
		t.Error("expected condition ScaleDownBlocked when rook-ceph is present")
	}
	// Machines must not have been released.
	if len(got.Status.AllocatedMachineIDs) != 3 {
		t.Errorf("AllocatedMachineIDs = %v, want 3 (unchanged)", got.Status.AllocatedMachineIDs)
	}
}

func TestClusterClaim_GracePeriod_FutureAnnotation_NoDelete(t *testing.T) {
	cc := newReadyCC("ml-train", "tenant-acme", []int{100, 101})
	// Deletion scheduled 1 hour from now — should NOT trigger deletion yet.
	cc.Annotations = map[string]string{
		"portal.smeltry.io/delete-at": metav1.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}

	r := newCCReconciler(t, netboxfake.New(), cc)
	reconcileCC(t, r, cc)

	got := getCC(t, r, cc)
	if !got.DeletionTimestamp.IsZero() {
		t.Error("DeletionTimestamp should NOT be set before grace period expires")
	}
}

func TestClusterClaim_GracePeriod_ExpiredAnnotation_TriggersDeletion(t *testing.T) {
	cc := newReadyCC("ml-train", "tenant-acme", []int{100, 101})
	cc.Status.NetboxIPAMIDs = []int{10, 11}
	// Delete-at is in the past — deletion must be triggered immediately.
	cc.Annotations = map[string]string{
		"portal.smeltry.io/delete-at": metav1.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}

	nb := netboxfake.New()
	for _, id := range []int{100, 101} {
		d := netbox.Device{ID: id}
		d.Status.Value = netbox.DeviceStatusStaged
		d.DeviceType.Model = "standard"
		nb.Devices = append(nb.Devices, d)
	}

	r := newCCReconciler(t, nb, cc)

	// First reconcile: detects expired annotation → triggers Delete.
	reconcileCC(t, r, cc)
	got := getCC(t, r, cc)
	if got.DeletionTimestamp.IsZero() {
		t.Fatal("DeletionTimestamp should be set after grace period expires")
	}

	// Second reconcile: finalizer runs → resources released.
	reconcileCC(t, r, cc)
	if len(nb.ReleasedIPs) == 0 {
		t.Error("expected IPs to be released after finalizer ran")
	}
}

func TestClusterClaim_Idempotent_NoDoubleIPAllocation(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	cc.Status.ControlPlaneIP = "10.0.1.1"
	cc.Status.WebhookIP = "10.0.1.2"
	cc.Status.NetboxIPAMIDs = []int{1, 2}
	cc.Status.AllocatedMachineIDs = []int{100, 101}

	// An incomplete job keeps the reconciler in the "wait" branch — no CAPI calls needed.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ml-train-machinecfg", Namespace: "tenant-acme"},
	}

	nb := netboxfake.New()
	r := newCCReconciler(t, nb, cc, defaultSiteConfig(), job)

	// Two reconcile passes; IPs and machines were already set — no new allocations expected.
	reconcileCC(t, r, cc)
	cc = getCC(t, r, cc)
	reconcileCC(t, r, cc)

	if len(nb.AllocatedIPs) > 0 {
		t.Errorf("expected 0 new IP allocations (already set), got %d", len(nb.AllocatedIPs))
	}
}
