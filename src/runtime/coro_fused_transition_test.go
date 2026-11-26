// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroFusedSchedulerTransitions(t *testing.T) {
	if msg := runtime.CheckStacklessCoroFusedSchedulerTransitionsForTest(); msg != "" {
		t.Fatal(msg)
	}
}
