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

func TestStacklessCoroDeferAwait(t *testing.T) {
	for _, owner := range []string{"return", "panic", "goexit"} {
		for _, child := range []string{"return", "panic", "native-panic", "goexit"} {
			t.Run(owner+"/"+child, func(t *testing.T) {
				done := make(chan struct{})
				var recovered any
				var visits int
				var returned bool
				go func() {
					defer close(done)
					state, childState := 0, 0
					runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
						if state == 0 {
							state = 1
							switch owner {
							case "panic":
								runtime.PanicStacklessCoroForTest(ctx, "owner")
							case "goexit":
								runtime.GoexitStacklessCoroForTest(ctx)
							}
							runtime.DeferAwaitStacklessCoroForTest(ctx, func(childCtx unsafe.Pointer) uint8 {
								visits++
								if childState == 0 {
									childState = 1
									return runtime.StacklessCoroActionYield
								}
								if childState == 1 {
									childState = 2
									runtime.GC()
									switch child {
									case "panic":
										runtime.PanicStacklessCoroForTest(childCtx, "child")
									case "native-panic":
										panic("child")
									case "goexit":
										runtime.GoexitStacklessCoroForTest(childCtx)
									}
								}
								action := runtime.TerminalActionStacklessCoroForTest(childCtx)
								if action == runtime.StacklessCoroActionInvalid {
									return runtime.StacklessCoroActionComplete
								}
								return action
							})
							return runtime.StacklessCoroActionWait
						}
						recovered = runtime.DeferRecoverStacklessCoroForTest(runtime.DeferTokenStacklessCoroForTest(ctx))
						action := runtime.TerminalActionStacklessCoroForTest(ctx)
						if action == runtime.StacklessCoroActionInvalid {
							return runtime.StacklessCoroActionComplete
						}
						return action
					})
					returned = true
				}()
				<-done
				var want any
				if child == "panic" || child == "native-panic" {
					want = "child"
				} else if owner == "panic" && child != "goexit" {
					want = "owner"
				}
				if recovered != want {
					t.Fatalf("recovered = %v, want %v", recovered, want)
				}
				if returned != (owner != "goexit" && child != "goexit") {
					t.Fatalf("returned = %v; pending Goexit was lost or introduced", returned)
				}
				wantVisits := 2
				if child == "native-panic" {
					wantVisits++
				}
				if visits != wantVisits {
					t.Fatalf("defer resumes = %d, want %d", visits, wantVisits)
				}
			})
		}
	}
}
