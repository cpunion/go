// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"fmt"
	"runtime"
	"testing"
)

func TestStacklessCoroClosePublication(t *testing.T) {
	for _, count := range []int{1, 2, 31, 32, 33, 257} {
		for _, schedulers := range []int{1, 3} {
			if count < schedulers {
				continue
			}
			for _, kind := range []struct{ buffered, pointers, full bool }{
				{}, {true, false, false}, {false, true, false},
				{true, true, false}, {false, true, true},
			} {
				name := fmt.Sprintf("n%d/s%d/buffered%t/pointers%t/full%t",
					count, schedulers, kind.buffered, kind.pointers, kind.full)
				t.Run(name, func(t *testing.T) {
					if !runtime.StacklessCoroClosePublicationForTest(count, schedulers,
						kind.buffered, kind.pointers, kind.full) {
						t.Fatal("close published an uncleared result, reordered a scheduler, or outlived drainage")
					}
				})
			}
		}
	}
}
