// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package rbac_test

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/smeltry-io/smeltry-operator/internal/rbac"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme setup: %v", err)
	}
	return s
}

// Story 1.3 — smeltry-admin ClusterRole grants full access to portal.smeltry.io resources.
func TestEnsureClusterRBAC_CreatesAdminClusterRole(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	if err := rbac.EnsureClusterRBAC(context.Background(), c); err != nil {
		t.Fatalf("EnsureClusterRBAC: %v", err)
	}

	role := &rbacv1.ClusterRole{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "smeltry-admin"}, role); err != nil {
		t.Fatalf("ClusterRole smeltry-admin not found: %v", err)
	}
	found := false
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			if group == "portal.smeltry.io" {
				found = true
			}
		}
	}
	if !found {
		t.Error("smeltry-admin ClusterRole should have a rule for apiGroup portal.smeltry.io")
	}
}

// Story 1.3 — smeltry-catalog-reader ClusterRole grants read access to AddonProfiles and SiteConfigs.
func TestEnsureClusterRBAC_CreatesCatalogReaderClusterRole(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	if err := rbac.EnsureClusterRBAC(context.Background(), c); err != nil {
		t.Fatalf("EnsureClusterRBAC: %v", err)
	}

	role := &rbacv1.ClusterRole{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "smeltry-catalog-reader"}, role); err != nil {
		t.Fatalf("ClusterRole smeltry-catalog-reader not found: %v", err)
	}
	wantResources := map[string]bool{"addonprofiles": false, "siteconfigs": false}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if _, ok := wantResources[res]; ok {
				wantResources[res] = true
			}
		}
	}
	for res, found := range wantResources {
		if !found {
			t.Errorf("smeltry-catalog-reader ClusterRole missing rule for resource %q", res)
		}
	}
}

// Story 1.3 — smeltry-admin is bound to the Authentik group smeltry-admins.
func TestEnsureClusterRBAC_CreatesAdminClusterRoleBinding(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	if err := rbac.EnsureClusterRBAC(context.Background(), c); err != nil {
		t.Fatalf("EnsureClusterRBAC: %v", err)
	}

	binding := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "smeltry-admins"}, binding); err != nil {
		t.Fatalf("ClusterRoleBinding smeltry-admins not found: %v", err)
	}
	if binding.RoleRef.Name != "smeltry-admin" {
		t.Errorf("RoleRef.Name = %q, want smeltry-admin", binding.RoleRef.Name)
	}
	found := false
	for _, s := range binding.Subjects {
		if s.Kind == "Group" && s.Name == "smeltry-admins" {
			found = true
		}
	}
	if !found {
		t.Error("expected subject Group smeltry-admins in ClusterRoleBinding smeltry-admins")
	}
}

// Story 1.3 — smeltry-catalog-reader is bound to system:authenticated so any
// logged-in user can discover available AddonProfiles and SiteConfigs.
func TestEnsureClusterRBAC_CreatesCatalogReaderClusterRoleBinding(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	if err := rbac.EnsureClusterRBAC(context.Background(), c); err != nil {
		t.Fatalf("EnsureClusterRBAC: %v", err)
	}

	binding := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "smeltry-catalog-readers"}, binding); err != nil {
		t.Fatalf("ClusterRoleBinding smeltry-catalog-readers not found: %v", err)
	}
	if binding.RoleRef.Name != "smeltry-catalog-reader" {
		t.Errorf("RoleRef.Name = %q, want smeltry-catalog-reader", binding.RoleRef.Name)
	}
	found := false
	for _, s := range binding.Subjects {
		if s.Kind == "Group" && s.Name == "system:authenticated" {
			found = true
		}
	}
	if !found {
		t.Error("expected subject Group system:authenticated in ClusterRoleBinding smeltry-catalog-readers")
	}
}

// EnsureClusterRBAC must be idempotent: a second call must not return an error.
func TestEnsureClusterRBAC_Idempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	if err := rbac.EnsureClusterRBAC(context.Background(), c); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := rbac.EnsureClusterRBAC(context.Background(), c); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}
