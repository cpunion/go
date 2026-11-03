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

func TestStacklessCoroDeferAwaitRecover(t *testing.T) {
	for _, mode := range []string{"return", "panic", "goexit", "panic-during-goexit", "nil-panic"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan struct{})
			var direct, indirect any
			var returned bool
			go func() {
				defer close(done)
				parentState, childState := 0, 0
				runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
					if runtime.DeferSuspendedStacklessCoroForTest(ctx) {
						panic("cleanup resumed in waiting state")
					}
					if parentState == 0 {
						parentState = 1
						switch mode {
						case "panic":
							runtime.PanicStacklessCoroForTest(ctx, "owner")
						case "nil-panic":
							runtime.PanicStacklessCoroForTest(ctx, nil)
						case "goexit", "panic-during-goexit":
							runtime.GoexitStacklessCoroForTest(ctx)
							if mode == "panic-during-goexit" {
								runtime.DeferPanicStacklessCoroForTest(runtime.DeferTokenStacklessCoroForTest(ctx), "owner")
							}
						}
						runtime.DeferStartStacklessCoroForTest(ctx, func(childCtx unsafe.Pointer) uint8 {
							switch childState {
							case 0:
								childState = 1
								return runtime.StacklessCoroActionYield
							case 1:
								childState = 2
								runtime.AwaitStacklessCoroForTest(childCtx, func(normalCtx unsafe.Pointer) uint8 {
									indirect = runtime.DeferRecoverAwaitStacklessCoroForTest(normalCtx)
									return runtime.StacklessCoroActionComplete
								})
								return runtime.StacklessCoroActionWait
							default:
								direct = runtime.DeferRecoverAwaitStacklessCoroForTest(childCtx)
								if again := runtime.DeferRecoverAwaitStacklessCoroForTest(childCtx); again != nil {
									t.Errorf("second recover = %v, want nil", again)
								}
								return runtime.StacklessCoroActionComplete
							}
						})
						if !runtime.DeferSuspendedStacklessCoroForTest(ctx) {
							panic("cleanup starter lost handoff")
						}
						return runtime.StacklessCoroActionWait
					}
					if action := runtime.TerminalActionStacklessCoroForTest(ctx); action != runtime.StacklessCoroActionInvalid {
						return action
					}
					return runtime.StacklessCoroActionComplete
				})
				returned = true
			}()
			<-done
			if indirect != nil {
				t.Fatalf("ordinary child recovered owner panic: %v", indirect)
			}
			switch mode {
			case "panic", "panic-during-goexit":
				if direct != "owner" {
					t.Fatalf("direct recover = %v, want owner", direct)
				}
			case "nil-panic":
				if direct != nil {
					if _, ok := direct.(*runtime.PanicNilError); !ok {
						t.Fatalf("nil panic recovered as %T", direct)
					}
				}
			default:
				if direct != nil {
					t.Fatalf("direct recover without panic = %v", direct)
				}
			}
			if returned != (mode != "goexit" && mode != "panic-during-goexit") {
				t.Fatalf("returned = %v; sticky Goexit changed", returned)
			}
		})
	}
}
