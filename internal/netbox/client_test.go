// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// checkAuth verifies that every request carries the expected Bearer token.
func checkAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Token test-token" {
		t.Errorf("Authorization header = %q, want %q", got, "Token test-token")
	}
}

// mustJSON encodes v to JSON or fails the test.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// ── ListTenants ────────────────────────────────────────────────────────────

func TestListTenants_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/api/tenancy/tenants/") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, mustJSON(t, map[string]any{
			"results": []Tenant{
				{ID: 1, Name: "Acme", Slug: "acme"},
				{ID: 2, Name: "Beta", Slug: "beta"},
			},
		}))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	tenants, err := c.ListTenants(context.Background())
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(tenants) != 2 {
		t.Errorf("len(tenants) = %d, want 2", len(tenants))
	}
	if tenants[0].Slug != "acme" {
		t.Errorf("tenants[0].Slug = %q, want %q", tenants[0].Slug, "acme")
	}
}

func TestListTenants_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer srv.Close()

	tenants, err := NewClient(srv.URL, "test-token").ListTenants(context.Background())
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(tenants) != 0 {
		t.Errorf("expected empty slice, got %d items", len(tenants))
	}
}

func TestListTenants_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// HTTP 500 body is not valid JSON for our struct, so Decode will fail.
	_, err := NewClient(srv.URL, "test-token").ListTenants(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

// ── ListAvailableDevices ───────────────────────────────────────────────────

func TestListAvailableDevices_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		q := r.URL.Query()
		if q.Get("site") != "paris-dc1" {
			t.Errorf("site query param = %q, want %q", q.Get("site"), "paris-dc1")
		}
		if q.Get("device_type__model") != "gpu-large" {
			t.Errorf("device_type__model = %q", q.Get("device_type__model"))
		}
		if q.Get("status") != "active" {
			t.Errorf("status = %q, want active", q.Get("status"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, mustJSON(t, map[string]any{
			"results": []Device{
				{ID: 10, Name: "node-01"},
				{ID: 11, Name: "node-02"},
			},
		}))
	}))
	defer srv.Close()

	devices, err := NewClient(srv.URL, "test-token").
		ListAvailableDevices(context.Background(), "paris-dc1", "gpu-large")
	if err != nil {
		t.Fatalf("ListAvailableDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Errorf("len(devices) = %d, want 2", len(devices))
	}
}

func TestListAvailableDevices_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer srv.Close()

	devices, err := NewClient(srv.URL, "test-token").
		ListAvailableDevices(context.Background(), "site", "model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("expected empty, got %d", len(devices))
	}
}

// ── SetDeviceStatus ────────────────────────────────────────────────────────

func TestSetDeviceStatus_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/dcim/devices/42/") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":42}`)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "test-token").
		SetDeviceStatus(context.Background(), 42, DeviceStatusStaged, "acme")
	if err != nil {
		t.Fatalf("SetDeviceStatus: %v", err)
	}
}

func TestSetDeviceStatus_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "test-token").
		SetDeviceStatus(context.Background(), 99, DeviceStatusStaged, "")
	if err == nil {
		t.Fatal("expected error for HTTP 400, got nil")
	}
}

// ── ListOSDDisks ───────────────────────────────────────────────────────────

func TestListOSDDisks_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if q := r.URL.Query().Get("device_id"); q != "7" {
			t.Errorf("device_id = %q, want 7", q)
		}
		if q := r.URL.Query().Get("tag"); q != "ceph-osd" {
			t.Errorf("tag = %q, want ceph-osd", q)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, mustJSON(t, map[string]any{
			"results": []InventoryItem{
				{ID: 1, Name: "sdb"},
				{ID: 2, Name: "sdc"},
			},
		}))
	}))
	defer srv.Close()

	disks, err := NewClient(srv.URL, "test-token").ListOSDDisks(context.Background(), 7)
	if err != nil {
		t.Fatalf("ListOSDDisks: %v", err)
	}
	if len(disks) != 2 {
		t.Errorf("len(disks) = %d, want 2", len(disks))
	}
	if disks[0].Name != "sdb" {
		t.Errorf("disks[0].Name = %q, want sdb", disks[0].Name)
	}
}

func TestListOSDDisks_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer srv.Close()

	disks, err := NewClient(srv.URL, "test-token").ListOSDDisks(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(disks) != 0 {
		t.Errorf("expected empty, got %d", len(disks))
	}
}

// ── AllocateIP ────────────────────────────────────────────────────────────

func TestAllocateIP_OK(t *testing.T) {
	// AllocateIP makes 2 sequential requests:
	//   1. GET /api/ipam/prefixes/?prefix=10.0.1.0/24  → resolve ID
	//   2. POST /api/ipam/prefixes/5/available-ips/     → allocate
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		callCount++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/ipam/prefixes/"):
			fmt.Fprint(w, `{"results":[{"id":5}]}`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/available-ips/"):
			if !strings.Contains(r.URL.Path, "/5/") {
				t.Errorf("POST to wrong prefix ID: %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, mustJSON(t, IPAddress{ID: 99, Address: "10.0.1.47/24", DNSName: "api.acme.infra.example.com"}))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ip, err := NewClient(srv.URL, "test-token").
		AllocateIP(context.Background(), "10.0.1.0/24", "api.acme.infra.example.com", []string{"smeltry"})
	if err != nil {
		t.Fatalf("AllocateIP: %v", err)
	}
	if ip.ID != 99 {
		t.Errorf("ip.ID = %d, want 99", ip.ID)
	}
	if ip.Address != "10.0.1.47/24" {
		t.Errorf("ip.Address = %q", ip.Address)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", callCount)
	}
}

func TestAllocateIP_PrefixNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[]}`) // empty → prefix not found
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "test-token").
		AllocateIP(context.Background(), "10.99.0.0/24", "test.dns", nil)
	if err == nil {
		t.Fatal("expected error for missing prefix, got nil")
	}
}

func TestAllocateIP_AllocError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"results":[{"id":1}]}`)
			return
		}
		// POST fails
		http.Error(w, "no IPs left", http.StatusConflict)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "test-token").
		AllocateIP(context.Background(), "10.0.1.0/24", "test.dns", nil)
	if err == nil {
		t.Fatal("expected error for HTTP 409, got nil")
	}
}

// ── ReleaseIP ─────────────────────────────────────────────────────────────

func TestReleaseIP_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/ipam/ip-addresses/42/") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "test-token").ReleaseIP(context.Background(), 42); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}
}

func TestReleaseIP_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "test-token").ReleaseIP(context.Background(), 99); err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}
