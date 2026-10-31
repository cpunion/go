// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestStacklessCoroExecutorJoinGrowth(t *testing.T) {
	// Put the manager before, between, and after replacement completions. The
	// join must include growth even if the original replacements finish first.
	for _, capacity := range []int{0, runtime.StacklessCoroWarmExecutorCount} {
		for _, beforeManager := range []int{0, 2, 5} {
			t.Run(fmt.Sprintf("capacity%d/early%d", capacity, beforeManager), func(t *testing.T) {
				join := runtime.NewStacklessCoroExecutorJoinForTest(capacity)
				done := make(chan struct{})
				go func() {
					join.Join()
					close(done)
				}()
				for range min(beforeManager, 3) {
					join.Complete(false)
				}
				join.Grow()
				for i := 3; i < beforeManager; i++ {
					join.Complete(false)
				}
				join.Complete(true)
				for i := beforeManager; i < 4; i++ {
					join.Complete(false)
				}
				if beforeManager < 5 {
					select {
					case <-done:
						t.Fatal("join returned while a replacement remained live")
					case <-time.After(10 * time.Millisecond):
					}
					join.Complete(false)
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("join did not finish after every producer completed")
				}
			})
		}
	}
}
