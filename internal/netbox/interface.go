// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package netbox

import "context"

// ClientIface abstracts the Netbox API operations used by the reconcilers.
// The real implementation is *Client; test code uses a fake.
type ClientIface interface {
	ListTenants(ctx context.Context) ([]Tenant, error)
	ListAvailableDevices(ctx context.Context, siteSlug, model string) ([]Device, error)
	ListDevicesBySite(ctx context.Context, siteSlug string) ([]Device, error)
	SetDeviceStatus(ctx context.Context, deviceID int, status DeviceStatus, tenantSlug string) error
	ListOSDDisks(ctx context.Context, deviceID int) ([]InventoryItem, error)
	AllocateIP(ctx context.Context, prefix, dnsName string, tags []string) (*IPAddress, error)
	ReleaseIP(ctx context.Context, ipID int) error
}
