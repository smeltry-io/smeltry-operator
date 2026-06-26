// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package config

import (
	"sync"

	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

// NetboxHolder is a thread-safe container for the Netbox client.
// Reconcilers call Get() on every reconcile pass so that hot-reloads
// take effect without restarting the controller.
type NetboxHolder struct {
	mu     sync.RWMutex
	client netbox.ClientIface
}

// NewNetboxHolder returns a holder pre-initialised with the given client.
func NewNetboxHolder(c netbox.ClientIface) *NetboxHolder {
	return &NetboxHolder{client: c}
}

// Get returns the current Netbox client. Safe for concurrent use.
func (h *NetboxHolder) Get() netbox.ClientIface {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

// Set atomically replaces the Netbox client. Safe for concurrent use.
func (h *NetboxHolder) Set(c netbox.ClientIface) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client = c
}
