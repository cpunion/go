// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroNativeProfileTransitions(t *testing.T) {
	for _, test := range []struct {
		name    string
		enter   bool
		records int64
	}{
		{"enter", true, 0}, {"return", false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := runtime.StacklessCoroNativeProfileTransitionForTest(test.enter)
			if !got.Native || !got.Reused {
				t.Fatalf("native executor reuse was not exercised: %+v", got)
			}
			// goroutineProfileSatisfied is the terminal state of the concurrent
			// profiling protocol, including for an excluded system executor.
			if got.State != 2 || got.Records != test.records || (got.PCs != 0) != (test.records != 0) {
				t.Fatalf("native handoff did not finish pending profile: %+v; want state2 records%d", got, test.records)
			}
		})
	}
}
