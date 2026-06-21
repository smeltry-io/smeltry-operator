// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package config

import (
	"sync"
	"testing"

	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

func TestNetboxHolder_GetSet(t *testing.T) {
	c1 := netbox.NewClient("http://netbox1.example.com", "token1")
	h := NewNetboxHolder(c1)

	if got := h.Get(); got != c1 {
		t.Errorf("Get() after NewNetboxHolder returned unexpected client")
	}

	c2 := netbox.NewClient("http://netbox2.example.com", "token2")
	h.Set(c2)

	if got := h.Get(); got != c2 {
		t.Errorf("Get() after Set() returned wrong client")
	}
}

func TestNetboxHolder_Concurrent(t *testing.T) {
	h := NewNetboxHolder(netbox.NewClient("http://initial.example.com", "tok"))

	const readers = 20
	const writers = 4

	var wg sync.WaitGroup
	wg.Add(readers + writers)

	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c := h.Get()
				if c == nil {
					t.Errorf("Get() returned nil")
				}
			}
		}()
	}

	for i := 0; i < writers; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h.Set(netbox.NewClient("http://updated.example.com", "tok"))
			}
		}(i)
	}

	wg.Wait()
}
