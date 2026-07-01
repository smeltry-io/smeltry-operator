// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

// Package fake provides an in-memory implementation of netbox.ClientIface for use in tests.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

// Client is an in-memory Netbox client for controller tests.
// It is safe for concurrent use.
type Client struct {
	mu sync.RWMutex

	Tenants  []netbox.Tenant
	Devices  []netbox.Device
	OSDDisks map[int][]netbox.InventoryItem // keyed by device ID

	// nextIPID is incremented on each AllocateIP call to generate unique IDs.
	nextIPID int
	// AllocatedIPs records IPs reserved during a test.
	AllocatedIPs []netbox.IPAddress
	// ReleasedIPs records IP IDs deleted during a test.
	ReleasedIPs []int

	// DeviceStatuses records SetDeviceStatus calls: key = device ID.
	DeviceStatuses map[int]netbox.DeviceStatus
	// DeviceTenants records the tenant slug set by SetDeviceStatus.
	DeviceTenants map[int]string

	// ListTenantsErr, if non-nil, is returned by ListTenants.
	ListTenantsErr error
}

// New returns an empty fake client ready for use.
func New() *Client {
	return &Client{
		OSDDisks:       make(map[int][]netbox.InventoryItem),
		DeviceStatuses: make(map[int]netbox.DeviceStatus),
		DeviceTenants:  make(map[int]string),
		nextIPID:       1,
	}
}

func (c *Client) ListTenants(_ context.Context) ([]netbox.Tenant, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ListTenantsErr != nil {
		return nil, c.ListTenantsErr
	}
	out := make([]netbox.Tenant, len(c.Tenants))
	copy(out, c.Tenants)
	return out, nil
}

func (c *Client) ListDevicesBySite(_ context.Context, siteSlug string) ([]netbox.Device, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []netbox.Device
	for _, d := range c.Devices {
		if d.Status.Value != netbox.DeviceStatusActive {
			continue
		}
		if d.Site.Slug != siteSlug {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (c *Client) ListAvailableDevices(_ context.Context, siteSlug, model string) ([]netbox.Device, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []netbox.Device
	for _, d := range c.Devices {
		if d.Status.Value != netbox.DeviceStatusActive {
			continue
		}
		if d.Tenant != nil {
			continue
		}
		if siteSlug != "" && model != "" && d.DeviceType.Model != model {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (c *Client) SetDeviceStatus(_ context.Context, deviceID int, status netbox.DeviceStatus, tenantSlug string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for i, d := range c.Devices {
		if d.ID == deviceID {
			c.Devices[i].Status.Value = status
			if tenantSlug != "" {
				c.Devices[i].Tenant = &struct {
					Slug string `json:"slug"`
				}{Slug: tenantSlug}
			} else {
				c.Devices[i].Tenant = nil
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("fake: device %d not found", deviceID)
	}
	c.DeviceStatuses[deviceID] = status
	c.DeviceTenants[deviceID] = tenantSlug
	return nil
}

func (c *Client) ListOSDDisks(_ context.Context, deviceID int) ([]netbox.InventoryItem, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	disks := c.OSDDisks[deviceID]
	out := make([]netbox.InventoryItem, len(disks))
	copy(out, disks)
	return out, nil
}

func (c *Client) AllocateIP(_ context.Context, prefix, dnsName string, _ []string) (*netbox.IPAddress, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextIPID
	c.nextIPID++
	ip := netbox.IPAddress{
		ID:      id,
		Address: fmt.Sprintf("10.0.1.%d/24", id),
		DNSName: dnsName,
	}
	_ = prefix
	c.AllocatedIPs = append(c.AllocatedIPs, ip)
	return &ip, nil
}

func (c *Client) ReleaseIP(_ context.Context, ipID int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ReleasedIPs = append(c.ReleasedIPs, ipID)
	return nil
}
