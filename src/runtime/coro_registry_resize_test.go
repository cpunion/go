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
	const n = 4096
	grownBuckets, nonEmpty, maxChain, drainedBuckets, largeNil, remaining := runtime.CheckStacklessCoroRegistryResizeForTest(n)
	if grownBuckets < 2048 {
		t.Fatalf("grownBuckets = %d, want >= 2048 for %d live records", grownBuckets, n)
	}
	if nonEmpty < grownBuckets/2 {
		t.Fatalf("nonEmpty = %d / %d, hash distribution unexpectedly skewed", nonEmpty, grownBuckets)
	}
	if maxChain > 16 {
		t.Fatalf("maxChain = %d for %d records across %d buckets, want <= 16", maxChain, n, grownBuckets)
	}
	if drainedBuckets != 256 || !largeNil || remaining != 0 {
		t.Fatalf("after drain: buckets=%d largeNil=%v remaining=%d, want 256, true, 0",
			drainedBuckets, largeNil, remaining)
	}
}
