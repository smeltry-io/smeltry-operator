// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/smeltry-io/smeltry-operator/internal/config"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
	netboxfake "github.com/smeltry-io/smeltry-operator/internal/netbox/fake"
)

// newTestReconciler builds a NetboxTenantReconciler backed by a fake kube client
// and a fake Netbox client pre-loaded with the given tenants.
func newTestReconciler(t *testing.T, tenants []netbox.Tenant) (*NetboxTenantReconciler, *netboxfake.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme rbacv1: %v", err)
	}

	fakeNetbox := netboxfake.New()
	fakeNetbox.Tenants = tenants

	r := &NetboxTenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme:       scheme,
		NetboxHolder: config.NewNetboxHolder(fakeNetbox),
		PollInterval: time.Second,
	}
	return r, fakeNetbox
}

// reconcileOnce runs Reconcile with a sentinel request and returns the result.
func reconcileOnce(t *testing.T, r *NetboxTenantReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	return res
}

// ── Story 2.1 — Namespace créé automatiquement ─────────────────────────────

func TestNetboxTenant_CreatesNamespace(t *testing.T) {
	tenant := netbox.Tenant{ID: 1, Name: "Acme Corp", Slug: "acme"}
	r, _ := newTestReconciler(t, []netbox.Tenant{tenant})

	reconcileOnce(t, r)

	ns := &corev1.Namespace{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "tenant-acme"}, ns); err != nil {
		t.Fatalf("namespace tenant-acme not found: %v", err)
	}
	if ns.Labels["portal.smeltry.io/tenant"] != "acme" {
		t.Errorf("namespace missing label portal.smeltry.io/tenant=acme, got %v", ns.Labels)
	}
}

func TestNetboxTenant_NoTenantsNoNamespace(t *testing.T) {
	r, _ := newTestReconciler(t, nil)

	reconcileOnce(t, r)

	nsList := &corev1.NamespaceList{}
	if err := r.List(context.Background(), nsList); err != nil {
		t.Fatalf("List namespaces: %v", err)
	}
	for _, ns := range nsList.Items {
		if len(ns.Name) >= 7 && ns.Name[:7] == "tenant-" {
			t.Errorf("unexpected tenant namespace created: %s", ns.Name)
		}
	}
}

// ── Story 2.2 — ResourceQuota depuis les custom fields Netbox ───────────────

func TestNetboxTenant_AppliesResourceQuota(t *testing.T) {
	tenant := netbox.Tenant{ID: 2, Name: "Beta", Slug: "beta"}
	tenant.CustomFields.K8sMaxClusters = 5
	r, _ := newTestReconciler(t, []netbox.Tenant{tenant})

	reconcileOnce(t, r)

	quota := &corev1.ResourceQuota{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "smeltry-quota", Namespace: "tenant-beta",
	}, quota); err != nil {
		t.Fatalf("ResourceQuota not found: %v", err)
	}

	want := resource.MustParse("5")
	got, ok := quota.Spec.Hard["count/clusterclaims.portal.smeltry.io"]
	if !ok {
		t.Fatal("quota missing count/clusterclaims.portal.smeltry.io")
	}
	if got.Cmp(want) != 0 {
		t.Errorf("cluster quota = %s, want %s", got.String(), want.String())
	}
}

func TestNetboxTenant_QuotaDefaultsToOne(t *testing.T) {
	// k8s_max_clusters == 0 → should default to 1 (via maxInt)
	tenant := netbox.Tenant{ID: 3, Name: "Gamma", Slug: "gamma"}
	// CustomFields.K8sMaxClusters left at zero value
	r, _ := newTestReconciler(t, []netbox.Tenant{tenant})

	reconcileOnce(t, r)

	quota := &corev1.ResourceQuota{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "smeltry-quota", Namespace: "tenant-gamma",
	}, quota); err != nil {
		t.Fatalf("ResourceQuota not found: %v", err)
	}

	want := resource.MustParse("1")
	got := quota.Spec.Hard["count/clusterclaims.portal.smeltry.io"]
	if got.Cmp(want) != 0 {
		t.Errorf("cluster quota = %s, want %s (default)", got.String(), want.String())
	}
}

// ── Story 2.3 — Role + RoleBinding RBAC créés ──────────────────────────────

func TestNetboxTenant_CreatesRoleAndRoleBinding(t *testing.T) {
	tenant := netbox.Tenant{ID: 4, Name: "Delta", Slug: "delta"}
	r, _ := newTestReconciler(t, []netbox.Tenant{tenant})

	reconcileOnce(t, r)

	role := &rbacv1.Role{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "cluster-user", Namespace: "tenant-delta",
	}, role); err != nil {
		t.Fatalf("Role cluster-user not found: %v", err)
	}

	// Role must grant create/delete on clusterclaims and serverclaims.
	foundClaims := false
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res == "clusterclaims" {
				foundClaims = true
			}
		}
	}
	if !foundClaims {
		t.Error("Role does not grant access to clusterclaims")
	}

	rb := &rbacv1.RoleBinding{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "cluster-user-binding", Namespace: "tenant-delta",
	}, rb); err != nil {
		t.Fatalf("RoleBinding cluster-user-binding not found: %v", err)
	}

	if len(rb.Subjects) == 0 || rb.Subjects[0].Name != "delta" {
		t.Errorf("RoleBinding subject = %v, want group 'delta'", rb.Subjects)
	}
	if rb.RoleRef.Name != "cluster-user" {
		t.Errorf("RoleBinding roleRef = %q, want 'cluster-user'", rb.RoleRef.Name)
	}
}

// ── Idempotence — 2 réconciliations, pas de doublon ─────────────────────────

func TestNetboxTenant_Idempotent(t *testing.T) {
	tenant := netbox.Tenant{ID: 5, Name: "Echo", Slug: "echo"}
	r, _ := newTestReconciler(t, []netbox.Tenant{tenant})

	reconcileOnce(t, r)
	reconcileOnce(t, r) // deuxième passe — ne doit pas créer de doublon

	nsList := &corev1.NamespaceList{}
	if err := r.List(context.Background(), nsList); err != nil {
		t.Fatalf("List namespaces: %v", err)
	}
	count := 0
	for _, ns := range nsList.Items {
		if ns.Name == "tenant-echo" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 namespace tenant-echo, got %d", count)
	}
}

// ── Erreur Netbox — requeue sans panique ────────────────────────────────────

func TestNetboxTenant_NetboxError_Requeues(t *testing.T) {
	r, fakeNetbox := newTestReconciler(t, nil)
	fakeNetbox.ListTenantsErr = errNetboxDown

	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err == nil {
		t.Error("expected error from Reconcile when Netbox is down")
	}
	if res.RequeueAfter == 0 {
		t.Error("expected non-zero RequeueAfter on Netbox error")
	}
}

// ── Poll interval renvoyé ────────────────────────────────────────────────────

func TestNetboxTenant_ReturnsConfiguredPollInterval(t *testing.T) {
	r, _ := newTestReconciler(t, nil)
	r.PollInterval = 42 * time.Second

	res := reconcileOnce(t, r)

	if res.RequeueAfter != 42*time.Second {
		t.Errorf("RequeueAfter = %v, want 42s", res.RequeueAfter)
	}
}

func TestNetboxTenant_DefaultPollIntervalIs5min(t *testing.T) {
	r, _ := newTestReconciler(t, nil)
	r.PollInterval = 0 // zero → default

	res := reconcileOnce(t, r)

	if res.RequeueAfter != 5*time.Minute {
		t.Errorf("RequeueAfter = %v, want 5m (default)", res.RequeueAfter)
	}
}

// errNetboxDown is a sentinel error used in tests.
var errNetboxDown = fmt.Errorf("netbox unavailable")
