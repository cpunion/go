// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroPublicGState(t *testing.T) {
	// Run on a dedicated goroutine so gp.stacklessCoro starts nil.
	done := make(chan string, 1)
	go func() {
		done <- runtime.CheckStacklessCoroPublicGStateForTest()
	}()
	if msg := <-done; msg != "" {
		t.Fatal(msg)
	}
}
