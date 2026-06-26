// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

// Package rbac provides helpers for managing the cluster-scoped RBAC resources
// that Smeltry requires at startup.
package rbac

import (
	"context"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// EnsureClusterRBAC idempotently creates or updates the two cluster-scoped
// ClusterRoles and their bindings required by Smeltry:
//
//   - smeltry-admin         → bound to Authentik group "smeltry-admins"
//   - smeltry-catalog-reader → bound to system:authenticated
func EnsureClusterRBAC(ctx context.Context, c client.Client) error {
	if err := ensureAdminRole(ctx, c); err != nil {
		return err
	}
	if err := ensureCatalogReaderRole(ctx, c); err != nil {
		return err
	}
	if err := ensureAdminBinding(ctx, c); err != nil {
		return err
	}
	return ensureCatalogReaderBinding(ctx, c)
}

// ensureAdminRole creates or updates the smeltry-admin ClusterRole.
// It grants full access to all portal.smeltry.io resources so that members of
// the Authentik group "smeltry-admins" can manage any ClusterClaim, ServerClaim,
// AddonProfile, or SiteConfig cluster-wide.
func ensureAdminRole(ctx context.Context, c client.Client) error {
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "smeltry-admin"}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{"portal.smeltry.io"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
		}
		return nil
	})
	return err
}

// ensureCatalogReaderRole creates or updates the smeltry-catalog-reader ClusterRole.
// It grants read-only access to AddonProfiles and SiteConfigs in portal-system so
// that any authenticated user can discover what offerings and sites are available
// before submitting a ClusterClaim or ServerClaim.
func ensureCatalogReaderRole(ctx context.Context, c client.Client) error {
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "smeltry-catalog-reader"}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{"portal.smeltry.io"},
				Resources: []string{"addonprofiles", "siteconfigs"},
				Verbs:     []string{"get", "list", "watch"},
			},
		}
		return nil
	})
	return err
}

// ensureAdminBinding binds smeltry-admin to the Authentik group "smeltry-admins".
func ensureAdminBinding(ctx context.Context, c client.Client) error {
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "smeltry-admins"}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, binding, func() error {
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "smeltry-admin",
		}
		binding.Subjects = []rbacv1.Subject{
			{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "smeltry-admins"},
		}
		return nil
	})
	return err
}

// ensureCatalogReaderBinding binds smeltry-catalog-reader to system:authenticated
// so every logged-in user can list available AddonProfiles and SiteConfigs.
func ensureCatalogReaderBinding(ctx context.Context, c client.Client) error {
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "smeltry-catalog-readers"}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, binding, func() error {
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "smeltry-catalog-reader",
		}
		binding.Subjects = []rbacv1.Subject{
			{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "system:authenticated"},
		}
		return nil
	})
	return err
}
