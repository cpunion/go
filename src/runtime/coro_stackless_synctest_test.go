// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"testing"
	"testing/synctest"
	"time"
)

func TestStacklessCoroSynctestContext(t *testing.T) {
	if runtime.StacklessCoroInBubbleForTest() {
		t.Fatal("ordinary goroutine is in a bubble")
	}
	for range 2 {
		synctest.Test(t, func(t *testing.T) {
			if !runtime.StacklessCoroInBubbleForTest() {
				t.Fatal("bubble goroutine selected a native entry")
			}
			checked := false
			go func() {
				if !runtime.StacklessCoroInBubbleForTest() {
					t.Error("child goroutine lost bubble context")
				}
				checked = true
			}()
			synctest.Wait()
			if !checked {
				t.Fatal("child did not run")
			}
			time.Sleep(time.Hour)
			if !runtime.StacklessCoroInBubbleForTest() {
				t.Fatal("timer wakeup lost bubble context")
			}
		})
		if runtime.StacklessCoroInBubbleForTest() {
			t.Fatal("bubble context leaked after Test")
		}
	}
}
