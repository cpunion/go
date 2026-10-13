// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroLateRootSignal(t *testing.T) {
	for _, spawned := range []bool{false, true} {
		for _, concurrent := range []bool{false, true} {
			if !runtime.CheckStacklessCoroLateRootSignalForTest(spawned, concurrent) {
				t.Errorf("root wake changed while a producer remained: spawned=%t concurrent=%t", spawned, concurrent)
			}
		}
	}
}
