// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroSourcePCRangeLookup(t *testing.T) {
	funcs, queries, mismatches, firstMismatch := runtime.CheckStacklessCoroSourcePCRangeForTest(512)
	if funcs == 0 || queries == 0 {
		t.Fatalf("insufficient coverage: funcs=%d queries=%d", funcs, queries)
	}
	if mismatches != 0 {
		t.Fatalf("found %d source PC mismatches across %d funcs / %d queries (first: %s)",
			mismatches, funcs, queries, firstMismatch)
	}
}
