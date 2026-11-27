// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroTaskLifecycleAtomics(t *testing.T) {
	if msg := runtime.CheckStacklessCoroTaskLifecycleAtomicsForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroPStateCachesAndGC(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	if msg := runtime.CheckStacklessCoroPStateCachesAndGCForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroPRunqShrinkDrain(t *testing.T) {
	if msg := runtime.CheckStacklessCoroPRunqShrinkDrainForTest(); msg != "" {
		t.Fatal(msg)
	}
}
