// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestStacklessCoroRootCacheOwnership(t *testing.T) {
	if !runtime.CheckStacklessCoroRootCacheOwnershipForTest() {
		t.Fatal("discarding a cached root changed non-root frame accounting")
	}
	for _, mode := range []string{"panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan struct{})
			var recovered any
			var returned bool
			go func() {
				defer close(done)
				defer func() { recovered = recover() }()
				frame := new([16]byte)
				runtime.RunCachedStacklessCoroFrameForTest(unsafe.Pointer(frame),
					func(ctx unsafe.Pointer) uint8 {
						if mode == "panic" {
							runtime.PanicStacklessCoroForTest(ctx, "cached root")
							return runtime.StacklessCoroActionPanic
						}
						runtime.GoexitStacklessCoroForTest(ctx)
						return runtime.StacklessCoroActionGoexit
					}, unsafe.Sizeof(*frame))
				returned = true
			}()
			<-done
			if returned {
				t.Fatal("cached root terminal action returned")
			}
			if mode == "panic" {
				if recovered != "cached root" {
					t.Fatalf("recover = %v, want cached root", recovered)
				}
			} else if recovered != nil {
				t.Fatalf("Goexit panic = %v", recovered)
			}
		})
	}
}
