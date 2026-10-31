// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
	"time"
)

func TestStacklessCoroReceivedStorageGC(t *testing.T) {
	for kind, name := range []string{"interior-frame", "external-allocation", "before-frame", "plain-promotion", "io-origin"} {
		for _, waiterView := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/operation", true: "/waiter"}[waiterView], func(t *testing.T) {
				valid, token, expected := runtime.StacklessCoroReceivedStorageGCForTest(kind, waiterView)
				if !valid {
					t.Fatal("receive result lost its typed root, scanned-slot provenance or cleared-source state")
				}
				deadline := time.Now().Add(10 * time.Second)
				for runtime.StacklessCoroReceivedStorageFinalizedForTest(token) != expected && time.Now().Before(deadline) {
					runtime.GC()
					time.Sleep(time.Millisecond)
				}
				if got := runtime.StacklessCoroReceivedStorageFinalizedForTest(token); got != expected {
					t.Fatalf("cleared receive frame/result retained: finalized=%d, want %d", got, expected)
				}
			})
		}
	}
}
