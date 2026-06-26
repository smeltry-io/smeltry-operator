// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/config"
	netboxfake "github.com/smeltry-io/smeltry-operator/internal/netbox/fake"
)

// ── Scheme ────────────────────────────────────────────────────────────────────

func newAuditScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newClusterClaimScheme(t)
	return s
}

// ── AuditEventPurgeReconciler helpers ────────────────────────────────────────

func newPurgeReconciler(t *testing.T, defaultTTL time.Duration, objs ...client.Object) *AuditEventPurgeReconciler {
	t.Helper()
	s := newAuditScheme(t)
	return &AuditEventPurgeReconciler{
		Client:     fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		DefaultTTL: defaultTTL,
	}
}

func makeAuditEvent(name, namespace string, age time.Duration, ttl string) *portalv1alpha1.AuditEvent {
	return &portalv1alpha1.AuditEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
		},
		Spec: portalv1alpha1.AuditEventSpec{
			Type:         portalv1alpha1.AuditTypePhaseChanged,
			ResourceKind: "ClusterClaim",
			ResourceName: "ml-train",
			Timestamp:    metav1.Now(),
			TTL:          ttl,
		},
	}
}

// ── Story 10.4 — TTL purge ────────────────────────────────────────────────────

// Story 10.4 — An AuditEvent past its TTL is deleted by the purge controller.
func TestAuditEventPurge_DeletesExpiredEvent(t *testing.T) {
	ev := makeAuditEvent("ev-old", "tenant-acme", 31*24*time.Hour, "720h") // 31 days old, TTL 30 days
	r := newPurgeReconciler(t, 720*time.Hour, ev)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ev.Name, Namespace: ev.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &portalv1alpha1.AuditEvent{}
	err = r.Get(context.Background(), types.NamespacedName{Name: ev.Name, Namespace: ev.Namespace}, got)
	if !k8serrors.IsNotFound(err) {
		t.Errorf("expected expired AuditEvent to be deleted (IsNotFound), got err=%v", err)
	}
}

// Story 10.4 — A recent AuditEvent within its TTL is kept.
func TestAuditEventPurge_KeepsRecentEvent(t *testing.T) {
	ev := makeAuditEvent("ev-new", "tenant-acme", 5*24*time.Hour, "720h") // 5 days old, TTL 30 days
	r := newPurgeReconciler(t, 720*time.Hour, ev)

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ev.Name, Namespace: ev.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected RequeueAfter > 0 for a non-expired event, got %v", res.RequeueAfter)
	}

	got := &portalv1alpha1.AuditEvent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: ev.Name, Namespace: ev.Namespace}, got); err != nil {
		t.Errorf("expected recent AuditEvent to be kept, got error: %v", err)
	}
}

// Story 10.4 — When spec.ttl is empty, the operator default TTL applies.
func TestAuditEventPurge_UsesDefaultTTL(t *testing.T) {
	// No TTL set on the event; default is 1 hour; event is 2 hours old → expired.
	ev := makeAuditEvent("ev-default", "tenant-acme", 2*time.Hour, "")
	r := newPurgeReconciler(t, time.Hour, ev)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ev.Name, Namespace: ev.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &portalv1alpha1.AuditEvent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: ev.Name, Namespace: ev.Namespace}, got); err == nil {
		t.Error("expected AuditEvent with default TTL to be deleted after expiry")
	}
}

// ── Story 10.1 — AuditEvent emission from ClusterClaimReconciler ─────────────

func newCCReconcilerWithAudit(t *testing.T, nb *netboxfake.Client, objs ...client.Object) *ClusterClaimReconciler {
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
		DefaultAuditTTL: "720h",
	}
}

// Story 10.1 — A PhaseChanged AuditEvent is emitted when Pending transitions to Provisioning.
func TestClusterClaim_EmitsAuditEvent_OnPhaseTransition(t *testing.T) {
	cc := newCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = defaultDevices(3)
	r := newCCReconcilerWithAudit(t, nb, cc, defaultAddonProfile(), defaultSiteConfig())

	reconcileCC(t, r, cc)

	list := &portalv1alpha1.AuditEventList{}
	if err := r.List(context.Background(), list, client.InNamespace("tenant-acme")); err != nil {
		t.Fatalf("List AuditEvents: %v", err)
	}
	found := false
	for _, ev := range list.Items {
		if ev.Spec.Type == portalv1alpha1.AuditTypePhaseChanged &&
			ev.Spec.ResourceName == "ml-train" &&
			ev.Spec.NewPhase == string(portalv1alpha1.ClusterClaimPhaseProvisioning) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected PhaseChanged AuditEvent for ml-train → Provisioning, got %d events", len(list.Items))
	}
}

// Story 10.1 — A MachineAllocated AuditEvent is emitted for each machine allocated.
func TestClusterClaim_EmitsAuditEvent_OnMachineAllocation(t *testing.T) {
	cc := newProvisioningCC("ml-train", "tenant-acme")
	cc.Status.ControlPlaneIP = "10.0.1.1"
	cc.Status.WebhookIP = "10.0.1.2"
	cc.Status.NetboxIPAMIDs = []int{1, 2}

	nb := netboxfake.New()
	nb.Devices = defaultDevices(3)
	r := newCCReconcilerWithAudit(t, nb, cc, defaultSiteConfig())

	reconcileCC(t, r, cc)

	list := &portalv1alpha1.AuditEventList{}
	if err := r.List(context.Background(), list, client.InNamespace("tenant-acme")); err != nil {
		t.Fatalf("List AuditEvents: %v", err)
	}
	machineEvents := 0
	for _, ev := range list.Items {
		if ev.Spec.Type == portalv1alpha1.AuditTypeMachineAllocated {
			machineEvents++
		}
	}
	if machineEvents != 2 { // MachineCount=2 from newCC
		t.Errorf("expected 2 MachineAllocated AuditEvents, got %d", machineEvents)
	}
}

// Story 10.1 — A ClusterDeleted AuditEvent is emitted when the finalizer runs.
func TestClusterClaim_EmitsAuditEvent_OnDeletion(t *testing.T) {
	cc := deletedCC("ml-train", "tenant-acme")
	nb := netboxfake.New()
	nb.Devices = defaultDevices(2)
	nb.Devices[0].ID = 100
	nb.Devices[1].ID = 101
	r := newCCReconcilerWithAudit(t, nb, cc)

	reconcileCC(t, r, cc)

	list := &portalv1alpha1.AuditEventList{}
	if err := r.List(context.Background(), list, client.InNamespace("tenant-acme")); err != nil {
		t.Fatalf("List AuditEvents: %v", err)
	}
	found := false
	for _, ev := range list.Items {
		if ev.Spec.Type == portalv1alpha1.AuditTypeClusterDeleted && ev.Spec.ResourceName == "ml-train" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ClusterDeleted AuditEvent for ml-train, got %d events", len(list.Items))
	}
}
