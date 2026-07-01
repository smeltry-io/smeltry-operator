// Package netbox provides a minimal client for the Netbox REST API,
// covering the operations required by the smeltry-operator reconcilers.
package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Client is a Netbox API client.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new Netbox API client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL:    baseURL,
		token:      token,
		httpClient: &http.Client{},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Content-Type", "application/json")
	return c.httpClient.Do(req)
}

// ── Tenants ────────────────────────────────────────────────────────────────

// Tenant represents a Netbox tenancy/tenant object.
type Tenant struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	CustomFields struct {
		K8sMaxClusters int `json:"k8s_max_clusters"`
		K8sMaxNodes    int `json:"k8s_max_nodes"`
	} `json:"custom_fields"`
}

// ListTenants returns all tenants from Netbox.
func (c *Client) ListTenants(ctx context.Context) ([]Tenant, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/tenancy/tenants/?limit=1000", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Results []Tenant `json:"results"`
	}
	return result.Results, json.NewDecoder(resp.Body).Decode(&result)
}

// ── Devices ────────────────────────────────────────────────────────────────

// DeviceStatus represents a Netbox device status value.
type DeviceStatus string

const (
	DeviceStatusActive          DeviceStatus = "active"
	DeviceStatusStaged          DeviceStatus = "staged"
	DeviceStatusOffline         DeviceStatus = "offline"
	DeviceStatusDecommissioning DeviceStatus = "decommissioning"
)

// Device represents a Netbox dcim/device object.
type Device struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status struct {
		Value DeviceStatus `json:"value"`
	} `json:"status"`
	Site struct {
		Slug string `json:"slug"`
	} `json:"site"`
	DeviceType struct {
		Model string `json:"model"`
	} `json:"device_type"`
	Tenant *struct {
		Slug string `json:"slug"`
	} `json:"tenant"`
	Tags []struct {
		Slug string `json:"slug"`
	} `json:"tags"`
}

// ListAvailableDevices returns active, unassigned devices matching site and model.
func (c *Client) ListAvailableDevices(ctx context.Context, siteSlug, model string) ([]Device, error) {
	q := url.Values{}
	q.Set("site", siteSlug)
	q.Set("device_type__model", model)
	q.Set("status", "active")
	q.Set("tenant_id__isnull", "true") // not yet assigned to a tenant
	q.Set("limit", "1000")

	resp, err := c.do(ctx, http.MethodGet, "/api/dcim/devices/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Results []Device `json:"results"`
	}
	return result.Results, json.NewDecoder(resp.Body).Decode(&result)
}

// ListDevicesBySite returns all active devices on the given site regardless of model.
// Both available (no tenant) and assigned (with tenant) devices are included so callers
// can compute totalCount; only unassigned ones should count as "available".
func (c *Client) ListDevicesBySite(ctx context.Context, siteSlug string) ([]Device, error) {
	q := url.Values{}
	q.Set("site", siteSlug)
	q.Set("status", "active")
	q.Set("limit", "1000")

	resp, err := c.do(ctx, http.MethodGet, "/api/dcim/devices/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Results []Device `json:"results"`
	}
	return result.Results, json.NewDecoder(resp.Body).Decode(&result)
}

// SetDeviceStatus patches the status and optional tenant of a device.
func (c *Client) SetDeviceStatus(ctx context.Context, deviceID int, status DeviceStatus, tenantSlug string) error {
	payload := map[string]any{"status": string(status)}
	if tenantSlug != "" {
		payload["tenant"] = map[string]string{"slug": tenantSlug}
	}
	resp, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/dcim/devices/%d/", deviceID), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("netbox PATCH device %d: status %d", deviceID, resp.StatusCode)
	}
	return nil
}

// ── Inventory items (OSD disks) ────────────────────────────────────────────

// InventoryItem represents a Netbox dcim/inventory-item object.
type InventoryItem struct {
	ID   int    `json:"id"`
	Name string `json:"name"` // e.g. "sdb"
}

// ListOSDDisks returns inventory items tagged ceph-osd for a device.
func (c *Client) ListOSDDisks(ctx context.Context, deviceID int) ([]InventoryItem, error) {
	q := url.Values{}
	q.Set("device_id", fmt.Sprint(deviceID))
	q.Set("tag", "ceph-osd")
	q.Set("limit", "100")

	resp, err := c.do(ctx, http.MethodGet, "/api/dcim/inventory-items/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Results []InventoryItem `json:"results"`
	}
	return result.Results, json.NewDecoder(resp.Body).Decode(&result)
}

// ── IPAM ───────────────────────────────────────────────────────────────────

// IPAddress represents a Netbox ipam/ip-address object.
type IPAddress struct {
	ID      int    `json:"id"`
	Address string `json:"address"` // e.g. "10.0.1.47/24"
	DNSName string `json:"dns_name"`
}

// AllocateIP reserves the next available IP in a prefix and registers a DNS name.
func (c *Client) AllocateIP(ctx context.Context, prefix, dnsName string, tags []string) (*IPAddress, error) {
	// Netbox "next available IP" endpoint
	path := fmt.Sprintf("/api/ipam/prefixes/%s/available-ips/", url.PathEscape(prefix))

	tagObjs := make([]map[string]string, len(tags))
	for i, t := range tags {
		tagObjs[i] = map[string]string{"slug": t}
	}
	payload := map[string]any{
		"dns_name": dnsName,
		"tags":     tagObjs,
	}

	// First, resolve the prefix to its numeric ID.
	prefixID, err := c.resolvePrefixID(ctx, prefix)
	if err != nil {
		return nil, err
	}
	path = fmt.Sprintf("/api/ipam/prefixes/%d/available-ips/", prefixID)

	resp, err := c.do(ctx, http.MethodPost, path, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("netbox allocate IP in %s: status %d", prefix, resp.StatusCode)
	}
	var ip IPAddress
	return &ip, json.NewDecoder(resp.Body).Decode(&ip)
}

func (c *Client) resolvePrefixID(ctx context.Context, prefix string) (int, error) {
	q := url.Values{}
	q.Set("prefix", prefix)
	resp, err := c.do(ctx, http.MethodGet, "/api/ipam/prefixes/?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var result struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	if len(result.Results) == 0 {
		return 0, fmt.Errorf("netbox prefix %q not found", prefix)
	}
	return result.Results[0].ID, nil
}

// ReleaseIP deletes an IP address record by its Netbox ID.
func (c *Client) ReleaseIP(ctx context.Context, ipID int) error {
	resp, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/ipam/ip-addresses/%d/", ipID), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("netbox DELETE ip %d: status %d", ipID, resp.StatusCode)
	}
	return nil
}
