// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "testing"

func TestStableTerminalAction(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*lowerCandidate)
		want   bool
	}{
		{"await", func(*lowerCandidate) {}, true},
		{"yield", func(c *lowerCandidate) { c.function.Sites = append(c.function.Sites, Site{Kind: SiteYield}) }, true},
		{"child-terminal", func(c *lowerCandidate) { c.function.Terminal = MayPanic | MayGoexit }, false},
		{"no-factory", func(c *lowerCandidate) { c.factoryABI = NoFactory }, false},
		{"closure-factory", func(c *lowerCandidate) { c.factoryABI = FactoryABI1 }, false},
		{"explicit-factory", func(c *lowerCandidate) { c.factoryABI = FactoryABI2 }, false},
		{"self-only", func(c *lowerCandidate) { c.fusedResume = false; c.fusedSelf = true }, false},
		{"defer", func(c *lowerCandidate) { c.defers = []*lowerDefer{{}} }, false},
		{"empty", func(c *lowerCandidate) { c.function.Sites = nil }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := &lowerCandidate{
				factoryABI:  FactoryABI3,
				fusedResume: true,
				function:    &Function{Sites: []Site{{Kind: SiteAwait}}},
			}
			test.change(candidate)
			if got := candidate.stableTerminalAction(); got != test.want {
				t.Errorf("stableTerminalAction = %v, want %v", got, test.want)
			}
		})
	}
	for kind := SiteInvalid; kind <= SiteSemaphore; kind++ {
		candidate := &lowerCandidate{
			factoryABI:  FactoryABI3,
			fusedResume: true,
			function:    &Function{Sites: []Site{{Kind: SiteAwait}, {Kind: kind}}},
		}
		want := kind == SiteAwait || kind == SiteYield
		if got := candidate.stableTerminalAction(); got != want {
			t.Errorf("site %s: stableTerminalAction = %v, want %v", kind, got, want)
		}
	}
}
