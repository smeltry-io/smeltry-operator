// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

// ── tenantFromNamespace ────────────────────────────────────────────────────

func TestTenantFromNamespace(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"tenant-acme", "acme"},
		{"tenant-my-org", "my-org"},
		{"acme", "acme"}, // no prefix — returned unchanged
		{"", ""},
	}
	for _, c := range cases {
		if got := tenantFromNamespace(c.in); got != c.want {
			t.Errorf("tenantFromNamespace(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── stripPrefix ───────────────────────────────────────────────────────────

func TestStripPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"10.0.1.5/24", "10.0.1.5"},
		{"192.168.1.1/32", "192.168.1.1"},
		{"10.0.1.5", "10.0.1.5"}, // no suffix
		{"", ""},
	}
	for _, c := range cases {
		if got := stripPrefix(c.in); got != c.want {
			t.Errorf("stripPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── machinesHaveTag ───────────────────────────────────────────────────────

func TestMachinesHaveTag(t *testing.T) {
	makeDevice := func(tags ...string) netbox.Device {
		d := netbox.Device{}
		for _, s := range tags {
			d.Tags = append(d.Tags, struct {
				Slug string `json:"slug"`
			}{Slug: s})
		}
		return d
	}

	t.Run("found", func(t *testing.T) {
		devices := []netbox.Device{makeDevice("gpu", "infra"), makeDevice("storage")}
		if !machinesHaveTag(devices, "gpu") {
			t.Error("expected tag 'gpu' to be found")
		}
	})

	t.Run("not found", func(t *testing.T) {
		devices := []netbox.Device{makeDevice("storage")}
		if machinesHaveTag(devices, "gpu") {
			t.Error("expected tag 'gpu' NOT to be found")
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		if machinesHaveTag(nil, "gpu") {
			t.Error("expected false for empty slice")
		}
	})
}

// ── containsString ────────────────────────────────────────────────────────

func TestContainsString(t *testing.T) {
	s := []string{"alpha", "beta", "gamma"}

	if !containsString(s, "beta") {
		t.Error("expected 'beta' to be found")
	}
	if containsString(s, "delta") {
		t.Error("expected 'delta' NOT to be found")
	}
	if containsString(nil, "alpha") {
		t.Error("expected false for nil slice")
	}
}

// ── removeString ──────────────────────────────────────────────────────────

func TestRemoveString(t *testing.T) {
	t.Run("removes element", func(t *testing.T) {
		s := []string{"a", "b", "c"}
		got := removeString(s, "b")
		if containsString(got, "b") {
			t.Errorf("'b' should have been removed; got %v", got)
		}
		if len(got) != 2 {
			t.Errorf("len = %d, want 2", len(got))
		}
	})

	t.Run("idempotent for absent element", func(t *testing.T) {
		s := []string{"a", "c"}
		got := removeString(s, "z")
		if len(got) != 2 {
			t.Errorf("len = %d, want 2", len(got))
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		got := removeString(nil, "x")
		if len(got) != 0 {
			t.Errorf("expected empty result, got %v", got)
		}
	})
}

// ── rulesCoversSecrets ────────────────────────────────────────────────────

func TestRulesCoversSecrets(t *testing.T) {
	yes := rbacv1.PolicyRule{Resources: []string{"pods", "secrets"}}
	no := rbacv1.PolicyRule{Resources: []string{"pods", "configmaps"}}
	empty := rbacv1.PolicyRule{}

	if !rulesCoversSecrets(yes) {
		t.Error("expected secrets rule to be covered")
	}
	if rulesCoversSecrets(no) {
		t.Error("expected non-secrets rule to return false")
	}
	if rulesCoversSecrets(empty) {
		t.Error("expected empty rule to return false")
	}
}

// ── setCondition ──────────────────────────────────────────────────────────

func TestSetCondition_Append(t *testing.T) {
	cc := &portalv1alpha1.ClusterClaim{}
	setCondition(cc, "Validated", metav1.ConditionTrue, "OK", "all good")

	if len(cc.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(cc.Status.Conditions))
	}
	c := cc.Status.Conditions[0]
	if c.Type != "Validated" {
		t.Errorf("Type = %q", c.Type)
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Status = %q, want True", c.Status)
	}
	if c.Reason != "OK" {
		t.Errorf("Reason = %q", c.Reason)
	}
}

func TestSetCondition_Update(t *testing.T) {
	cc := &portalv1alpha1.ClusterClaim{}
	setCondition(cc, "Validated", metav1.ConditionTrue, "OK", "first")
	setCondition(cc, "Validated", metav1.ConditionFalse, "NotOK", "second")

	if len(cc.Status.Conditions) != 1 {
		t.Errorf("expected 1 condition after update, got %d", len(cc.Status.Conditions))
	}
	c := cc.Status.Conditions[0]
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Status = %q, want False after update", c.Status)
	}
	if c.Message != "second" {
		t.Errorf("Message = %q, want 'second'", c.Message)
	}
}

func TestSetCondition_MultipleTypes(t *testing.T) {
	cc := &portalv1alpha1.ClusterClaim{}
	setCondition(cc, "Validated", metav1.ConditionTrue, "OK", "")
	setCondition(cc, "IPAllocated", metav1.ConditionFalse, "Pending", "")

	if len(cc.Status.Conditions) != 2 {
		t.Errorf("expected 2 conditions, got %d", len(cc.Status.Conditions))
	}
}
