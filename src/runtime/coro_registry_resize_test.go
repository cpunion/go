// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroRegistryResize(t *testing.T) {
	for _, tc := range []struct {
		n          int
		minBuckets int
		maxChain   int
	}{
		{n: 4096, minBuckets: 2048, maxChain: 16},
		{n: 30000, minBuckets: 16384, maxChain: 16},
	} {
		grownBuckets, nonEmpty, maxChain, drainedBuckets, largeNil, remaining, unhostedDelta := runtime.CheckStacklessCoroRegistryResizeForTest(tc.n)
		if grownBuckets < tc.minBuckets {
			t.Fatalf("n=%d: grownBuckets = %d, want >= %d", tc.n, grownBuckets, tc.minBuckets)
		}
		if nonEmpty < grownBuckets/2 {
			t.Fatalf("n=%d: nonEmpty = %d / %d, hash distribution unexpectedly skewed", tc.n, nonEmpty, grownBuckets)
		}
		if maxChain > tc.maxChain {
			t.Fatalf("n=%d: maxChain = %d across %d buckets, want <= %d", tc.n, maxChain, grownBuckets, tc.maxChain)
		}
		if drainedBuckets != 256 || !largeNil || remaining != 0 || unhostedDelta != 0 {
			t.Fatalf("n=%d after drain: buckets=%d largeNil=%v remaining=%d unhostedDelta=%d, want 256, true, 0, 0",
				tc.n, drainedBuckets, largeNil, remaining, unhostedDelta)
		}
	}
}
