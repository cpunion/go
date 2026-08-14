// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"bytes"
	"internal/race"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroYield(t *testing.T) {
	var state int
	var trace []int
	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
		switch state {
		case 0:
			trace = append(trace, 1)
			state = 1
			return runtime.StacklessCoroActionYield
		case 1:
			trace = append(trace, 2)
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if got, want := state, 2; got != want {
		t.Fatalf("state = %d, want %d", got, want)
	}
	if got := len(trace); got != 2 || trace[0] != 1 || trace[1] != 2 {
		t.Fatalf("trace = %v, want [1 2]", trace)
	}
}

func TestStacklessCoroForeignState(t *testing.T) {
	calls := runtime.NumCgoCall()
	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
		if incgo, noCallback, ncgo := runtime.ForeignStateStacklessCoroForTest(); incgo || noCallback || ncgo != 0 {
			t.Fatalf("initial foreign state = (%t, %t, %d), want (false, false, 0)",
				incgo, noCallback, ncgo)
		}
		runtime.EnterForeignStacklessCoroForTest()
		if incgo, noCallback, ncgo := runtime.ForeignStateStacklessCoroForTest(); !incgo || !noCallback || ncgo != 1 {
			t.Fatalf("entered foreign state = (%t, %t, %d), want (true, true, 1)",
				incgo, noCallback, ncgo)
		}
		runtime.ExitForeignStacklessCoroForTest()
		if incgo, noCallback, ncgo := runtime.ForeignStateStacklessCoroForTest(); incgo || noCallback || ncgo != 0 {
			t.Fatalf("exited foreign state = (%t, %t, %d), want (false, false, 0)",
				incgo, noCallback, ncgo)
		}
		return runtime.StacklessCoroActionComplete
	})
	if got := runtime.NumCgoCall(); got <= calls {
		t.Errorf("NumCgoCall did not record direct foreign entry: before %d, after %d",
			calls, got)
	}
}

func TestStacklessCoroAwait(t *testing.T) {
	var parentState, childState int
	var trace []string
	child := func(unsafe.Pointer) uint8 {
		switch childState {
		case 0:
			trace = append(trace, "child-yield")
			childState = 1
			return runtime.StacklessCoroActionYield
		case 1:
			trace = append(trace, "child-complete")
			childState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected child state %d", childState)
			return runtime.StacklessCoroActionInvalid
		}
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch parentState {
		case 0:
			trace = append(trace, "parent-await")
			parentState = 1
			runtime.AwaitStacklessCoroForTest(ctx, child)
			return runtime.StacklessCoroActionWait
		case 1:
			trace = append(trace, "parent-complete")
			parentState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected parent state %d", parentState)
			return runtime.StacklessCoroActionInvalid
		}
	})
	want := []string{
		"parent-await",
		"child-yield",
		"child-complete",
		"parent-complete",
	}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i := range trace {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
}

func TestStacklessCoroPanic(t *testing.T) {
	for _, value := range []any{"panic-value", nil} {
		var recovered any
		func() {
			defer func() {
				recovered = recover()
			}()
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				if runtime.PanicPendingStacklessCoroForTest(ctx) {
					t.Fatal("new root has a pending panic")
				}
				if runtime.TerminalActionStacklessCoroForTest(ctx) ==
					runtime.StacklessCoroActionPanic {
					t.Fatal("new root has a pending panic")
				}
				runtime.PanicStacklessCoroForTest(ctx, value)
				if !runtime.PanicPendingStacklessCoroForTest(ctx) {
					t.Fatal("recorded panic is not pending")
				}
				return runtime.StacklessCoroActionPanic
			})
		}()
		if value == nil {
			if recovered == nil {
				t.Fatal("panic(nil) was not propagated")
			}
		} else if recovered != value {
			t.Fatalf("recovered panic = %v, want %v", recovered, value)
		}
	}
}

func TestStacklessCoroNativePanic(t *testing.T) {
	for _, test := range []struct {
		name       string
		setting    string
		value      any
		wantNil    bool
		wantString bool
	}{
		{"value", "panicnil=0", "native panic", false, true},
		{"nil", "panicnil=0", nil, false, false},
		{"legacy-nil", "panicnil=1", nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GODEBUG", test.setting)
			state := 0
			deferred := false
			var recovered any
			func() {
				defer func() {
					deferred = true
					recovered = recover()
				}()
				runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
					switch state {
					case 0:
						state = 1
						panic(test.value)
					case 1:
						if !runtime.PanicPendingStacklessCoroForTest(ctx) {
							t.Fatal("native panic is not pending")
						}
						state = 2
						return runtime.StacklessCoroActionPanic
					default:
						t.Fatalf("unexpected resume state %d", state)
						return runtime.StacklessCoroActionInvalid
					}
				})
			}()
			if !deferred {
				t.Fatal("native panic did not unwind to recover")
			}
			if (recovered == nil) != test.wantNil {
				t.Fatalf("recovered panic = %v, want nil %t",
					recovered, test.wantNil)
			}
			if test.wantString && recovered != test.value {
				t.Fatalf("recovered panic = %v, want %v",
					recovered, test.value)
			}
			if state != 2 {
				t.Fatalf("resume state = %d, want 2", state)
			}
		})
	}
}

func TestStacklessCoroNativePanicReplacement(t *testing.T) {
	state := 0
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.PanicStacklessCoroForTest(ctx, "original panic")
				panic("replacement panic")
			case 1:
				if !runtime.PanicPendingStacklessCoroForTest(ctx) {
					t.Fatal("replacement panic is not pending")
				}
				state = 2
				return runtime.StacklessCoroActionPanic
			default:
				t.Fatalf("unexpected resume state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
	}()
	if recovered != "replacement panic" {
		t.Fatalf("recovered panic = %v, want %q",
			recovered, "replacement panic")
	}
	if state != 2 {
		t.Fatalf("resume state = %d, want 2", state)
	}
}

func TestStacklessCoroNamedDeferRecover(t *testing.T) {
	if got, want := runtime.StacklessCoroGSize, unsafe.Sizeof(uintptr(0)); got != want {
		t.Fatalf("stackless coroutine G state size = %d, want %d", got, want)
	}

	t.Run("logical", func(t *testing.T) {
		var recovered any
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.PanicStacklessCoroForTest(ctx, "logical panic")
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.DeferCallStacklessCoroForTest(token, func() {
				recovered = recover()
			})
			if action := runtime.TerminalActionStacklessCoroForTest(ctx); action !=
				runtime.StacklessCoroActionInvalid {
				t.Fatalf("terminal action after recover = %d, want invalid",
					action)
			}
			return runtime.StacklessCoroActionComplete
		})
		if recovered != "logical panic" {
			t.Fatalf("recovered panic = %v, want %q",
				recovered, "logical panic")
		}
	})

	t.Run("managed", func(t *testing.T) {
		var recovered any
		runtime.RunStacklessCoroInlineForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.PanicStacklessCoroForTest(ctx, "managed panic")
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.DeferCallStacklessCoroForTest(token, func() {
				recovered = recover()
			})
			return runtime.StacklessCoroActionComplete
		})
		if recovered != "managed panic" {
			t.Fatalf("recovered panic = %v, want %q",
				recovered, "managed panic")
		}
	})

	t.Run("native-precedence", func(t *testing.T) {
		var native, logical any
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.PanicStacklessCoroForTest(ctx, "logical panic")
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.DeferCallStacklessCoroForTest(token, func() {
				defer func() {
					native = recover()
				}()
				panic("native panic")
			})
			runtime.DeferCallStacklessCoroForTest(token, func() {
				logical = recover()
			})
			return runtime.StacklessCoroActionComplete
		})
		if native != "native panic" || logical != "logical panic" {
			t.Fatalf("recovered panics = (%v, %v), want (%q, %q)",
				native, logical, "native panic", "logical panic")
		}
	})

	t.Run("scope-cleared", func(t *testing.T) {
		state := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				runtime.DeferCallStacklessCoroForTest(token, func() {
					panic("escaped panic")
				})
				t.Fatal("defer call returned after panic")
			case 1:
				state = 2
				if value := recover(); value != nil {
					t.Fatalf("recover outside defer call = %v, want nil", value)
				}
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				if value := runtime.DeferRecoverStacklessCoroForTest(token); value != "escaped panic" {
					t.Fatalf("pending panic = %v, want %q",
						value, "escaped panic")
				}
				return runtime.StacklessCoroActionComplete
			}
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		})
		if state != 2 {
			t.Fatalf("state = %d, want 2", state)
		}
	})
}

func TestStacklessCoroDeferRun(t *testing.T) {
	t.Run("recover-parent", func(t *testing.T) {
		var recovered any
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.PanicStacklessCoroForTest(ctx, "parent panic")
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.DeferRunStacklessCoroForTest(token,
				func(unsafe.Pointer) uint8 {
					recovered = recover()
					return runtime.StacklessCoroActionComplete
				})
			if action := runtime.TerminalActionStacklessCoroForTest(ctx); action !=
				runtime.StacklessCoroActionInvalid {
				t.Fatalf("terminal action after recover = %d, want invalid",
					action)
			}
			return runtime.StacklessCoroActionComplete
		})
		if recovered != "parent panic" {
			t.Fatalf("recovered panic = %v, want %q",
				recovered, "parent panic")
		}
	})

	t.Run("replace-parent-panic", func(t *testing.T) {
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.PanicStacklessCoroForTest(ctx, "original")
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.DeferRunStacklessCoroForTest(token,
				func(childCtx unsafe.Pointer) uint8 {
					childToken := runtime.DeferTokenStacklessCoroForTest(childCtx)
					runtime.DeferPanicStacklessCoroForTest(childToken,
						"replacement")
					return runtime.StacklessCoroActionPanic
				})
			if got := runtime.DeferRecoverStacklessCoroForTest(token); got !=
				"replacement" {
				t.Fatalf("recovered panic = %v, want replacement", got)
			}
			return runtime.StacklessCoroActionComplete
		})
	})

	t.Run("Goexit-suppresses-panic", func(t *testing.T) {
		done := make(chan struct{})
		returned := make(chan struct{}, 1)
		terminal := make(chan uint8, 1)
		go func() {
			defer close(done)
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				runtime.PanicStacklessCoroForTest(ctx, "original")
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				runtime.DeferRunStacklessCoroForTest(token,
					func(childCtx unsafe.Pointer) uint8 {
						childToken :=
							runtime.DeferTokenStacklessCoroForTest(childCtx)
						if got := runtime.DeferRecoverStacklessCoroForTest(
							childToken); got != nil {
							panic("child recovered a parent panic")
						}
						runtime.DeferGoexitStacklessCoroForTest(childToken)
						return runtime.TerminalActionStacklessCoroForTest(
							childCtx)
					})
				if got := runtime.DeferRecoverStacklessCoroForTest(token); got != nil {
					panic("Goexit retained the parent panic")
				}
				action := runtime.TerminalActionStacklessCoroForTest(ctx)
				terminal <- action
				return action
			})
			returned <- struct{}{}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("nested defer Goexit did not terminate its goroutine")
		}
		if got := <-terminal; got != runtime.StacklessCoroActionGoexit {
			t.Fatalf("terminal action = %d, want Goexit", got)
		}
		select {
		case <-returned:
			t.Fatal("nested defer Goexit returned to its caller")
		default:
		}
	})

	t.Run("panic-during-Goexit", func(t *testing.T) {
		done := make(chan struct{})
		returned := make(chan struct{}, 1)
		recovered := make(chan any, 1)
		terminal := make(chan uint8, 1)
		go func() {
			defer close(done)
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				runtime.DeferRunStacklessCoroForTest(token,
					func(childCtx unsafe.Pointer) uint8 {
						childToken :=
							runtime.DeferTokenStacklessCoroForTest(childCtx)
						runtime.DeferGoexitStacklessCoroForTest(childToken)
						runtime.DeferPanicStacklessCoroForTest(childToken,
							"panic during Goexit")
						return runtime.StacklessCoroActionPanic
					})
				recovered <- runtime.DeferRecoverStacklessCoroForTest(token)
				action := runtime.TerminalActionStacklessCoroForTest(ctx)
				terminal <- action
				return action
			})
			returned <- struct{}{}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("panic recovery did not resume nested defer Goexit")
		}
		if got := <-recovered; got != "panic during Goexit" {
			t.Fatalf("recovered panic = %v, want %q",
				got, "panic during Goexit")
		}
		if got := <-terminal; got != runtime.StacklessCoroActionGoexit {
			t.Fatalf("terminal action after recover = %d, want Goexit", got)
		}
		select {
		case <-returned:
			t.Fatal("recovered panic bypassed nested defer Goexit")
		default:
		}
	})
}

func TestStacklessCoroDeferGoexit(t *testing.T) {
	done := make(chan struct{})
	returned := make(chan struct{}, 1)
	terminal := make(chan uint8, 1)
	go func() {
		defer close(done)
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.PanicStacklessCoroForTest(ctx, "suppressed")
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.DeferGoexitStacklessCoroForTest(token)
			if got := runtime.DeferRecoverStacklessCoroForTest(token); got != nil {
				panic("defer Goexit retained a panic")
			}
			action := runtime.TerminalActionStacklessCoroForTest(ctx)
			terminal <- action
			return action
		})
		returned <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("defer Goexit did not terminate its goroutine")
	}
	if got := <-terminal; got != runtime.StacklessCoroActionGoexit {
		t.Fatalf("terminal action = %d, want Goexit", got)
	}
	select {
	case <-returned:
		t.Fatal("defer Goexit returned to its caller")
	default:
	}
}

func TestStacklessCoroDeferOutcomeErrors(t *testing.T) {
	want := []string{
		"runtime: incomplete stackless coroutine defer run",
		"runtime: unexpected stackless coroutine defer value",
		"runtime: missing stackless coroutine defer panic value",
		"runtime: invalid stackless coroutine defer outcome",
	}
	got := runtime.StacklessCoroDeferOutcomeErrorsForTest()
	if len(got) != len(want) {
		t.Fatalf("defer outcome errors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("defer outcome error %d = %q, want %q",
				i, got[i], want[i])
		}
	}
}

func TestStacklessCoroNativeGoexit(t *testing.T) {
	done := make(chan struct{})
	returned := make(chan struct{}, 1)
	go func() {
		defer close(done)
		// The inline test scheduler exercises the recovery boundary without
		// asking a real Goexit to terminate an internal executor G.
		runtime.RunStacklessCoroInlineForTest(func(unsafe.Pointer) uint8 {
			runtime.Goexit()
			return runtime.StacklessCoroActionInvalid
		})
		returned <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("native Goexit did not terminate its goroutine")
	}
	select {
	case <-returned:
		t.Fatal("native Goexit returned to its caller")
	default:
	}
}

func TestStacklessCoroNativePanicDuringGoexit(t *testing.T) {
	done := make(chan struct{})
	recovered := make(chan any, 1)
	returned := make(chan struct{}, 1)
	state := 0
	recoverOutside := func() {
		defer func() {
			recovered <- recover()
		}()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.GoexitStacklessCoroForTest(ctx)
				panic("native panic during Goexit")
			case 1:
				state = 2
				return runtime.TerminalActionStacklessCoroForTest(ctx)
			default:
				panic("unexpected native Goexit resume state")
			}
		})
	}
	go func() {
		defer close(done)
		recoverOutside()
		returned <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("native panic did not resume stackless coroutine Goexit")
	}
	if got := <-recovered; got != "native panic during Goexit" {
		t.Fatalf("recovered panic = %v, want %q",
			got, "native panic during Goexit")
	}
	if state != 2 {
		t.Fatalf("resume state = %d, want 2", state)
	}
	select {
	case <-returned:
		t.Fatal("recovered native panic bypassed the pending Goexit")
	default:
	}
}

func TestStacklessCoroDetachedNativePanic(t *testing.T) {
	state := 0
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		runtime.RunDetachedStacklessCoroForTest(
			func(unsafe.Pointer) uint8 {
				return runtime.StacklessCoroActionYield
			},
			func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					state = 1
					panic("detached native panic")
				case 1:
					if !runtime.PanicPendingStacklessCoroForTest(ctx) {
						t.Fatal("detached native panic is not pending")
					}
					state = 2
					return runtime.StacklessCoroActionPanic
				default:
					t.Fatalf("unexpected detached resume state %d", state)
					return runtime.StacklessCoroActionInvalid
				}
			})
	}()
	if recovered != "detached native panic" {
		t.Fatalf("recovered panic = %v, want %q",
			recovered, "detached native panic")
	}
	if state != 2 {
		t.Fatalf("detached resume state = %d, want 2", state)
	}
}

func TestStacklessCoroPanicAwait(t *testing.T) {
	var parentState int
	var cleanup bool
	child := func(ctx unsafe.Pointer) uint8 {
		runtime.PanicStacklessCoroForTest(ctx, "child panic")
		return runtime.StacklessCoroActionPanic
	}

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0:
				parentState = 1
				runtime.AwaitStacklessCoroForTest(ctx, child)
				return runtime.StacklessCoroActionWait
			case 1:
				if !runtime.PanicPendingStacklessCoroForTest(ctx) {
					t.Fatal("child panic is not pending in its parent")
				}
				if runtime.TerminalActionStacklessCoroForTest(ctx) !=
					runtime.StacklessCoroActionPanic {
					t.Fatal("child panic was not transferred to its parent")
				}
				cleanup = true
				parentState = 2
				return runtime.StacklessCoroActionPanic
			default:
				t.Fatalf("unexpected parent state %d", parentState)
				return runtime.StacklessCoroActionInvalid
			}
		})
	}()
	if recovered != "child panic" {
		t.Fatalf("recovered panic = %v, want %q", recovered, "child panic")
	}
	if parentState != 2 || !cleanup {
		t.Fatalf("parent state = (%d, %t), want (2, true)",
			parentState, cleanup)
	}
}

func TestStacklessCoroGoexit(t *testing.T) {
	done := make(chan struct{})
	action := make(chan uint8, 1)
	returned := make(chan struct{}, 1)
	go func() {
		defer close(done)
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			runtime.GoexitStacklessCoroForTest(ctx)
			got := runtime.TerminalActionStacklessCoroForTest(ctx)
			action <- got
			return got
		})
		returned <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stackless coroutine Goexit did not terminate its goroutine")
	}
	if got := <-action; got != runtime.StacklessCoroActionGoexit {
		t.Fatalf("terminal action = %d, want Goexit", got)
	}
	select {
	case <-returned:
		t.Fatal("stackless coroutine Goexit returned to its caller")
	default:
	}
}

func TestStacklessCoroGoexitAwait(t *testing.T) {
	var parentState int
	var childCleanup, parentCleanup bool
	child := func(ctx unsafe.Pointer) uint8 {
		runtime.GoexitStacklessCoroForTest(ctx)
		childCleanup = true
		return runtime.TerminalActionStacklessCoroForTest(ctx)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0:
				parentState = 1
				runtime.AwaitStacklessCoroForTest(ctx, child)
				return runtime.StacklessCoroActionWait
			case 1:
				parentState = 2
				parentCleanup = true
				return runtime.TerminalActionStacklessCoroForTest(ctx)
			default:
				panic("unexpected parent state")
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("awaited Goexit did not terminate the root goroutine")
	}
	if parentState != 2 || !childCleanup || !parentCleanup {
		t.Fatalf("Goexit state = (%d, %t, %t), want (2, true, true)",
			parentState, childCleanup, parentCleanup)
	}
}

func TestStacklessCoroGoexitSpawn(t *testing.T) {
	var rootState int
	var childCleanup bool
	child := func(ctx unsafe.Pointer) uint8 {
		runtime.GoexitStacklessCoroForTest(ctx)
		childCleanup = true
		return runtime.TerminalActionStacklessCoroForTest(ctx)
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch rootState {
		case 0:
			rootState = 1
			runtime.SpawnStacklessCoroForTest(ctx, child)
			return runtime.StacklessCoroActionYield
		case 1:
			rootState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected root state %d", rootState)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if rootState != 2 || !childCleanup {
		t.Fatalf("spawn Goexit state = (%d, %t), want (2, true)",
			rootState, childCleanup)
	}
}

func TestStacklessCoroGoexitPanic(t *testing.T) {
	done := make(chan struct{})
	recovered := make(chan any, 1)
	returned := make(chan struct{}, 1)
	recoverOutside := func() {
		defer func() {
			recovered <- recover()
		}()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.GoexitStacklessCoroForTest(ctx)
			runtime.DeferPanicStacklessCoroForTest(token, "panic during Goexit")
			return runtime.TerminalActionStacklessCoroForTest(ctx)
		})
	}
	go func() {
		defer close(done)
		recoverOutside()
		returned <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered panic did not resume stackless coroutine Goexit")
	}
	if got := <-recovered; got != "panic during Goexit" {
		t.Fatalf("recovered panic = %v, want %q", got, "panic during Goexit")
	}
	select {
	case <-returned:
		t.Fatal("recovered panic bypassed the pending Goexit")
	default:
	}

	done = make(chan struct{})
	terminal := make(chan uint8, 1)
	go func() {
		defer close(done)
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.GoexitStacklessCoroForTest(ctx)
			runtime.DeferPanicStacklessCoroForTest(token, "recovered")
			if got := runtime.DeferRecoverStacklessCoroForTest(token); got != "recovered" {
				panic("bad stackless coroutine recover")
			}
			got := runtime.TerminalActionStacklessCoroForTest(ctx)
			terminal <- got
			return got
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("task-owned recover did not resume stackless coroutine Goexit")
	}
	if got := <-terminal; got != runtime.StacklessCoroActionGoexit {
		t.Fatalf("terminal action after recover = %d, want Goexit", got)
	}

	done = make(chan struct{})
	recovered = make(chan any, 1)
	terminal = make(chan uint8, 1)
	go func() {
		defer close(done)
		parentState := 0
		child := func(ctx unsafe.Pointer) uint8 {
			token := runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.GoexitStacklessCoroForTest(ctx)
			runtime.DeferPanicStacklessCoroForTest(token,
				"structured panic during Goexit")
			return runtime.TerminalActionStacklessCoroForTest(ctx)
		}
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0:
				parentState = 1
				runtime.AwaitStacklessCoroForTest(ctx, child)
				return runtime.StacklessCoroActionWait
			case 1:
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				recovered <- runtime.DeferRecoverStacklessCoroForTest(token)
				got := runtime.TerminalActionStacklessCoroForTest(ctx)
				terminal <- got
				return got
			default:
				panic("unexpected parent state")
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("structured recover did not resume stackless coroutine Goexit")
	}
	if got := <-recovered; got != "structured panic during Goexit" {
		t.Fatalf("structured recovered panic = %v, want %q",
			got, "structured panic during Goexit")
	}
	if got := <-terminal; got != runtime.StacklessCoroActionGoexit {
		t.Fatalf("structured terminal action after recover = %d, want Goexit",
			got)
	}
}

func TestStacklessCoroDeferTerminal(t *testing.T) {
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		token := runtime.DeferTokenStacklessCoroForTest(ctx)
		if got := runtime.DeferRecoverStacklessCoroForTest(token); got != nil {
			t.Fatalf("recover without panic = %v, want nil", got)
		}

		runtime.PanicStacklessCoroForTest(ctx, "original")
		runtime.DeferPanicStacklessCoroForTest(token, "replacement")
		if got := runtime.DeferRecoverStacklessCoroForTest(token); got != "replacement" {
			t.Fatalf("recovered panic = %v, want replacement", got)
		}
		if runtime.TerminalActionStacklessCoroForTest(ctx) ==
			runtime.StacklessCoroActionPanic {
			t.Fatal("recovered task retains a pending panic")
		}
		return runtime.StacklessCoroActionComplete
	})

	for _, test := range []struct {
		name    string
		setting string
		wantNil bool
	}{
		{"default", "panicnil=0", false},
		{"legacy", "panicnil=1", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GODEBUG", test.setting)
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				runtime.PanicStacklessCoroForTest(ctx, nil)
				got := runtime.DeferRecoverStacklessCoroForTest(token)
				if (got == nil) != test.wantNil {
					t.Fatalf("recover of panic(nil) = %v, want nil %t",
						got, test.wantNil)
				}
				return runtime.StacklessCoroActionComplete
			})
		})
	}
}

func TestStacklessCoroTaskSize(t *testing.T) {
	ptrSize := unsafe.Sizeof(uintptr(0))
	wantTaskSize := 6 * ptrSize
	if ptrSize == 4 {
		wantTaskSize = 7 * ptrSize
	}
	if got := runtime.StacklessCoroTaskSizeForTest(); got != wantTaskSize {
		t.Fatalf("stackless coroutine task size = %d, want %d", got, wantTaskSize)
	}
	if got, want := runtime.StacklessCoroTaskChunkSize, 4; got != want {
		t.Fatalf("stackless coroutine task chunk size = %d, want %d", got, want)
	}
	wantChunkSize := uintptr(192)
	if ptrSize == 4 {
		wantChunkSize = 112
	}
	if got := uintptr(runtime.StacklessCoroTaskChunkSize) *
		runtime.StacklessCoroTaskSizeForTest(); got != wantChunkSize {
		t.Fatalf("stackless coroutine task chunk bytes = %d, want %d",
			got, wantChunkSize)
	}
}

func TestStacklessCoroOperationSize(t *testing.T) {
	ptrSize := unsafe.Sizeof(uintptr(0))
	want := 22 * ptrSize
	if ptrSize == 4 {
		want = 26 * ptrSize
	}
	if got := runtime.StacklessCoroOperationSizeForTest(); got != want {
		t.Fatalf("stackless coroutine operation size = %d, want %d", got, want)
	}
}

func TestStacklessCoroSchedulerSize(t *testing.T) {
	ptrSize := unsafe.Sizeof(uintptr(0))
	want := uintptr(192)
	if ptrSize == 4 {
		want = 112
	}
	if got := runtime.StacklessCoroSchedulerSizeForTest(); got != want {
		t.Fatalf("stackless coroutine scheduler size = %d, want %d", got, want)
	}
}

type stacklessCoroFrameCacheTestFrame struct {
	value int
	total *int
}

func stacklessCoroFrameCacheResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroFrameCacheTestFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	runtime.FrameNeedsClearStacklessCoroForTest(ctx)
	*frame.total += frame.value
	frame.total = nil
	return runtime.StacklessCoroActionComplete
}

func stacklessCoroFrameCacheOtherResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroFrameCacheTestFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	*frame.total += frame.value
	frame.total = nil
	return runtime.StacklessCoroActionComplete
}

func stacklessCoroFrameCacheThirdResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroFrameCacheTestFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	*frame.total += frame.value
	frame.total = nil
	return runtime.StacklessCoroActionComplete
}

func stacklessCoroFrameCachePanicResume(ctx unsafe.Pointer) uint8 {
	runtime.PanicStacklessCoroForTest(ctx, "cached frame panic")
	return runtime.StacklessCoroActionPanic
}

func stacklessCoroFrameCacheGoexitResume(ctx unsafe.Pointer) uint8 {
	runtime.GoexitStacklessCoroForTest(ctx)
	return runtime.StacklessCoroActionGoexit
}

type stacklessCoroLargeFrameCacheTestFrame struct {
	done    *atomic.Int32
	padding [runtime.StacklessCoroFrameCacheSize / 2]byte
}

type stacklessCoroUncachedParentTestFrame struct {
	state    int
	reserved *bool
	total    *int
}

type stacklessCoroDeepFrameCacheTestFrame struct {
	depth          int
	state          int
	bypassed       *atomic.Int32
	overflowCounts *[2]int
}

type stacklessCoroFrameChunkTestFrame struct {
	depth   int
	state   int
	tracker *stacklessCoroFrameChunkTracker
}

type stacklessCoroFrameChunkTracker struct {
	chunks        int
	adjacent      int
	clearRequired int
	direct        int
	next          uintptr
	remaining     int
	invalid       bool
}

func stacklessCoroLargeFrameCacheResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroLargeFrameCacheTestFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	frame.done.Add(1)
	frame.done = nil
	return runtime.StacklessCoroActionComplete
}

func stacklessCoroUncachedParentResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroUncachedParentTestFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	switch frame.state {
	case 0:
		frame.state = 1
		child := runtime.TakeStacklessCoroFrameForTest(ctx,
			stacklessCoroFrameCacheResume,
			unsafe.Sizeof(stacklessCoroFrameCacheTestFrame{}))
		*frame.reserved =
			runtime.StacklessCoroReservedFrameCountForTest(ctx) != 0
		if child == nil {
			child = unsafe.Pointer(new(stacklessCoroFrameCacheTestFrame))
		}
		*(*stacklessCoroFrameCacheTestFrame)(child) =
			stacklessCoroFrameCacheTestFrame{value: 1, total: frame.total}
		runtime.AwaitStacklessCoroFrameForTest(ctx, child,
			stacklessCoroFrameCacheResume)
		return runtime.StacklessCoroActionWait
	case 1:
		frame.reserved = nil
		frame.total = nil
		return runtime.StacklessCoroActionComplete
	default:
		return runtime.StacklessCoroActionInvalid
	}
}

func stacklessCoroDeepFrameCacheResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroDeepFrameCacheTestFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	switch frame.state {
	case 0:
		if frame.depth == 0 {
			if frame.overflowCounts != nil {
				frame.overflowCounts[0], frame.overflowCounts[1] =
					runtime.StacklessCoroOverflowTaskCountsForTest(ctx)
			}
			frame.bypassed = nil
			frame.overflowCounts = nil
			return runtime.StacklessCoroActionComplete
		}
		frame.state = 1
		child := runtime.TakeStacklessCoroFrameForTest(ctx,
			stacklessCoroDeepFrameCacheResume,
			unsafe.Sizeof(stacklessCoroDeepFrameCacheTestFrame{}))
		if !race.Enabled &&
			runtime.StacklessCoroReservedFrameCountForTest(ctx) == 0 {
			frame.bypassed.Add(1)
		}
		if child == nil {
			child = unsafe.Pointer(new(stacklessCoroDeepFrameCacheTestFrame))
		}
		*(*stacklessCoroDeepFrameCacheTestFrame)(child) =
			stacklessCoroDeepFrameCacheTestFrame{
				depth: frame.depth - 1, bypassed: frame.bypassed,
				overflowCounts: frame.overflowCounts,
			}
		runtime.AwaitStacklessCoroFrameForTest(ctx, child,
			stacklessCoroDeepFrameCacheResume)
		return runtime.StacklessCoroActionWait
	case 1:
		frame.bypassed = nil
		frame.overflowCounts = nil
		return runtime.StacklessCoroActionComplete
	default:
		return runtime.StacklessCoroActionInvalid
	}
}

func stacklessCoroFrameChunkResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroFrameChunkTestFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	needsClear := runtime.FrameNeedsClearStacklessCoroForTest(ctx)
	if needsClear {
		frame.tracker.clearRequired++
	} else {
		frame.tracker.direct++
	}
	switch frame.state {
	case 0:
		if frame.depth == 0 {
			frame.tracker = nil
			return runtime.StacklessCoroActionComplete
		}
		frame.state = 1
		if frame.depth == 2*runtime.StacklessCoroFrameChunkSize-2 {
			other, chunk := runtime.TakeStacklessCoroFrameChunkForTest(ctx,
				stacklessCoroFrameCacheOtherResume,
				unsafe.Sizeof(stacklessCoroFrameChunkTestFrame{}))
			zero, zeroChunk := runtime.TakeStacklessCoroFrameChunkForTest(ctx,
				stacklessCoroFrameChunkResume, 0)
			large, largeChunk := runtime.TakeStacklessCoroFrameChunkForTest(ctx,
				stacklessCoroFrameChunkResume,
				runtime.StacklessCoroFrameCacheSize/
					runtime.StacklessCoroFrameChunkSize+1)
			if other != nil || chunk || zero != nil || zeroChunk ||
				large != nil || largeChunk {
				frame.tracker.invalid = true
			}
		}
		child, chunk := runtime.TakeStacklessCoroFrameChunkForTest(ctx,
			stacklessCoroFrameChunkResume,
			unsafe.Sizeof(stacklessCoroFrameChunkTestFrame{}))
		tracker := frame.tracker
		if chunk {
			if child == nil || tracker.remaining != 0 {
				tracker.invalid = true
			}
			tracker.chunks++
			tracker.next = uintptr(child) +
				unsafe.Sizeof(stacklessCoroFrameChunkTestFrame{})
			tracker.remaining = runtime.StacklessCoroFrameChunkSize - 1
		} else if child == nil {
			child = unsafe.Pointer(new(stacklessCoroFrameChunkTestFrame))
		} else if tracker.remaining != 0 {
			if uintptr(child) != tracker.next {
				tracker.invalid = true
			}
			tracker.adjacent++
			tracker.remaining--
			tracker.next += unsafe.Sizeof(stacklessCoroFrameChunkTestFrame{})
		}
		*(*stacklessCoroFrameChunkTestFrame)(child) =
			stacklessCoroFrameChunkTestFrame{
				depth: frame.depth - 1, tracker: tracker,
			}
		runtime.AwaitStacklessCoroFrameForTest(ctx, child,
			stacklessCoroFrameChunkResume)
		return runtime.StacklessCoroActionWait
	case 1:
		frame.tracker = nil
		return runtime.StacklessCoroActionComplete
	default:
		return runtime.StacklessCoroActionInvalid
	}
}

func newStacklessCoroFrameCacheTestFrame(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8, value int, total *int) unsafe.Pointer {
	frame := runtime.TakeStacklessCoroFrameForTest(ctx, resume,
		unsafe.Sizeof(stacklessCoroFrameCacheTestFrame{}))
	if frame == nil {
		frame = unsafe.Pointer(new(stacklessCoroFrameCacheTestFrame))
	}
	*(*stacklessCoroFrameCacheTestFrame)(frame) =
		stacklessCoroFrameCacheTestFrame{value: value, total: total}
	return frame
}

func TestStacklessCoroFrameCache(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		if frame := runtime.TakeStacklessCoroFrameForTest(nil,
			stacklessCoroFrameCacheResume, 1); frame != nil {
			t.Fatalf("frame without context = %p, want nil", frame)
		}
		if frame, chunk := runtime.TakeStacklessCoroFrameChunkForTest(nil,
			stacklessCoroFrameCacheResume, 1); frame != nil || chunk {
			t.Fatalf("frame chunk without context = (%p, %t), want nil, false",
				frame, chunk)
		}
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			frame := runtime.TakeStacklessCoroFrameForTest(ctx,
				stacklessCoroFrameCacheResume,
				runtime.StacklessCoroFrameCacheSize+1)
			if frame != nil {
				t.Fatalf("oversized cached frame = %p, want nil", frame)
			}
			frame, chunk := runtime.TakeStacklessCoroFrameChunkForTest(ctx,
				stacklessCoroFrameCacheResume,
				runtime.StacklessCoroFrameCacheSize+1)
			if frame != nil || chunk {
				t.Fatalf("oversized frame chunk = (%p, %t), want nil, false",
					frame, chunk)
			}
			if got := runtime.StacklessCoroReservedFrameCountForTest(ctx); got != 0 {
				t.Fatalf("unavailable frame reservations = %d, want 0", got)
			}
			return runtime.StacklessCoroActionComplete
		})
	})

	t.Run("reuse", func(t *testing.T) {
		var state, total, cachedBytes int
		var frames [2]unsafe.Pointer
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0, 1:
				index := state
				state++
				frame := newStacklessCoroFrameCacheTestFrame(ctx,
					stacklessCoroFrameCacheResume, index+1, &total)
				frames[index] = frame
				runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
					stacklessCoroFrameCacheResume)
				return runtime.StacklessCoroActionWait
			case 2:
				cachedBytes = runtime.StacklessCoroFreeFrameBytesForTest(ctx)
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected frame-cache state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if total != 3 {
			t.Fatalf("frame total = %d, want 3", total)
		}
		if got, want := frames[0] == frames[1], !race.Enabled; got != want {
			t.Fatalf("frame reused = %t, want %t", got, want)
		}
		frameSize := int(unsafe.Sizeof(stacklessCoroFrameCacheTestFrame{}))
		if !race.Enabled &&
			(cachedBytes < frameSize || cachedBytes > runtime.StacklessCoroFrameCacheSize) ||
			race.Enabled && cachedBytes != 0 {
			t.Fatalf("cached frame bytes = %d, want one bounded frame cache", cachedBytes)
		}
	})

	t.Run("native-context", func(t *testing.T) {
		var frames [runtime.StacklessCoroWarmExecutorCount + 1]unsafe.Pointer
		for i := range frames {
			state, total := 0, 0
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					state = 1
					frame := newStacklessCoroFrameCacheTestFrame(ctx,
						stacklessCoroFrameCacheResume, i+1, &total)
					frames[i] = frame
					runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
						stacklessCoroFrameCacheResume)
					return runtime.StacklessCoroActionWait
				case 1:
					state = 2
					return runtime.StacklessCoroActionComplete
				default:
					t.Fatalf("unexpected native-context state %d", state)
					return runtime.StacklessCoroActionInvalid
				}
			})
			if total != i+1 {
				t.Fatalf("native-context total = %d, want %d", total, i+1)
			}
		}
		native := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
			runtime.GOOS == "linux" && runtime.GOARCH == "amd64"
		if got, want := frames[0] == frames[len(frames)-1],
			native && !race.Enabled; got != want {
			t.Fatalf("native-context frame reused = %t, want %t", got, want)
		}
	})

	t.Run("resume-identity", func(t *testing.T) {
		var state, total int
		var frames [3]unsafe.Pointer
		resumes := [3]func(unsafe.Pointer) uint8{
			stacklessCoroFrameCacheResume,
			stacklessCoroFrameCacheOtherResume,
			stacklessCoroFrameCacheResume,
		}
		// Use a fresh inline scheduler so preexisting native-context cache
		// entries cannot make this capacity-sensitive identity test evict A.
		runtime.RunStacklessCoroInlineForTest(func(ctx unsafe.Pointer) uint8 {
			if state == len(frames) {
				return runtime.StacklessCoroActionComplete
			}
			index := state
			state++
			frames[index] = newStacklessCoroFrameCacheTestFrame(ctx,
				resumes[index], index+1, &total)
			runtime.AwaitStacklessCoroFrameForTest(ctx, frames[index],
				resumes[index])
			return runtime.StacklessCoroActionWait
		})
		if total != 6 {
			t.Fatalf("frame total = %d, want 6", total)
		}
		if frames[0] == frames[1] {
			t.Fatal("frame cache crossed resume identities")
		}
		if got, want := frames[0] == frames[2], !race.Enabled; got != want {
			t.Fatalf("first frame identity reused = %t, want %t", got, want)
		}
	})

	t.Run("reservation-order", func(t *testing.T) {
		oldProcs := runtime.GOMAXPROCS(1)
		defer runtime.GOMAXPROCS(oldProcs)

		var state, total int
		var first, reused unsafe.Pointer
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				first = newStacklessCoroFrameCacheTestFrame(ctx,
					stacklessCoroFrameCacheResume, 1, &total)
				second := newStacklessCoroFrameCacheTestFrame(ctx,
					stacklessCoroFrameCacheOtherResume, 2, &total)
				third := unsafe.Pointer(new(stacklessCoroFrameCacheTestFrame))
				*(*stacklessCoroFrameCacheTestFrame)(third) =
					stacklessCoroFrameCacheTestFrame{value: 4, total: &total}
				runtime.SpawnStacklessCoroFrameForTest(ctx, third,
					stacklessCoroFrameCacheThirdResume)
				runtime.SpawnStacklessCoroFrameForTest(ctx, first,
					stacklessCoroFrameCacheResume)
				runtime.SpawnStacklessCoroFrameForTest(ctx, second,
					stacklessCoroFrameCacheOtherResume)
				return runtime.StacklessCoroActionYield
			case 1:
				if total != 7 {
					return runtime.StacklessCoroActionYield
				}
				state = 2
				reused = newStacklessCoroFrameCacheTestFrame(ctx,
					stacklessCoroFrameCacheResume, 8, &total)
				runtime.AwaitStacklessCoroFrameForTest(ctx, reused,
					stacklessCoroFrameCacheResume)
				return runtime.StacklessCoroActionWait
			case 2:
				state = 3
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected reservation-order state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if total != 15 {
			t.Fatalf("reservation-order total = %d, want 15", total)
		}
		if got, want := reused == first, !race.Enabled; got != want {
			t.Fatalf("non-head frame reused = %t, want %t", got, want)
		}
	})

	t.Run("panic", func(t *testing.T) {
		var state, bytesBefore, bytesAfter int
		var recovered any
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				bytesBefore = runtime.StacklessCoroFreeFrameBytesForTest(ctx)
				frame := newStacklessCoroFrameCacheTestFrame(ctx,
					stacklessCoroFrameCachePanicResume, 0, new(int))
				runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
					stacklessCoroFrameCachePanicResume)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				bytesAfter = runtime.StacklessCoroFreeFrameBytesForTest(ctx)
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				recovered = runtime.DeferRecoverStacklessCoroForTest(token)
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected cached-frame panic state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if recovered != "cached frame panic" || bytesAfter > bytesBefore {
			t.Fatalf("cached-frame panic = (%v, %d -> %d bytes), want (%q, no cache growth)",
				recovered, bytesBefore, bytesAfter, "cached frame panic")
		}
	})

	t.Run("Goexit", func(t *testing.T) {
		var state, bytesBefore, bytesAfter int
		done := make(chan struct{})
		go func() {
			defer close(done)
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					state = 1
					bytesBefore = runtime.StacklessCoroFreeFrameBytesForTest(ctx)
					frame := newStacklessCoroFrameCacheTestFrame(ctx,
						stacklessCoroFrameCacheGoexitResume, 0, new(int))
					runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
						stacklessCoroFrameCacheGoexitResume)
					return runtime.StacklessCoroActionWait
				case 1:
					state = 2
					bytesAfter = runtime.StacklessCoroFreeFrameBytesForTest(ctx)
					return runtime.TerminalActionStacklessCoroForTest(ctx)
				default:
					panic("unexpected cached-frame Goexit state")
				}
			})
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cached-frame Goexit did not terminate")
		}
		if state != 2 || bytesAfter > bytesBefore {
			t.Fatalf("cached-frame Goexit = (state %d, %d -> %d bytes), want (state 2, no cache growth)",
				state, bytesBefore, bytesAfter)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		const children = 3
		var state int
		var done atomic.Int32
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				for range children {
					frame := runtime.TakeStacklessCoroFrameForTest(ctx,
						stacklessCoroLargeFrameCacheResume,
						unsafe.Sizeof(stacklessCoroLargeFrameCacheTestFrame{}))
					if frame == nil {
						frame = unsafe.Pointer(new(stacklessCoroLargeFrameCacheTestFrame))
					}
					*(*stacklessCoroLargeFrameCacheTestFrame)(frame) =
						stacklessCoroLargeFrameCacheTestFrame{done: &done}
					runtime.SpawnStacklessCoroFrameForTest(ctx, frame,
						stacklessCoroLargeFrameCacheResume)
				}
				return runtime.StacklessCoroActionYield
			case 1:
				if done.Load() != children {
					return runtime.StacklessCoroActionYield
				}
				bytes := runtime.StacklessCoroFreeFrameBytesForTest(ctx)
				if bytes > runtime.StacklessCoroFrameCacheSize {
					t.Fatalf("cached frame bytes = %d, limit %d",
						bytes, runtime.StacklessCoroFrameCacheSize)
				}
				if got, wantZero := bytes == 0, race.Enabled; got != wantZero {
					t.Fatalf("cached frame bytes = %d, race enabled %t",
						bytes, race.Enabled)
				}
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected frame-cache bound state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
	})

	t.Run("uncached-parent", func(t *testing.T) {
		var state int
		var reserved bool
		var total int
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				frame := runtime.TakeStacklessCoroFrameForTest(ctx,
					stacklessCoroUncachedParentResume,
					runtime.StacklessCoroFrameCacheSize+1)
				if frame != nil {
					t.Fatalf("oversized cached frame = %p, want nil", frame)
				}
				frame = unsafe.Pointer(new(stacklessCoroUncachedParentTestFrame))
				*(*stacklessCoroUncachedParentTestFrame)(frame) =
					stacklessCoroUncachedParentTestFrame{
						reserved: &reserved, total: &total,
					}
				runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
					stacklessCoroUncachedParentResume)
				return runtime.StacklessCoroActionWait
			case 1:
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected uncached-parent state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if got, want := reserved, !race.Enabled; got != want {
			t.Fatalf("uncached-parent child reserved = %t, want %t", got, want)
		}
		if total != 1 {
			t.Fatalf("uncached-parent total = %d, want 1", total)
		}
	})

	t.Run("deep-retention", func(t *testing.T) {
		const depth = 2 * runtime.StacklessCoroTaskCacheSize
		frameSize := unsafe.Sizeof(stacklessCoroDeepFrameCacheTestFrame{})
		var state int
		var frames [2]unsafe.Pointer
		var bypassed atomic.Int32
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0, 1:
				index := state
				if state == 1 {
					want := runtime.StacklessCoroTaskCacheSize * int(frameSize)
					if race.Enabled {
						want = 0
					}
					if got := runtime.StacklessCoroFreeFrameBytesForTest(ctx); got != want {
						t.Fatalf("cached frame bytes after deep unwind = %d, want %d",
							got, want)
					}
				}
				state++
				frame := runtime.TakeStacklessCoroFrameForTest(ctx,
					stacklessCoroDeepFrameCacheResume, frameSize)
				if frame == nil {
					frame = unsafe.Pointer(new(stacklessCoroDeepFrameCacheTestFrame))
				}
				frames[index] = frame
				*(*stacklessCoroDeepFrameCacheTestFrame)(frame) =
					stacklessCoroDeepFrameCacheTestFrame{
						depth: depth, bypassed: &bypassed,
					}
				runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
					stacklessCoroDeepFrameCacheResume)
				return runtime.StacklessCoroActionWait
			case 2:
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected deep frame-cache state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if got, want := frames[0] == frames[1], !race.Enabled; got != want {
			t.Fatalf("deep-unwind frame reused = %t, want %t", got, want)
		}
		wantBypassed := int32(0)
		if !race.Enabled {
			wantBypassed = 2 * int32(depth-runtime.StacklessCoroTaskCacheSize)
		}
		if got := bypassed.Load(); got != wantBypassed {
			t.Fatalf("deep frame-cache bypasses = %d, want %d",
				got, wantBypassed)
		}
	})

	t.Run("recursive-frame-chunks", func(t *testing.T) {
		if !runtime.StacklessCoroFrameChunkMarkerIsolationForTest() {
			t.Fatal("frame-chunk marker crossed resume identities")
		}
		valid, missing, wrongKind, wrongLength, wrongElementSize :=
			runtime.ValidStacklessCoroFrameChunkTypesForTest()
		if !valid || missing || wrongKind || wrongLength || wrongElementSize {
			t.Fatalf("frame-chunk types = (%t, %t, %t, %t, %t), want (true, false, false, false, false)",
				valid, missing, wrongKind, wrongLength, wrongElementSize)
		}
		const depth = runtime.StacklessCoroTaskCacheSize +
			runtime.StacklessCoroFrameChunkDirectCount +
			2*runtime.StacklessCoroFrameChunkSize
		var state int
		var tracker stacklessCoroFrameChunkTracker
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0, 1:
				state++
				frame, chunk := runtime.TakeStacklessCoroFrameChunkForTest(ctx,
					stacklessCoroFrameChunkResume,
					unsafe.Sizeof(stacklessCoroFrameChunkTestFrame{}))
				if chunk {
					t.Fatal("root frame requested a chunk")
				}
				if frame == nil {
					frame = unsafe.Pointer(new(stacklessCoroFrameChunkTestFrame))
				}
				*(*stacklessCoroFrameChunkTestFrame)(frame) =
					stacklessCoroFrameChunkTestFrame{
						depth: depth, tracker: &tracker,
					}
				runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
					stacklessCoroFrameChunkResume)
				return runtime.StacklessCoroActionWait
			case 2:
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected frame-chunk state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if tracker.invalid || tracker.remaining != 0 {
			t.Fatalf("invalid frame-chunk sequence: %+v", tracker)
		}
		if race.Enabled {
			if tracker.chunks != 0 || tracker.adjacent != 0 ||
				tracker.clearRequired != 0 || tracker.direct == 0 {
				t.Fatalf("race frame chunks = %+v, want distinct frames", tracker)
			}
			return
		}
		if tracker.chunks != 4 ||
			tracker.adjacent != 4*(runtime.StacklessCoroFrameChunkSize-1) ||
			tracker.clearRequired == 0 || tracker.direct == 0 {
			t.Fatalf("frame chunks = %+v, want four complete chunks", tracker)
		}
	})

	t.Run("overflow-task-chunks", func(t *testing.T) {
		const depth = runtime.StacklessCoroTaskCacheSize +
			2*runtime.StacklessCoroTaskChunkSize - 2
		var state int
		var bypassed atomic.Int32
		var deepest, unwound, afterPlain [2]int
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				frame := runtime.TakeStacklessCoroFrameForTest(ctx,
					stacklessCoroDeepFrameCacheResume,
					unsafe.Sizeof(stacklessCoroDeepFrameCacheTestFrame{}))
				if frame == nil {
					frame = unsafe.Pointer(new(stacklessCoroDeepFrameCacheTestFrame))
				}
				*(*stacklessCoroDeepFrameCacheTestFrame)(frame) =
					stacklessCoroDeepFrameCacheTestFrame{
						depth: depth, bypassed: &bypassed,
						overflowCounts: &deepest,
					}
				runtime.AwaitStacklessCoroFrameForTest(ctx, frame,
					stacklessCoroDeepFrameCacheResume)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				unwound[0], unwound[1] =
					runtime.StacklessCoroOverflowTaskCountsForTest(ctx)
				runtime.AwaitStacklessCoroForTest(ctx,
					func(unsafe.Pointer) uint8 {
						return runtime.StacklessCoroActionComplete
					})
				return runtime.StacklessCoroActionWait
			case 2:
				afterPlain[0], afterPlain[1] =
					runtime.StacklessCoroOverflowTaskCountsForTest(ctx)
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected overflow-task state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if race.Enabled {
			if deepest != [2]int{} || unwound != [2]int{} ||
				afterPlain != [2]int{} {
				t.Fatalf("race overflow-task counts = %v, %v, %v, want zero",
					deepest, unwound, afterPlain)
			}
			return
		}
		want := [2]int{
			runtime.StacklessCoroTaskChunkSize - 2,
			runtime.StacklessCoroTaskChunkSize,
		}
		if deepest != want {
			t.Fatalf("deepest overflow-task counts = %v, want %v",
				deepest, want)
		}
		if unwound != deepest {
			t.Fatalf("unwound overflow-task counts = %v, want %v",
				unwound, deepest)
		}
		if afterPlain != deepest {
			t.Fatalf("overflow-task counts after plain child = %v, want %v",
				afterPlain, deepest)
		}
	})

	t.Run("factory-panic", func(t *testing.T) {
		var state int
		var tasksBefore, tasksAfter int
		var bytesBefore, bytesAfter, reserved int
		var recovered any
		func() {
			defer func() {
				recovered = recover()
			}()
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					state = 1
					tasksBefore = runtime.StacklessCoroFreeTaskCountForTest(ctx)
					bytesBefore = runtime.StacklessCoroFreeFrameBytesForTest(ctx)
					runtime.TakeStacklessCoroFrameForTest(ctx,
						stacklessCoroFrameCacheResume,
						unsafe.Sizeof(stacklessCoroFrameCacheTestFrame{}))
					panic("frame factory")
				case 1:
					tasksAfter = runtime.StacklessCoroFreeTaskCountForTest(ctx)
					bytesAfter = runtime.StacklessCoroFreeFrameBytesForTest(ctx)
					reserved = runtime.StacklessCoroReservedFrameCountForTest(ctx)
					return runtime.StacklessCoroActionPanic
				default:
					t.Fatalf("unexpected frame panic state %d", state)
					return runtime.StacklessCoroActionInvalid
				}
			})
		}()
		if recovered != "frame factory" {
			t.Fatalf("recovered panic = %v, want frame factory", recovered)
		}
		wantTasks := tasksBefore
		if !race.Enabled && wantTasks == 0 {
			wantTasks = 1
		}
		if tasksAfter != wantTasks || bytesAfter > bytesBefore || reserved != 0 {
			t.Fatalf("cache after factory panic = (%d tasks, %d bytes, %d reservations), want (%d tasks, at most %d bytes, 0 reservations)",
				tasksAfter, bytesAfter, reserved, wantTasks, bytesBefore)
		}
	})

	t.Run("cancel-one-parent", func(t *testing.T) {
		if !runtime.StacklessCoroCancelReservedFramesForTest() {
			t.Fatal("canceling one parent did not preserve another reservation")
		}
	})

	t.Run("prefer-plain-task", func(t *testing.T) {
		if !runtime.StacklessCoroPreferPlainTaskForTest() {
			t.Fatal("ordinary task allocation discarded a cached frame")
		}
	})

	t.Run("drop-uncached-task", func(t *testing.T) {
		if !runtime.StacklessCoroUncachedTaskNotRecycledForTest() {
			t.Fatal("uncached-lineage task entered the ordinary task cache")
		}
	})

	t.Run("isolate-overflow-task", func(t *testing.T) {
		if !runtime.StacklessCoroOverflowTaskIsolationForTest() {
			t.Fatal("ordinary task allocation consumed an overflow-task slot")
		}
	})

	t.Run("select-overflow-task", func(t *testing.T) {
		if race.Enabled {
			t.Skip("race builds do not reuse task addresses")
		}
		if !runtime.StacklessCoroOverflowTaskSelectionForTest() {
			t.Fatal("overflow-task selection violated cache priority or isolation")
		}
	})
}

func TestStacklessCoroTaskReuse(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	check := func(t *testing.T, tokens [2]unsafe.Pointer) {
		t.Helper()
		if tokens[0] == nil || tokens[1] == nil {
			t.Fatalf("task tokens = %p, %p, want non-nil", tokens[0], tokens[1])
		}
		if got, want := tokens[0] == tokens[1], !race.Enabled; got != want {
			t.Fatalf("task token reused = %t, want %t", got, want)
		}
	}

	t.Run("complete", func(t *testing.T) {
		var tokens [2]unsafe.Pointer
		var childIndex, parentState int
		child := func(ctx unsafe.Pointer) uint8 {
			tokens[childIndex] = runtime.DeferTokenStacklessCoroForTest(ctx)
			childIndex++
			return runtime.StacklessCoroActionComplete
		}
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0, 1:
				parentState++
				runtime.AwaitStacklessCoroForTest(ctx, child)
				return runtime.StacklessCoroActionWait
			case 2:
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected parent state %d", parentState)
				return runtime.StacklessCoroActionInvalid
			}
		})
		check(t, tokens)
	})

	t.Run("bounded", func(t *testing.T) {
		const tasks = runtime.StacklessCoroTaskCacheSize + 1
		var completed atomic.Int32
		child := func(unsafe.Pointer) uint8 {
			completed.Add(1)
			return runtime.StacklessCoroActionComplete
		}
		var parentState int
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0:
				for range tasks {
					runtime.SpawnStacklessCoroForTest(ctx, child)
				}
				parentState = 1
				return runtime.StacklessCoroActionYield
			case 1:
				if completed.Load() != tasks {
					return runtime.StacklessCoroActionYield
				}
				want := runtime.StacklessCoroTaskCacheSize
				if race.Enabled {
					want = 0
				}
				if got := runtime.StacklessCoroFreeTaskCountForTest(ctx); got != want {
					t.Fatalf("cached tasks = %d, want %d", got, want)
				}
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected parent state %d", parentState)
				return runtime.StacklessCoroActionInvalid
			}
		})
	})

	t.Run("recovered-panic", func(t *testing.T) {
		var tokens [2]unsafe.Pointer
		var parentState int
		panicking := func(ctx unsafe.Pointer) uint8 {
			tokens[0] = runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.PanicStacklessCoroForTest(ctx, "reused panic task")
			return runtime.StacklessCoroActionPanic
		}
		completing := func(ctx unsafe.Pointer) uint8 {
			tokens[1] = runtime.DeferTokenStacklessCoroForTest(ctx)
			return runtime.StacklessCoroActionComplete
		}
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0:
				parentState = 1
				runtime.AwaitStacklessCoroForTest(ctx, panicking)
				return runtime.StacklessCoroActionWait
			case 1:
				if got := runtime.DeferRecoverStacklessCoroForTest(
					runtime.DeferTokenStacklessCoroForTest(ctx)); got != "reused panic task" {
					t.Fatalf("recovered panic = %v, want reused panic task", got)
				}
				parentState = 2
				runtime.AwaitStacklessCoroForTest(ctx, completing)
				return runtime.StacklessCoroActionWait
			case 2:
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected parent state %d", parentState)
				return runtime.StacklessCoroActionInvalid
			}
		})
		check(t, tokens)
	})

	t.Run("detached-goexit", func(t *testing.T) {
		token := make(chan unsafe.Pointer, 2)
		goexiting := func(ctx unsafe.Pointer) uint8 {
			token <- runtime.DeferTokenStacklessCoroForTest(ctx)
			runtime.GoexitStacklessCoroForTest(ctx)
			return runtime.TerminalActionStacklessCoroForTest(ctx)
		}
		completing := func(ctx unsafe.Pointer) uint8 {
			token <- runtime.DeferTokenStacklessCoroForTest(ctx)
			return runtime.StacklessCoroActionComplete
		}
		var parentState int
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0:
				parentState = 1
				runtime.SpawnStacklessCoroForTest(ctx, goexiting)
				return runtime.StacklessCoroActionYield
			case 1:
				if len(token) != 1 {
					return runtime.StacklessCoroActionYield
				}
				parentState = 2
				runtime.SpawnStacklessCoroForTest(ctx, completing)
				return runtime.StacklessCoroActionYield
			case 2:
				if len(token) != 2 {
					return runtime.StacklessCoroActionYield
				}
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected parent state %d", parentState)
				return runtime.StacklessCoroActionInvalid
			}
		})
		check(t, [2]unsafe.Pointer{<-token, <-token})
	})
}

func TestStacklessCoroSpawn(t *testing.T) {
	const tasks = 100000
	var completed atomic.Int32
	var rootState int
	child := func(unsafe.Pointer) uint8 {
		completed.Add(1)
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch rootState {
		case 0:
			for range tasks {
				runtime.SpawnStacklessCoroForTest(ctx, child)
			}
			rootState = 1
			return runtime.StacklessCoroActionYield
		case 1:
			if completed.Load() != tasks {
				return runtime.StacklessCoroActionYield
			}
			rootState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected root state %d", rootState)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroLazyExecutorChannels(t *testing.T) {
	var childDone atomic.Bool
	child := func(unsafe.Pointer) uint8 {
		childDone.Store(true)
		return runtime.StacklessCoroActionComplete
	}
	var state int
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			wake, executorWake, executorDone :=
				runtime.ExecutorChannelsStacklessCoroForTest(ctx)
			if !wake || executorWake || executorDone {
				t.Fatalf("initial channels = (%t, %t, %t), want (true, false, false)",
					wake, executorWake, executorDone)
			}
			runtime.SpawnStacklessCoroForTest(ctx, child)
			wake, executorWake, executorDone =
				runtime.ExecutorChannelsStacklessCoroForTest(ctx)
			if !wake || !executorWake || !executorDone {
				t.Fatalf("prepared channels = (%t, %t, %t), want (true, true, true)",
					wake, executorWake, executorDone)
			}
			state = 1
			return runtime.StacklessCoroActionYield
		case 1:
			if !childDone.Load() {
				return runtime.StacklessCoroActionYield
			}
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroWakePool(t *testing.T) {
	if race.Enabled {
		t.Skip("race builds retain per-scheduler wake identities")
	}
	const roots = 2 * runtime.StacklessCoroWarmExecutorCount

	oldProcs := runtime.GOMAXPROCS(runtime.StacklessCoroWarmExecutorCount)
	defer runtime.GOMAXPROCS(oldProcs)

	var ready atomic.Int32
	var release atomic.Bool
	wakes := make([]chan struct{}, roots)
	done := make(chan struct{}, roots)
	for i := range roots {
		go func() {
			state := 0
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					wakes[i] = runtime.WakeStacklessCoroForTest(ctx)
					runtime.SignalStacklessCoroForTest(ctx)
					state = 1
					ready.Add(1)
					return runtime.StacklessCoroActionYield
				case 1:
					if !release.Load() {
						return runtime.StacklessCoroActionYield
					}
					return runtime.StacklessCoroActionComplete
				default:
					panic("unexpected wake-pool state")
				}
			})
			done <- struct{}{}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for ready.Load() != roots && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if ready.Load() != roots {
		t.Fatalf("ready roots = %d, want %d", ready.Load(), roots)
	}
	for i, wake := range wakes {
		for j := range i {
			if wake == wakes[j] {
				t.Fatalf("roots %d and %d share a wake channel", i, j)
			}
		}
	}
	release.Store(true)
	timeout := time.After(5 * time.Second)
	for range roots {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("wake-pool root did not stop")
		}
	}
	if got := runtime.StacklessCoroWakePoolSizeForTest(); got !=
		runtime.StacklessCoroWarmExecutorCount {
		t.Fatalf("wake pool size = %d, want %d", got,
			runtime.StacklessCoroWarmExecutorCount)
	}
	for i := range runtime.StacklessCoroWarmExecutorCount {
		buffered := -1
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			buffered = len(runtime.WakeStacklessCoroForTest(ctx))
			return runtime.StacklessCoroActionComplete
		})
		if buffered != 0 {
			t.Fatalf("reused wake channel %d has %d notifications", i,
				buffered)
		}
	}

	native := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
		runtime.GOOS == "linux" && runtime.GOARCH == "amd64"
	if !native {
		return
	}
	resume := func(unsafe.Pointer) uint8 {
		return runtime.StacklessCoroActionComplete
	}
	if got := testing.AllocsPerRun(100, func() {
		runtime.RunStacklessCoroForTest(resume)
	}); got != 0 {
		t.Fatalf("native root allocations = %v, want 0", got)
	}
}

func TestStacklessCoroRootSchedulerPool(t *testing.T) {
	if race.Enabled {
		t.Skip("race builds retain per-root scheduler identities")
	}
	const roots = 2 * runtime.StacklessCoroWarmExecutorCount

	oldProcs := runtime.GOMAXPROCS(runtime.StacklessCoroWarmExecutorCount)
	defer runtime.GOMAXPROCS(oldProcs)

	var ready atomic.Int32
	var release atomic.Bool
	schedulers := make([]unsafe.Pointer, roots)
	done := make(chan struct{}, roots)
	for i := range roots {
		go func() {
			state := 0
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					schedulers[i] = runtime.SchedulerStacklessCoroForTest(ctx)
					state = 1
					ready.Add(1)
					return runtime.StacklessCoroActionYield
				case 1:
					if !release.Load() {
						return runtime.StacklessCoroActionYield
					}
					return runtime.StacklessCoroActionComplete
				default:
					panic("unexpected root scheduler pool state")
				}
			})
			done <- struct{}{}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for ready.Load() != roots && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if ready.Load() != roots {
		t.Fatalf("ready roots = %d, want %d", ready.Load(), roots)
	}
	for i, scheduler := range schedulers {
		if scheduler == nil {
			t.Fatalf("root %d has a nil scheduler", i)
		}
		for j := range i {
			if scheduler == schedulers[j] {
				t.Fatalf("roots %d and %d share a scheduler", i, j)
			}
		}
	}
	release.Store(true)
	timeout := time.After(5 * time.Second)
	for range roots {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("scheduler-pool root did not stop")
		}
	}
	if got := runtime.StacklessCoroRootSchedulerPoolSizeForTest(); got !=
		runtime.StacklessCoroWarmExecutorCount {
		t.Fatalf("root scheduler pool size = %d, want %d", got,
			runtime.StacklessCoroWarmExecutorCount)
	}

	channel := make(chan int, 1)
	channel <- 41
	state := 0
	value := 0
	received := false
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.RecvIntStacklessCoroForTest(ctx, channel,
				&value, &received)
			return runtime.StacklessCoroActionWait
		case 1:
			if value != 41 || !received {
				t.Fatalf("root receive = (%d, %t), want (41, true)",
					value, received)
			}
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected root receive state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if got, want := runtime.StacklessCoroRootSchedulerPoolSizeForTest(),
		runtime.StacklessCoroWarmExecutorCount-1; got != want {
		t.Fatalf("root scheduler pool size after operation = %d, want %d",
			got, want)
	}

	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		runtime.SpawnStacklessCoroForTest(ctx, func(unsafe.Pointer) uint8 {
			return runtime.StacklessCoroActionYield
		})
		return runtime.StacklessCoroActionComplete
	})
	if got, want := runtime.StacklessCoroRootSchedulerPoolSizeForTest(),
		runtime.StacklessCoroWarmExecutorCount-2; got != want {
		t.Fatalf("root scheduler pool size after detached task = %d, want %d",
			got, want)
	}
}

func TestStacklessCoroEmbeddedRoot(t *testing.T) {
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if !runtime.RootEmbeddedStacklessCoroForTest(ctx) {
			t.Fatal("root task is not embedded in its scheduler")
		}
		return runtime.StacklessCoroActionComplete
	})
}

func TestStacklessCoroResumeFrames(t *testing.T) {
	rootFrame := new(int)
	deferFrame := new(int)
	childFrame := new(int)
	spawnFrame := new(int)
	checkFrame := func(ctx, want unsafe.Pointer, name string) {
		t.Helper()
		if got := runtime.FrameStacklessCoroForTest(ctx); got != want {
			t.Fatalf("%s frame = %p, want %p", name, got, want)
		}
	}

	deferred := func(ctx unsafe.Pointer) uint8 {
		checkFrame(ctx, unsafe.Pointer(deferFrame), "defer")
		return runtime.StacklessCoroActionComplete
	}
	var childDone atomic.Bool
	child := func(ctx unsafe.Pointer) uint8 {
		checkFrame(ctx, unsafe.Pointer(childFrame), "child")
		childDone.Store(true)
		return runtime.StacklessCoroActionComplete
	}
	var spawnDone atomic.Bool
	spawned := func(ctx unsafe.Pointer) uint8 {
		checkFrame(ctx, unsafe.Pointer(spawnFrame), "spawn")
		spawnDone.Store(true)
		return runtime.StacklessCoroActionComplete
	}

	var state int
	runtime.RunStacklessCoroFrameForTest(unsafe.Pointer(rootFrame),
		func(ctx unsafe.Pointer) uint8 {
			checkFrame(ctx, unsafe.Pointer(rootFrame), "root")
			switch state {
			case 0:
				token := runtime.DeferTokenStacklessCoroForTest(ctx)
				runtime.DeferRunStacklessCoroFrameForTest(token,
					unsafe.Pointer(deferFrame), deferred)
				runtime.AwaitStacklessCoroFrameForTest(ctx,
					unsafe.Pointer(childFrame), child)
				state = 1
				return runtime.StacklessCoroActionWait
			case 1:
				if !childDone.Load() {
					t.Fatal("child did not complete")
				}
				runtime.SpawnStacklessCoroFrameForTest(ctx,
					unsafe.Pointer(spawnFrame), spawned)
				state = 2
				return runtime.StacklessCoroActionYield
			case 2:
				if !spawnDone.Load() {
					return runtime.StacklessCoroActionYield
				}
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
}

func TestStacklessCoroParallelSpawn(t *testing.T) {
	const workers = runtime.StacklessCoroWarmExecutorCount - 1

	oldProcs := runtime.GOMAXPROCS(runtime.StacklessCoroWarmExecutorCount)
	defer runtime.GOMAXPROCS(oldProcs)

	var started, completed atomic.Int32
	var release atomic.Bool
	worker := func(unsafe.Pointer) uint8 {
		started.Add(1)
		for !release.Load() {
			runtime.Gosched()
		}
		completed.Add(1)
		return runtime.StacklessCoroActionComplete
	}

	var state int
	var stalled bool
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			for range workers {
				runtime.SpawnStacklessCoroForTest(ctx, worker)
			}
			deadline := time.Now().Add(5 * time.Second)
			for started.Load() != workers && time.Now().Before(deadline) {
				runtime.Gosched()
			}
			stalled = started.Load() != workers
			release.Store(true)
			state = 1
			return runtime.StacklessCoroActionYield
		case 1:
			if completed.Load() != workers {
				return runtime.StacklessCoroActionYield
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if stalled {
		t.Fatal("spawned tasks did not run while their parent resume was active")
	}
}

func TestStacklessCoroChannel(t *testing.T) {
	channel := make(chan int, 1)
	discard := make(chan int, 1)
	discard <- 1
	sendValue := 42
	value := -1
	var received bool
	var state int

	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SendIntStacklessCoroForTest(ctx, channel, &sendValue)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			runtime.RecvIntStacklessCoroForTest(ctx, channel, &value,
				&received)
			return runtime.StacklessCoroActionWait
		case 2:
			if value != sendValue || !received {
				t.Fatalf("channel receive = (%d, %t), want (%d, true)",
					value, received, sendValue)
			}
			close(channel)
			value = -1
			received = true
			state = 3
			runtime.RecvIntStacklessCoroForTest(ctx, channel, &value,
				&received)
			return runtime.StacklessCoroActionWait
		case 3:
			if value != 0 || received {
				t.Fatalf("closed channel receive = (%d, %t), want (0, false)",
					value, received)
			}
			state = 4
			runtime.RecvIntStacklessCoroForTest(ctx, discard, nil, nil)
			return runtime.StacklessCoroActionWait
		case 4:
			state = 5
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroChannelWaiterCache(t *testing.T) {
	cache := runtime.NewStacklessCoroChannelWaiterCacheForTest()
	if got := testing.AllocsPerRun(100, cache.Cycle); got != 0 {
		t.Fatalf("cached channel waiter allocations = %v, want 0", got)
	}
	runtime.GC()
	if !cache.Valid() {
		t.Fatal("cached channel waiter was not retained across GC")
	}
	cache.Cycle()
	cache.CycleAcrossGC()
}

func TestStacklessCoroSelectStorageCache(t *testing.T) {
	cache := runtime.NewStacklessCoroSelectStorageForTest()
	cache.Cycle(2)
	if got := testing.AllocsPerRun(100, func() { cache.Cycle(2) }); got != 0 {
		t.Fatalf("cached select storage allocations = %v, want 0", got)
	}
	if locks, waiters := cache.Capacities(); locks != 2 || waiters != 2 {
		t.Fatalf("cached select capacities = (%d, %d), want (2, 2)",
			locks, waiters)
	}
	runtime.GC()
	if !cache.Valid() {
		t.Fatal("cached select storage was not retained across GC")
	}

	cache.Cycle(runtime.StacklessCoroSelectCaseCacheSize + 1)
	if locks, waiters := cache.Capacities(); locks != 0 || waiters != 0 {
		t.Fatalf("large select capacities = (%d, %d), want (0, 0)",
			locks, waiters)
	}
	if got := testing.AllocsPerRun(100, func() { cache.Cycle(2) }); got != 0 {
		t.Fatalf("recached select storage allocations = %v, want 0", got)
	}
}

func TestStacklessCoroSelectOperationReuse(t *testing.T) {
	ready := make(chan int, 1)
	ready <- 41
	channel := make(chan int, 1)
	value := -1
	sendValue := 43
	chosen := -1
	received := false
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{ready}, []unsafe.Pointer{unsafe.Pointer(&value)}, 0)
	state := 0

	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 1:
			if chosen != 0 || !received || value != 41 {
				t.Fatalf("first select = (%d, %t, %d), want (0, true, 41)",
					chosen, received, value)
			}
			state = 2
			runtime.SendIntStacklessCoroForTest(ctx, channel, &sendValue)
			return runtime.StacklessCoroActionWait
		case 2:
			if got := <-channel; got != sendValue {
				t.Fatalf("reused channel value = %d, want %d", got, sendValue)
			}
			ready <- 47
			value = -1
			chosen = -1
			received = false
			cases = runtime.NewStacklessCoroSelectCasesForTest(
				[]any{ready}, []unsafe.Pointer{unsafe.Pointer(&value)}, 0)
			state = 3
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 3:
			if chosen != 0 || !received || value != 47 {
				t.Fatalf("reused select = (%d, %t, %d), want (0, true, 47)",
					chosen, received, value)
			}
			state = 4
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroSelect(t *testing.T) {
	send := make(chan int, 1)
	ready := make(chan int, 1)
	ready <- 53
	closed := make(chan int)
	close(closed)
	var disabled chan int
	sendValue := 59
	receivedValue := -1
	chosen := -2
	received := false
	var cases *runtime.StacklessCoroSelectCasesForTest
	state := 0

	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			cases = runtime.NewStacklessCoroSelectCasesForTest(
				[]any{send}, []unsafe.Pointer{unsafe.Pointer(&sendValue)}, 1)
			state = 1
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 1:
			if chosen != 0 || received {
				t.Fatalf("buffered send select = (%d, %t), want (0, false)",
					chosen, received)
			}
			if value := <-send; value != sendValue {
				t.Fatalf("buffered send value = %d, want %d", value, sendValue)
			}
			cases = runtime.NewStacklessCoroSelectCasesForTest(
				[]any{disabled, ready},
				[]unsafe.Pointer{
					unsafe.Pointer(&sendValue),
					unsafe.Pointer(&receivedValue),
				}, 1)
			state = 2
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 2:
			if chosen != 1 || !received || receivedValue != 53 {
				t.Fatalf("ready select = (%d, %t, %d), want (1, true, 53)",
					chosen, received, receivedValue)
			}
			cases = runtime.NewStacklessCoroSelectCasesForTest(
				nil, nil, 0)
			chosen = -2
			received = true
			state = 3
			runtime.SelectStacklessCoroForTest(ctx, cases, false,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 3:
			if chosen != -1 || received {
				t.Fatalf("default select = (%d, %t), want (-1, false)",
					chosen, received)
			}
			receivedValue = -1
			cases = runtime.NewStacklessCoroSelectCasesForTest(
				[]any{closed}, []unsafe.Pointer{
					unsafe.Pointer(&receivedValue),
				}, 0)
			chosen = -2
			received = true
			state = 4
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 4:
			if chosen != 0 || received || receivedValue != 0 {
				t.Fatalf("closed receive select = (%d, %t, %d), want (0, false, 0)",
					chosen, received, receivedValue)
			}
			state = 5
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroSelectBlocked(t *testing.T) {
	first := make(chan int)
	second := make(chan int)
	var disabled chan int
	value := -1
	chosen := -1
	received := false
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{first, disabled, second},
		[]unsafe.Pointer{
			unsafe.Pointer(&value), unsafe.Pointer(&value), unsafe.Pointer(&value),
		}, 0)
	baselineOperations := runtime.StacklessCoroOperationCountForTest()
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		deadline := time.Now().Add(5 * time.Second)
		for {
			firstSend, firstRecv, firstLogical :=
				runtime.StacklessCoroChannelWaitersForTest(first)
			secondSend, secondRecv, secondLogical :=
				runtime.StacklessCoroChannelWaitersForTest(second)
			if firstSend == 0 && firstRecv == 1 && firstLogical == 1 &&
				secondSend == 0 && secondRecv == 1 && secondLogical == 1 {
				second <- 61
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("select waiters were not queued: first=(%d,%d,%d) second=(%d,%d,%d)",
					firstSend, firstRecv, firstLogical,
					secondSend, secondRecv, secondLogical)
				close(first)
				return
			}
			runtime.Gosched()
		}
	}()

	state := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 1:
			if chosen != 2 || !received || value != 61 {
				t.Fatalf("blocked select = (%d, %t, %d), want (2, true, 61)",
					chosen, received, value)
			}
			for _, channel := range []chan int{first, second} {
				send, recv, logical :=
					runtime.StacklessCoroChannelWaitersForTest(channel)
				if send != 0 || recv != 0 || logical != 0 {
					t.Fatalf("select loser remained queued: (%d, %d, %d)",
						send, recv, logical)
				}
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	<-peerDone
	if operations := runtime.StacklessCoroOperationCountForTest(); operations != baselineOperations {
		t.Fatalf("operation count = %d, want %d",
			operations, baselineOperations)
	}
}

func TestStacklessCoroSelectReadyPeers(t *testing.T) {
	t.Run("Receiver", func(t *testing.T) {
		channel := make(chan int)
		result := make(chan int, 1)
		go func() {
			result <- <-channel
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send == 0 && recv == 1 && logical == 0 {
				break
			}
			if time.Now().After(deadline) {
				close(channel)
				t.Fatal("ordinary select receiver was not queued")
			}
			runtime.Gosched()
		}

		value := 63
		chosen := -1
		received := true
		cases := runtime.NewStacklessCoroSelectCasesForTest(
			[]any{channel}, []unsafe.Pointer{unsafe.Pointer(&value)}, 1)
		state := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.SelectStacklessCoroForTest(ctx, cases, true,
					&chosen, &received)
				return runtime.StacklessCoroActionWait
			case 1:
				if chosen != 0 || received {
					t.Fatalf("send select = (%d, %t), want (0, false)",
						chosen, received)
				}
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		if got := <-result; got != value {
			t.Fatalf("ordinary receiver got %d, want %d", got, value)
		}
	})

	t.Run("Sender", func(t *testing.T) {
		channel := make(chan int)
		sent := make(chan struct{})
		go func() {
			channel <- 65
			close(sent)
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send == 1 && recv == 0 && logical == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("ordinary select sender was not queued")
			}
			runtime.Gosched()
		}

		value := -1
		chosen := -1
		received := false
		cases := runtime.NewStacklessCoroSelectCasesForTest(
			[]any{channel}, []unsafe.Pointer{unsafe.Pointer(&value)}, 0)
		state := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.SelectStacklessCoroForTest(ctx, cases, true,
					&chosen, &received)
				return runtime.StacklessCoroActionWait
			case 1:
				if chosen != 0 || !received || value != 65 {
					t.Fatalf("receive select = (%d, %t, %d), want (0, true, 65)",
						chosen, received, value)
				}
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		<-sent
	})
}

func TestStacklessCoroSelectBlockedSend(t *testing.T) {
	first := make(chan int)
	second := make(chan int)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	firstValue := 67
	secondValue := 69
	var timeoutValue time.Time
	chosen := -1
	received := true
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{first, second, timer.C},
		[]unsafe.Pointer{
			unsafe.Pointer(&firstValue),
			unsafe.Pointer(&secondValue),
			unsafe.Pointer(&timeoutValue),
		}, 2)
	peerResult := make(chan int, 1)
	go func() {
		for {
			firstSend, firstRecv, firstLogical :=
				runtime.StacklessCoroChannelWaitersForTest(first)
			secondSend, secondRecv, secondLogical :=
				runtime.StacklessCoroChannelWaitersForTest(second)
			if firstSend == 1 && firstRecv == 0 && firstLogical == 1 &&
				secondSend == 1 && secondRecv == 0 && secondLogical == 1 {
				peerResult <- <-second
				return
			}
			runtime.Gosched()
		}
	}()

	state := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 1:
			if chosen != 1 || received {
				t.Fatalf("blocked send select = (%d, %t), want (1, false)",
					chosen, received)
			}
			for _, channel := range []any{first, second, timer.C} {
				send, recv, logical :=
					runtime.StacklessCoroChannelWaitersForTest(channel)
				if send != 0 || recv != 0 || logical != 0 {
					t.Fatalf("send select loser remained queued: (%d, %d, %d)",
						send, recv, logical)
				}
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	if value := <-peerResult; value != secondValue {
		t.Fatalf("blocked select sent %d, want %d", value, secondValue)
	}
}

func TestStacklessCoroSelectTimer(t *testing.T) {
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	var value time.Time
	chosen := -1
	received := false
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{timer.C}, []unsafe.Pointer{unsafe.Pointer(&value)}, 0)
	state := 0

	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 1:
			if chosen != 0 || !received || value.IsZero() {
				t.Fatalf("timer select = (%d, %t, %v), want timer value",
					chosen, received, value)
			}
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(timer.C)
			if send != 0 || recv != 0 || logical != 0 {
				t.Fatalf("timer select waiter remained queued: (%d, %d, %d)",
					send, recv, logical)
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroSelectClose(t *testing.T) {
	first := make(chan int)
	second := make(chan int)
	value := -1
	chosen := -1
	received := true
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{first, first, second},
		[]unsafe.Pointer{
			unsafe.Pointer(&value),
			unsafe.Pointer(&value),
			unsafe.Pointer(&value),
		}, 0)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, firstRecv, _ :=
				runtime.StacklessCoroChannelWaitersForTest(first)
			_, secondRecv, _ :=
				runtime.StacklessCoroChannelWaitersForTest(second)
			if firstRecv == 2 && secondRecv == 1 {
				close(first)
				return
			}
			if time.Now().After(deadline) {
				t.Error("select close waiters were not queued")
				close(first)
				return
			}
			runtime.Gosched()
		}
	}()

	state := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SelectStacklessCoroForTest(ctx, cases, true,
				&chosen, &received)
			return runtime.StacklessCoroActionWait
		case 1:
			if (chosen != 0 && chosen != 1) || received || value != 0 {
				t.Fatalf("closed select = (%d, %t, %d), want duplicate closed case",
					chosen, received, value)
			}
			_, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(second)
			if recv != 0 || logical != 0 {
				t.Fatalf("closed select loser remained queued: (%d, %d)",
					recv, logical)
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroSelectBlockedSendPanic(t *testing.T) {
	if race.Enabled {
		t.Skip("concurrent channel close and send is a data race")
	}
	channel := make(chan int)
	value := 71
	chosen := -1
	received := false
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{channel}, []unsafe.Pointer{unsafe.Pointer(&value)}, 1)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		deadline := time.Now().Add(5 * time.Second)
		for {
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send == 1 && recv == 0 && logical == 1 {
				close(channel)
				return
			}
			if time.Now().After(deadline) {
				t.Error("blocked select sender was not queued")
				close(channel)
				return
			}
			runtime.Gosched()
		}
	}()

	state := 0
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if action := runtime.TerminalActionStacklessCoroForTest(ctx); action !=
				runtime.StacklessCoroActionInvalid {
				return action
			}
			switch state {
			case 0:
				state = 1
				runtime.SelectStacklessCoroForTest(ctx, cases, true,
					&chosen, &received)
				return runtime.StacklessCoroActionWait
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
	}()
	<-peerDone
	err, ok := recovered.(error)
	if !ok || err.Error() != "send on closed channel" {
		t.Fatalf("recovered blocked select panic = %v, want send on closed channel",
			recovered)
	}
}

func TestStacklessCoroSelectPanic(t *testing.T) {
	channel := make(chan int)
	close(channel)
	value := 67
	chosen := -1
	received := false
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{channel}, []unsafe.Pointer{unsafe.Pointer(&value)}, 1)
	state := 0
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if action := runtime.TerminalActionStacklessCoroForTest(ctx); action !=
				runtime.StacklessCoroActionInvalid {
				return action
			}
			switch state {
			case 0:
				state = 1
				runtime.SelectStacklessCoroForTest(ctx, cases, true,
					&chosen, &received)
				return runtime.StacklessCoroActionWait
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
	}()
	err, ok := recovered.(error)
	if !ok || err.Error() != "send on closed channel" {
		t.Fatalf("recovered select panic = %v, want send on closed channel",
			recovered)
	}
}

func TestStacklessCoroChannelPanic(t *testing.T) {
	channel := make(chan int)
	close(channel)
	value := 1
	state := 0
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if action := runtime.TerminalActionStacklessCoroForTest(ctx); action !=
				runtime.StacklessCoroActionInvalid {
				return action
			}
			switch state {
			case 0:
				state = 1
				runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
				return runtime.StacklessCoroActionWait
			default:
				t.Fatalf("unexpected state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
	}()
	err, ok := recovered.(error)
	if !ok || err.Error() != "send on closed channel" {
		t.Fatalf("recovered channel panic = %v, want send on closed channel",
			recovered)
	}
}

func TestStacklessCoroChannelProgress(t *testing.T) {
	const senderCount = 64

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	baselineGoroutines := runtime.NumGoroutine()
	baselineOperations := runtime.StacklessCoroOperationCountForTest()

	type senderState struct {
		state int
		value int
	}
	senders := make([]senderState, senderCount)
	channel := make(chan int)
	var completed atomic.Int32
	var rootState, receivedCount, value, sum int
	var waiting, received bool

	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch rootState {
		case 0:
			for i := range senders {
				sender := &senders[i]
				sender.value = i + 1
				runtime.SpawnStacklessCoroForTest(ctx,
					func(childCtx unsafe.Pointer) uint8 {
						switch sender.state {
						case 0:
							sender.state = 1
							runtime.SendIntStacklessCoroForTest(childCtx,
								channel, &sender.value)
							return runtime.StacklessCoroActionWait
						case 1:
							sender.state = 2
							completed.Add(1)
							return runtime.StacklessCoroActionComplete
						default:
							return runtime.StacklessCoroActionInvalid
						}
					})
			}
			rootState = 1
			return runtime.StacklessCoroActionYield

		case 1:
			sendWaiters, recvWaiters, logicalWaiters :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if sendWaiters != senderCount {
				return runtime.StacklessCoroActionYield
			}
			if recvWaiters != 0 || logicalWaiters != senderCount {
				t.Fatalf("channel waiters = (%d, %d, %d), want (%d, 0, %d)",
					sendWaiters, recvWaiters, logicalWaiters,
					senderCount, senderCount)
			}
			if goroutines := runtime.NumGoroutine(); goroutines >
				baselineGoroutines+16 {
				t.Fatalf("blocked channel operations added %d goroutines",
					goroutines-baselineGoroutines)
			}
			rootState = 2
			return runtime.StacklessCoroActionYield

		case 2:
			if waiting {
				if !received {
					t.Fatal("unbuffered channel receive reported closed")
				}
				sum += value
				receivedCount++
				waiting = false
			}
			if receivedCount == senderCount {
				rootState = 3
				return runtime.StacklessCoroActionYield
			}
			value = 0
			received = false
			waiting = true
			runtime.RecvIntStacklessCoroForTest(ctx, channel, &value,
				&received)
			return runtime.StacklessCoroActionWait

		case 3:
			if completed.Load() != senderCount {
				return runtime.StacklessCoroActionYield
			}
			rootState = 4
			return runtime.StacklessCoroActionComplete

		default:
			t.Fatalf("unexpected root state %d", rootState)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if want := senderCount * (senderCount + 1) / 2; sum != want {
		t.Fatalf("channel sum = %d, want %d", sum, want)
	}
	for i := range senders {
		if senders[i].state != 2 {
			t.Fatalf("sender %d state = %d, want 2", i, senders[i].state)
		}
	}
	if operations := runtime.StacklessCoroOperationCountForTest(); operations != baselineOperations {
		t.Fatalf("operation count = %d, want %d",
			operations, baselineOperations)
	}
}

func waitStacklessCoroChannelPeer(queued *atomic.Bool) {
	for !queued.Load() {
		runtime.Gosched()
	}
}

func TestStacklessCoroChannelInterop(t *testing.T) {
	t.Run("WaitingOrdinaryReceiver", func(t *testing.T) {
		channel := make(chan int)
		result := make(chan int, 1)
		go func() {
			result <- <-channel
		}()
		for {
			sendWaiters, recvWaiters, logicalWaiters :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if sendWaiters == 0 && recvWaiters == 1 &&
				logicalWaiters == 0 {
				break
			}
			runtime.Gosched()
		}

		state := 0
		value := 37
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		if got := <-result; got != value {
			t.Fatalf("receive = %d, want %d", got, value)
		}
	})

	t.Run("WaitingOrdinarySender", func(t *testing.T) {
		channel := make(chan int)
		sent := make(chan struct{})
		go func() {
			channel <- 39
			close(sent)
		}()
		for {
			sendWaiters, recvWaiters, logicalWaiters :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if sendWaiters == 1 && recvWaiters == 0 &&
				logicalWaiters == 0 {
				break
			}
			runtime.Gosched()
		}

		state := 0
		value := -1
		var received bool
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.RecvIntStacklessCoroForTest(ctx, channel, &value,
					&received)
				return runtime.StacklessCoroActionWait
			case 1:
				if value != 39 || !received {
					t.Fatalf("receive = (%d, %t), want (39, true)",
						value, received)
				}
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		<-sent
	})

	t.Run("UnbufferedReceive", func(t *testing.T) {
		channel := make(chan int)
		var queued atomic.Bool
		sent := make(chan struct{})
		go func() {
			waitStacklessCoroChannelPeer(&queued)
			channel <- 41
			close(sent)
		}()

		state := 0
		value := -1
		var received bool
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.RecvIntStacklessCoroForTest(ctx, channel, &value,
					&received)
				sendWaiters, recvWaiters, logicalWaiters :=
					runtime.StacklessCoroChannelWaitersForTest(channel)
				if sendWaiters != 0 || recvWaiters != 1 ||
					logicalWaiters != 1 {
					t.Fatalf("channel waiters = (%d, %d, %d), want (0, 1, 1)",
						sendWaiters, recvWaiters, logicalWaiters)
				}
				queued.Store(true)
				return runtime.StacklessCoroActionWait
			case 1:
				if value != 41 || !received {
					t.Fatalf("receive = (%d, %t), want (41, true)",
						value, received)
				}
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		<-sent
	})

	t.Run("UnbufferedSend", func(t *testing.T) {
		channel := make(chan int)
		var queued atomic.Bool
		result := make(chan int, 1)
		go func() {
			waitStacklessCoroChannelPeer(&queued)
			result <- <-channel
		}()

		state := 0
		value := 43
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
				sendWaiters, recvWaiters, logicalWaiters :=
					runtime.StacklessCoroChannelWaitersForTest(channel)
				if sendWaiters != 1 || recvWaiters != 0 ||
					logicalWaiters != 1 {
					t.Fatalf("channel waiters = (%d, %d, %d), want (1, 0, 1)",
						sendWaiters, recvWaiters, logicalWaiters)
				}
				queued.Store(true)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		if got := <-result; got != value {
			t.Fatalf("receive = %d, want %d", got, value)
		}
	})

	t.Run("BufferedFullSend", func(t *testing.T) {
		channel := make(chan int, 1)
		channel <- 47
		var queued atomic.Bool
		result := make(chan [2]int, 1)
		go func() {
			waitStacklessCoroChannelPeer(&queued)
			result <- [2]int{<-channel, <-channel}
		}()

		state := 0
		value := 53
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
				sendWaiters, recvWaiters, logicalWaiters :=
					runtime.StacklessCoroChannelWaitersForTest(channel)
				if sendWaiters != 1 || recvWaiters != 0 ||
					logicalWaiters != 1 {
					t.Fatalf("channel waiters = (%d, %d, %d), want (1, 0, 1)",
						sendWaiters, recvWaiters, logicalWaiters)
				}
				queued.Store(true)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		if got := <-result; got != [2]int{47, value} {
			t.Fatalf("receives = %v, want [47 %d]", got, value)
		}
	})

	t.Run("BufferedWaitingReceive", func(t *testing.T) {
		channel := make(chan int, 1)
		var queued atomic.Bool
		sent := make(chan struct{})
		go func() {
			waitStacklessCoroChannelPeer(&queued)
			channel <- 59
			close(sent)
		}()

		state := 0
		value := -1
		var received bool
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.RecvIntStacklessCoroForTest(ctx, channel, &value,
					&received)
				sendWaiters, recvWaiters, logicalWaiters :=
					runtime.StacklessCoroChannelWaitersForTest(channel)
				if sendWaiters != 0 || recvWaiters != 1 ||
					logicalWaiters != 1 {
					t.Fatalf("channel waiters = (%d, %d, %d), want (0, 1, 1)",
						sendWaiters, recvWaiters, logicalWaiters)
				}
				queued.Store(true)
				return runtime.StacklessCoroActionWait
			case 1:
				if value != 59 || !received {
					t.Fatalf("receive = (%d, %t), want (59, true)",
						value, received)
				}
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		<-sent
	})

	t.Run("ZeroSizeBufferedSend", func(t *testing.T) {
		channel := make(chan struct{}, 1)
		channel <- struct{}{}
		var queued atomic.Bool
		done := make(chan struct{})
		go func() {
			waitStacklessCoroChannelPeer(&queued)
			<-channel
			<-channel
			close(done)
		}()

		state := 0
		value := struct{}{}
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.SendStacklessCoroForTest(ctx, channel,
					unsafe.Pointer(&value))
				send, recv, logical :=
					runtime.StacklessCoroChannelWaitersForTest(channel)
				if send != 1 || recv != 0 || logical != 1 {
					t.Fatalf("channel waiters = (%d, %d, %d), want (1, 0, 1)",
						send, recv, logical)
				}
				queued.Store(true)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		<-done
	})
}

func TestStacklessCoroChannelSynchronization(t *testing.T) {
	channel := make(chan int, 1)
	channel <- 1
	var shared int
	result := make(chan int, 1)
	go func() {
		for {
			send, _, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send == 1 && logical == 1 {
				break
			}
			runtime.Gosched()
		}
		<-channel
		<-channel
		result <- shared
	}()

	state := 0
	value := 2
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			shared = 63
			runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	if got := <-result; got != shared {
		t.Fatalf("synchronized value = %d, want %d", got, shared)
	}
}

func TestStacklessCoroChannelCloseWaiter(t *testing.T) {
	t.Run("Receive", func(t *testing.T) {
		channel := make(chan int)
		var queued atomic.Bool
		closed := make(chan struct{})
		go func() {
			waitStacklessCoroChannelPeer(&queued)
			close(channel)
			close(closed)
		}()

		state := 0
		value := -1
		received := true
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.RecvIntStacklessCoroForTest(ctx, channel, &value,
					&received)
				queued.Store(true)
				return runtime.StacklessCoroActionWait
			case 1:
				if value != 0 || received {
					t.Fatalf("closed receive = (%d, %t), want (0, false)",
						value, received)
				}
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		<-closed
	})

	t.Run("Send", func(t *testing.T) {
		channel := make(chan int)
		var queued atomic.Bool
		closed := make(chan struct{})
		go func() {
			waitStacklessCoroChannelPeer(&queued)
			close(channel)
			close(closed)
		}()

		state := 0
		value := 61
		var recovered any
		func() {
			defer func() {
				recovered = recover()
			}()
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				if action := runtime.TerminalActionStacklessCoroForTest(ctx); action !=
					runtime.StacklessCoroActionInvalid {
					return action
				}
				switch state {
				case 0:
					state = 1
					runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
					queued.Store(true)
					return runtime.StacklessCoroActionWait
				default:
					return runtime.StacklessCoroActionInvalid
				}
			})
		}()
		<-closed
		err, ok := recovered.(error)
		if !ok || err.Error() != "send on closed channel" {
			t.Fatalf("recovered channel panic = %v, want send on closed channel",
				recovered)
		}
	})
}

func TestStacklessCoroChannelCloseMany(t *testing.T) {
	const waiters = 4

	type result struct {
		value     int
		received  bool
		recovered any
	}
	waitForQueue := func(t *testing.T, channel chan int, wantSend int,
		queued *atomic.Int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if queued.Load() != waiters {
				if time.Now().After(deadline) {
					t.Fatalf("queued waiters = %d, want %d",
						queued.Load(), waiters)
				}
				runtime.Gosched()
				continue
			}
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send == wantSend && recv == waiters-wantSend &&
				logical == waiters {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("channel waiters = (%d, %d, %d), want (%d, %d, %d)",
					send, recv, logical, wantSend, waiters-wantSend,
					waiters)
			}
			runtime.Gosched()
		}
	}

	t.Run("Receive", func(t *testing.T) {
		channel := make(chan int)
		results := make(chan result, waiters)
		var queued atomic.Int32
		for range waiters {
			go func() {
				state := 0
				value := -1
				received := true
				runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
					switch state {
					case 0:
						state = 1
						runtime.RecvIntStacklessCoroForTest(ctx, channel,
							&value, &received)
						queued.Add(1)
						return runtime.StacklessCoroActionWait
					case 1:
						state = 2
						return runtime.StacklessCoroActionComplete
					default:
						return runtime.StacklessCoroActionInvalid
					}
				})
				results <- result{value: value, received: received}
			}()
		}
		waitForQueue(t, channel, 0, &queued)
		close(channel)
		for range waiters {
			got := <-results
			if got.value != 0 || got.received {
				t.Fatalf("closed receive = (%d, %t), want (0, false)",
					got.value, got.received)
			}
		}
	})

	t.Run("Send", func(t *testing.T) {
		channel := make(chan int)
		results := make(chan result, waiters)
		var queued atomic.Int32
		for i := range waiters {
			go func(value int) {
				state := 0
				var recovered any
				func() {
					defer func() {
						recovered = recover()
					}()
					runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
						if action :=
							runtime.TerminalActionStacklessCoroForTest(ctx); action !=
							runtime.StacklessCoroActionInvalid {
							return action
						}
						switch state {
						case 0:
							state = 1
							runtime.SendIntStacklessCoroForTest(ctx, channel,
								&value)
							queued.Add(1)
							return runtime.StacklessCoroActionWait
						default:
							return runtime.StacklessCoroActionInvalid
						}
					})
				}()
				results <- result{recovered: recovered}
			}(i)
		}
		waitForQueue(t, channel, waiters, &queued)
		close(channel)
		for range waiters {
			got := <-results
			err, ok := got.recovered.(error)
			if !ok || err.Error() != "send on closed channel" {
				t.Fatalf("recovered channel panic = %v, want send on closed channel",
					got.recovered)
			}
		}
	})
}

func TestStacklessCoroChannelFIFO(t *testing.T) {
	channel := make(chan int)
	values := [...]int{1, 3}
	states := [len(values)]int{}
	var completed atomic.Int32
	sender := func(index int) func(unsafe.Pointer) uint8 {
		return func(ctx unsafe.Pointer) uint8 {
			switch states[index] {
			case 0:
				states[index] = 1
				runtime.SendIntStacklessCoroForTest(ctx, channel,
					&values[index])
				return runtime.StacklessCoroActionWait
			case 1:
				states[index] = 2
				completed.Add(1)
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		}
	}

	result := make(chan [3]int, 1)
	var rootState int
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch rootState {
		case 0:
			runtime.SpawnStacklessCoroForTest(ctx, sender(0))
			rootState = 1
			return runtime.StacklessCoroActionYield
		case 1:
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send != 1 {
				return runtime.StacklessCoroActionYield
			}
			if recv != 0 || logical != 1 {
				t.Fatalf("channel waiters = (%d, %d, %d), want (1, 0, 1)",
					send, recv, logical)
			}
			go func() {
				channel <- 2
			}()
			rootState = 2
			return runtime.StacklessCoroActionYield
		case 2:
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send != 2 {
				return runtime.StacklessCoroActionYield
			}
			if recv != 0 || logical != 1 {
				t.Fatalf("channel waiters = (%d, %d, %d), want (2, 0, 1)",
					send, recv, logical)
			}
			runtime.SpawnStacklessCoroForTest(ctx, sender(1))
			rootState = 3
			return runtime.StacklessCoroActionYield
		case 3:
			send, recv, logical :=
				runtime.StacklessCoroChannelWaitersForTest(channel)
			if send != 3 {
				return runtime.StacklessCoroActionYield
			}
			if recv != 0 || logical != 2 {
				t.Fatalf("channel waiters = (%d, %d, %d), want (3, 0, 2)",
					send, recv, logical)
			}
			go func() {
				result <- [3]int{<-channel, <-channel, <-channel}
			}()
			rootState = 4
			return runtime.StacklessCoroActionYield
		case 4:
			select {
			case got := <-result:
				if got != [3]int{1, 2, 3} {
					t.Fatalf("channel order = %v, want [1 2 3]", got)
				}
				rootState = 5
			default:
				return runtime.StacklessCoroActionYield
			}
			fallthrough
		case 5:
			if completed.Load() != int32(len(values)) {
				return runtime.StacklessCoroActionYield
			}
			rootState = 6
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	for i, state := range states {
		if state != 2 {
			t.Fatalf("sender %d state = %d, want 2", i, state)
		}
	}
}

func TestStacklessCoroTimerChannel(t *testing.T) {
	const iterations = 100

	baselineOperations := runtime.StacklessCoroOperationCountForTest()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	var value time.Time
	var received, pending bool
	var completed int
	resetTimer := func(unsafe.Pointer) uint8 {
		timer.Stop()
		timer.Reset(time.Nanosecond)
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if pending {
			if value.IsZero() || !received {
				t.Fatalf("timer receive = (%v, %t), want non-zero and true",
					value, received)
			}
			completed++
			pending = false
		}
		if completed == iterations {
			return runtime.StacklessCoroActionComplete
		}
		if completed != 0 {
			timer.Reset(time.Hour)
		}
		value = time.Time{}
		received = false
		pending = true
		runtime.SpawnStacklessCoroForTest(ctx, resetTimer)
		runtime.RecvStacklessCoroForTest(ctx, timer.C,
			unsafe.Pointer(&value), &received)
		return runtime.StacklessCoroActionWait
	})
	if operations := runtime.StacklessCoroOperationCountForTest(); operations !=
		baselineOperations {
		t.Fatalf("operation count = %d, want %d",
			operations, baselineOperations)
	}
	if send, recv, logical :=
		runtime.StacklessCoroChannelWaitersForTest(timer.C); send != 0 ||
		recv != 0 || logical != 0 {
		t.Fatalf("timer waiters = (%d, %d, %d), want (0, 0, 0)",
			send, recv, logical)
	}
}

func TestStacklessCoroSleep(t *testing.T) {
	const delay = 5 * time.Millisecond
	var state int
	start := time.Now()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			if !runtime.SleepStacklessCoroForTest(ctx, int64(delay)) {
				t.Fatal("positive sleep did not start a timer")
			}
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("sleep returned after %v, want at least %v", elapsed, delay)
	}
}

func TestStacklessCoroSleepNonpositive(t *testing.T) {
	baselineOperations := runtime.StacklessCoroOperationCountForTest()
	for _, delay := range []int64{-1, 0} {
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if runtime.SleepStacklessCoroForTest(ctx, delay) {
				t.Fatalf("sleep(%d) started a timer", delay)
			}
			return runtime.StacklessCoroActionComplete
		})
	}
	if operations := runtime.StacklessCoroOperationCountForTest(); operations !=
		baselineOperations {
		t.Fatalf("operation count = %d, want %d",
			operations, baselineOperations)
	}
}

func TestStacklessCoroSleepProgress(t *testing.T) {
	const delay = 5 * time.Second
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	var state int
	var token runtime.StacklessCoroTimerTokenForTest
	var progressed, canceled bool
	progress := func(unsafe.Pointer) uint8 {
		progressed = true
		canceled = runtime.CancelSleepStacklessCoroForTest(token)
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SpawnStacklessCoroForTest(ctx, progress)
			token = runtime.StartSleepStacklessCoroForTest(ctx, int64(delay))
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if !progressed || !canceled {
		t.Fatalf("sibling progress = (%v, %v), want (true, true)",
			progressed, canceled)
	}
}

func TestStacklessCoroSleepCancel(t *testing.T) {
	const maxDuration = int64(^uint64(0) >> 1)
	var state, round int
	var token runtime.StacklessCoroTimerTokenForTest
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			delay := int64(time.Hour)
			if round != 0 {
				delay = maxDuration
			}
			token = runtime.StartSleepStacklessCoroForTest(ctx, delay)
			if !runtime.CancelSleepStacklessCoroForTest(token) {
				t.Fatal("canceling live timer failed")
			}
			if runtime.CancelSleepStacklessCoroForTest(token) {
				t.Fatal("canceling stale timer succeeded")
			}
			return runtime.StacklessCoroActionWait
		case 1:
			if round == 0 {
				round = 1
				state = 0
				return runtime.StacklessCoroActionYield
			}
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroSleepCancelRace(t *testing.T) {
	const rounds = 100
	for range rounds {
		state := 0
		cancelDone := make(chan struct{})
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				id := runtime.StartSleepStacklessCoroForTest(ctx, 0)
				go func() {
					runtime.CancelSleepStacklessCoroForTest(id)
					close(cancelDone)
				}()
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		<-cancelDone
	}
}

func TestStacklessCoroTimerOwnerReuse(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	var first, second runtime.StacklessCoroTimerTokenForTest
	var firstOwner, secondOwner unsafe.Pointer
	state := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			first = runtime.StartSleepStacklessCoroForTest(ctx, int64(time.Hour))
			firstOwner = runtime.StacklessCoroTimerOwnerForTest(first)
			if !runtime.CancelSleepStacklessCoroForTest(first) {
				t.Fatal("canceling first reusable timer failed")
			}
			state = 1
			return runtime.StacklessCoroActionWait
		case 1:
			second = runtime.StartSleepStacklessCoroForTest(ctx, int64(time.Hour))
			secondOwner = runtime.StacklessCoroTimerOwnerForTest(second)
			runtime.ReadySleepStacklessCoroForTest(first)
			if runtime.CancelSleepStacklessCoroForTest(first) {
				t.Fatal("canceling stale reusable timer succeeded")
			}
			if !runtime.CancelSleepStacklessCoroForTest(second) {
				t.Fatal("late callback consumed reused timer")
			}
			state = 2
			return runtime.StacklessCoroActionWait
		case 2:
			state = 3
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected timer owner reuse state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if got, want := firstOwner == secondOwner, !race.Enabled; got != want {
		t.Fatalf("timer owner reused = %t, want %t", got, want)
	}
	if !runtime.CheckStacklessCoroTimerWrapForTest() {
		t.Fatal("timer generation wrap reused an old owner")
	}
}

func TestStacklessCoroOperationReuse(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	baselineOperations := runtime.StacklessCoroOperationCountForTest()

	cacheCount := func(t *testing.T, ctx unsafe.Pointer) {
		t.Helper()
		want := 1
		if race.Enabled {
			want = 0
		}
		if got := runtime.StacklessCoroFreeOperationCountForTest(ctx); got != want {
			t.Fatalf("cached operations = %d, want %d", got, want)
		}
	}

	t.Run("cross-kind", func(t *testing.T) {
		channel := make(chan int)
		startReceiver := make(chan struct{})
		received := make(chan int, 1)
		go func() {
			<-startReceiver
			received <- <-channel
		}()
		callRelease := make(chan struct{})
		value := 42
		var tokens [3]unsafe.Pointer
		var timer runtime.StacklessCoroTimerTokenForTest
		state := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				timer = runtime.StartSleepStacklessCoroForTest(ctx, int64(time.Hour))
				tokens[0] = runtime.StacklessCoroTimerOperationForTest(timer)
				if !runtime.CancelSleepStacklessCoroForTest(timer) {
					t.Fatal("canceling operation reuse timer failed")
				}
				state = 1
				return runtime.StacklessCoroActionWait
			case 1:
				cacheCount(t, ctx)
				runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
				tokens[1] = runtime.StacklessCoroOperationTokenForTest(ctx)
				runtime.ReadySleepStacklessCoroForTest(timer)
				close(startReceiver)
				state = 2
				return runtime.StacklessCoroActionWait
			case 2:
				cacheCount(t, ctx)
				runtime.CallReadStacklessCoroForTest(ctx, func() {
					<-callRelease
				})
				tokens[2] = runtime.StacklessCoroOperationTokenForTest(ctx)
				close(callRelease)
				state = 3
				return runtime.StacklessCoroActionWait
			case 3:
				cacheCount(t, ctx)
				state = 4
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected operation reuse state %d", state)
				return runtime.StacklessCoroActionInvalid
			}
		})
		if got := <-received; got != value {
			t.Fatalf("received value = %d, want %d", got, value)
		}
		for i, token := range tokens {
			if token == nil {
				t.Fatalf("operation token %d is nil", i)
			}
			for j := range i {
				if got, want := token == tokens[j], !race.Enabled; got != want {
					t.Fatalf("operation tokens %d and %d reused = %t, want %t",
						i, j, got, want)
				}
			}
		}
	})

	t.Run("bounded", func(t *testing.T) {
		const operations = runtime.StacklessCoroOperationCacheSize + 1
		timers := make([]runtime.StacklessCoroTimerTokenForTest, operations)
		var started, completed atomic.Int32
		parentState := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch parentState {
			case 0:
				for i := range operations {
					index := i
					childState := 0
					runtime.SpawnStacklessCoroForTest(ctx, func(childCtx unsafe.Pointer) uint8 {
						switch childState {
						case 0:
							timers[index] = runtime.StartSleepStacklessCoroForTest(
								childCtx, int64(time.Hour))
							started.Add(1)
							childState = 1
							return runtime.StacklessCoroActionWait
						case 1:
							completed.Add(1)
							childState = 2
							return runtime.StacklessCoroActionComplete
						default:
							t.Fatalf("unexpected operation child state %d", childState)
							return runtime.StacklessCoroActionInvalid
						}
					})
				}
				parentState = 1
				return runtime.StacklessCoroActionYield
			case 1:
				if started.Load() != operations {
					return runtime.StacklessCoroActionYield
				}
				for i, timer := range timers {
					if !runtime.CancelSleepStacklessCoroForTest(timer) {
						t.Fatalf("canceling bounded operation %d failed", i)
					}
				}
				want := runtime.StacklessCoroOperationCacheSize
				if race.Enabled {
					want = 0
				}
				if got := runtime.StacklessCoroFreeOperationCountForTest(ctx); got != want {
					t.Fatalf("cached operations = %d, want %d", got, want)
				}
				parentState = 2
				return runtime.StacklessCoroActionYield
			case 2:
				if completed.Load() != operations {
					return runtime.StacklessCoroActionYield
				}
				parentState = 3
				return runtime.StacklessCoroActionComplete
			default:
				t.Fatalf("unexpected operation cache parent state %d", parentState)
				return runtime.StacklessCoroActionInvalid
			}
		})
	})

	if operations := runtime.StacklessCoroOperationCountForTest(); operations != baselineOperations {
		t.Fatalf("operation count = %d, want %d", operations, baselineOperations)
	}
}

func TestStacklessCoroOperationRegistry(t *testing.T) {
	if !runtime.CheckStacklessCoroOperationRegistryForTest() {
		t.Fatal("operation registry did not reject stale or duplicate completion")
	}
}

func TestStacklessCoroEarlyReady(t *testing.T) {
	if !runtime.CheckEarlyReadyStacklessCoroForTest() {
		t.Fatal("operation completion was published before resume returned")
	}
}

func TestStacklessCoroCallRead(t *testing.T) {
	var state int
	called := false
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.CallReadStacklessCoroForTest(ctx, func() {
				called = true
			})
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if !called {
		t.Fatal("read call did not run")
	}
}

func TestStacklessCoroBlockingProgress(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	const rounds = 2*runtime.StacklessCoroWarmExecutorCount + 1
	var rescued atomic.Bool
	rescue := time.AfterFunc(5*time.Second, func() {
		rescued.Store(true)
		syscall.Write(fds[1], bytes.Repeat([]byte{'r'}, rounds))
	})
	defer rescue.Stop()

	var writes atomic.Int32
	var writeFailed atomic.Bool
	sibling := func(unsafe.Pointer) uint8 {
		if _, err := syscall.Write(fds[1], []byte{'p'}); err != nil {
			writeFailed.Store(true)
		}
		writes.Add(1)
		return runtime.StacklessCoroActionComplete
	}
	var state int
	buffer := make([]byte, 1)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			for range rounds {
				runtime.SpawnStacklessCoroForTest(ctx, sibling)
				if n := runtime.BlockingReadStacklessCoroForTest(ctx, fds[0], buffer); n != 1 {
					t.Fatalf("blocking read = %d, want 1", n)
				}
				if got := string(buffer); got != "p" {
					t.Fatalf("blocking read = %q, want %q", got, "p")
				}
			}
			state = 1
			return runtime.StacklessCoroActionYield
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if writeFailed.Load() {
		t.Fatal("replacement executor write failed")
	}
	if rescued.Load() {
		t.Fatal("blocking foreign call stopped stackless sibling progress")
	}
	if got := writes.Load(); got != rounds {
		t.Fatalf("replacement writes = %d, want %d", got, rounds)
	}
}

func TestStacklessCoroFileReadProgress(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], false); err != nil {
		t.Fatal(err)
	}

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	progress := make(chan struct{})
	baselineOperations := runtime.StacklessCoroOperationCountForTest()
	writeDone := stacklessCoroProgressWrite(fds[1], progress, "file", runtime.GC)

	var state, n int
	var errno uintptr
	buffer := make([]byte, 4)
	sibling := func(unsafe.Pointer) uint8 {
		close(progress)
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SpawnStacklessCoroForTest(ctx, sibling)
			runtime.FileReadStacklessCoroForTest(ctx, fds[0], buffer, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	write := <-writeDone
	if write.err != nil {
		t.Fatal(write.err)
	}
	if write.rescued {
		t.Fatal("file read blocked sibling scheduling")
	}
	if write.operations != baselineOperations {
		t.Fatalf("registered operations during direct file read = %d, want %d",
			write.operations, baselineOperations)
	}
	if n != 4 || errno != 0 || string(buffer) != "file" {
		t.Fatalf("read = (%d, %d, %q), want (4, 0, %q)",
			n, errno, buffer, "file")
	}
}

func TestStacklessCoroFileReadError(t *testing.T) {
	for _, test := range []struct {
		name string
		read func(unsafe.Pointer, int, []byte, *int, *uintptr)
	}{
		{"explicit", runtime.FileReadStacklessCoroForTest},
		{"public", runtime.PublicFileReadStacklessCoroForTest},
	} {
		t.Run(test.name, func(t *testing.T) {
			cgoCalls := runtime.NumCgoCall()
			var state, n int
			var errno uintptr
			buffer := make([]byte, 1)
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					state = 1
					test.read(ctx, -1, buffer, &n, &errno)
					return runtime.StacklessCoroActionWait
				case 1:
					state = 2
					return runtime.StacklessCoroActionComplete
				default:
					t.Fatalf("unexpected state %d", state)
					return runtime.StacklessCoroActionInvalid
				}
			})
			if n != -1 || errno == 0 {
				t.Fatalf("read from invalid descriptor = (%d, %d), want (-1, errno)",
					n, errno)
			}
			if got := runtime.NumCgoCall(); got != cgoCalls {
				t.Fatalf("NumCgoCall after file read = %d, want %d", got, cgoCalls)
			}
		})
	}
}

func TestStacklessCoroFileReadEmpty(t *testing.T) {
	state := 0
	n := -1
	errno := ^uintptr(0)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.FileReadStacklessCoroForTest(ctx, -1, nil, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if n != 0 || errno != 0 {
		t.Fatalf("empty read = (%d, %d), want (0, 0)", n, errno)
	}
}

func TestStacklessCoroSocketReadProgress(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	progress := make(chan struct{})
	baselineOperations := runtime.StacklessCoroOperationCountForTest()
	writeDone := stacklessCoroProgressWrite(fds[1], progress, "poll", runtime.GC)

	var state, n int
	var errno uintptr
	buffer := make([]byte, 4)
	sibling := func(unsafe.Pointer) uint8 {
		close(progress)
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SpawnStacklessCoroForTest(ctx, sibling)
			runtime.SocketReadStacklessCoroForTest(ctx, fds[0], buffer, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	write := <-writeDone
	if write.err != nil {
		t.Fatal(write.err)
	}
	if write.rescued {
		t.Fatal("socket read blocked sibling scheduling")
	}
	if write.operations != baselineOperations+1 {
		t.Fatalf("registered operations during socket read = %d, want %d",
			write.operations, baselineOperations+1)
	}
	if n != 4 || errno != 0 || string(buffer) != "poll" {
		t.Fatalf("read = (%d, %d, %q), want (4, 0, %q)",
			n, errno, buffer, "poll")
	}
	if operations := runtime.StacklessCoroOperationCountForTest(); operations != baselineOperations {
		t.Fatalf("registered operations after socket read = %d, want %d",
			operations, baselineOperations)
	}
}

func TestStacklessCoroSocketReadEOF(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Close(fds[1]); err != nil {
		t.Fatal(err)
	}

	var state, n int
	var errno uintptr
	buffer := make([]byte, 1)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SocketReadStacklessCoroForTest(ctx, fds[0], buffer, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if n != 0 || errno != 0 {
		t.Fatalf("read at EOF = (%d, %d), want (0, 0)", n, errno)
	}
}

func TestStacklessCoroSocketReadError(t *testing.T) {
	var state, n int
	var errno uintptr
	buffer := make([]byte, 1)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SocketReadStacklessCoroForTest(ctx, -1, buffer, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	if n != -1 || errno == 0 {
		t.Fatalf("read from invalid socket = (%d, %d), want (-1, errno)",
			n, errno)
	}
}

func TestStacklessCoroSocketReadWriteOnly(t *testing.T) {
	fds := stacklessCoroPipe(t)
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	var state, n int
	var errno uintptr
	buffer := make([]byte, 1)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SocketReadStacklessCoroForTest(ctx, fds[1], buffer, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	if n != -1 || errno == 0 {
		t.Fatalf("read from write-only descriptor = (%d, %d), want (-1, errno)",
			n, errno)
	}
}

func TestStacklessCoroSocketReadExpired(t *testing.T) {
	fds := stacklessCoroPipe(t)
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	var state, n int
	var errno uintptr
	buffer := make([]byte, 1)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SocketReadExpiredStacklessCoroForTest(ctx,
				fds[0], buffer, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	if n != -1 || errno != runtime.StacklessCoroPollErrTimeout {
		t.Fatalf("expired socket read = (%d, %d), want (-1, %d)",
			n, errno, runtime.StacklessCoroPollErrTimeout)
	}
}

func TestStacklessCoroSocketReadEmpty(t *testing.T) {
	state := 0
	n := -1
	errno := ^uintptr(0)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SocketReadStacklessCoroForTest(ctx, -1, nil, &n, &errno)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	if n != 0 || errno != 0 {
		t.Fatalf("empty socket read = (%d, %d), want (0, 0)", n, errno)
	}
	nilState := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if nilState == 0 {
			nilState = 1
			runtime.SocketReadStacklessCoroForTest(ctx, -1, nil, nil, nil)
			return runtime.StacklessCoroActionWait
		}
		return runtime.StacklessCoroActionComplete
	})
	borrowedState := 0
	borrowedN := -1
	borrowedErrno := ^uintptr(0)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if borrowedState == 0 {
			borrowedState = 1
			runtime.SocketReadWithPollDescStacklessCoroForTest(ctx,
				nil, -1, nil, &borrowedN, &borrowedErrno)
			return runtime.StacklessCoroActionWait
		}
		return runtime.StacklessCoroActionComplete
	})
	if borrowedN != 0 || borrowedErrno != 0 {
		t.Fatalf("empty borrowed socket read = (%d, %d), want (0, 0)",
			borrowedN, borrowedErrno)
	}
}

func TestStacklessCoroSocketReadBorrowedPollDesc(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}

	descriptor, errno := runtime.OpenStacklessCoroPollDescForTest(fds[0])
	if errno != 0 {
		t.Fatalf("open poll descriptor: errno %d", errno)
	}
	defer func() {
		if descriptor != nil {
			runtime.UnblockStacklessCoroPollDescForTest(descriptor)
			runtime.CloseStacklessCoroPollDescForTest(descriptor)
		}
	}()
	if _, err := syscall.Write(fds[1], []byte("ok")); err != nil {
		t.Fatal(err)
	}

	var state, n int
	var readErrno uintptr
	buffer := make([]byte, 2)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0, 1:
			index := state
			state++
			runtime.SocketReadWithPollDescStacklessCoroForTest(ctx,
				descriptor, fds[0], buffer[index:index+1], &n, &readErrno)
			return runtime.StacklessCoroActionWait
		case 2:
			state = 3
			return runtime.StacklessCoroActionComplete
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
	if state != 3 || n != 1 || readErrno != 0 || string(buffer) != "ok" {
		t.Fatalf("borrowed reads = (state %d, %d, %d, %q), want (3, 1, 0, %q)",
			state, n, readErrno, buffer, "ok")
	}
}

func TestStacklessCoroSocketReadBorrowedPollDescDeadline(t *testing.T) {
	testStacklessCoroSocketReadBorrowedPollDescWake(t, true)
}

func TestStacklessCoroSocketReadBorrowedPollDescClose(t *testing.T) {
	testStacklessCoroSocketReadBorrowedPollDescWake(t, false)
}

func testStacklessCoroSocketReadBorrowedPollDescWake(t *testing.T, deadline bool) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}

	descriptor, errno := runtime.OpenStacklessCoroPollDescForTest(fds[0])
	if errno != 0 {
		t.Fatalf("open poll descriptor: errno %d", errno)
	}
	baseline := runtime.StacklessCoroPollWaitCountForTest()
	var state, n int
	var readErrno uintptr
	buffer := make([]byte, 1)
	done := make(chan struct{})
	go func() {
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				state = 1
				runtime.SocketReadWithPollDescStacklessCoroForTest(ctx,
					descriptor, fds[0], buffer, &n, &readErrno)
				return runtime.StacklessCoroActionWait
			case 1:
				state = 2
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
		close(done)
	}()

	waiting := stacklessCoroWaitForPollCount(baseline + 1)
	if deadline {
		runtime.ExpireReadStacklessCoroPollDescForTest(descriptor)
	} else {
		runtime.UnblockStacklessCoroPollDescForTest(descriptor)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if deadline {
			runtime.UnblockStacklessCoroPollDescForTest(descriptor)
		}
		select {
		case <-done:
			runtime.CloseStacklessCoroPollDescForTest(descriptor)
			t.Fatal("borrowed poll descriptor did not wake the coroutine read")
		case <-time.After(5 * time.Second):
			t.Fatal("borrowed poll descriptor remained blocked after fallback close")
		}
	}
	if deadline {
		runtime.UnblockStacklessCoroPollDescForTest(descriptor)
	}
	runtime.CloseStacklessCoroPollDescForTest(descriptor)
	if !waiting {
		t.Fatal("coroutine read did not arm its borrowed poll descriptor")
	}
	wantErrno := runtime.StacklessCoroPollErrorStatusForTest(
		runtime.StacklessCoroPollErrClosing)
	if deadline {
		wantErrno = runtime.StacklessCoroPollErrorStatusForTest(
			runtime.StacklessCoroPollErrTimeout)
	}
	if state != 2 || n != -1 || readErrno != wantErrno {
		t.Fatalf("borrowed wake = (state %d, %d, %d), want (2, -1, %d)",
			state, n, readErrno, wantErrno)
	}
}

func TestStacklessCoroReadLength(t *testing.T) {
	if got := runtime.StacklessCoroReadLengthForTest(1); got != 1 {
		t.Fatalf("read length = %d, want 1", got)
	}
	if ^uint(0)>>63 != 0 {
		length := int64(1<<31 - 1)
		if got := runtime.StacklessCoroReadLengthForTest(int(length + 1)); got != int32(length) {
			t.Fatalf("large read length = %d, want %d", got, length)
		}
	}
}

func TestStacklessCoroPollArm(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	waiting, errno, tokenMatches, delta :=
		runtime.StacklessCoroPollArmForTest(fds[0], false, false)
	if !waiting || errno != 0 || !tokenMatches || delta != -1 {
		t.Fatalf("poll arm = (%t, %d, %t, %d), want (true, 0, true, -1)",
			waiting, errno, tokenMatches, delta)
	}
	waiting, errno, tokenMatches, delta =
		runtime.StacklessCoroPollArmForTest(fds[0], true, false)
	if waiting || errno != 0 || tokenMatches || delta != 0 {
		t.Fatalf("ready poll arm = (%t, %d, %t, %d), want (false, 0, false, 0)",
			waiting, errno, tokenMatches, delta)
	}
	waiting, errno, tokenMatches, delta =
		runtime.StacklessCoroPollArmForTest(fds[0], false, true)
	if waiting || errno != runtime.StacklessCoroPollErrTimeout ||
		tokenMatches || delta != 0 {
		t.Fatalf("expired poll arm = (%t, %d, %t, %d), want (false, %d, false, 0)",
			waiting, errno, tokenMatches, delta,
			runtime.StacklessCoroPollErrTimeout)
	}
}

func TestStacklessCoroPollIdleRetry(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}

	skipped, claimed, rearmed :=
		runtime.StacklessCoroPollIdleRetryForTest(fds[0])
	if !skipped || !claimed || !rearmed {
		t.Fatalf("idle poll retry = (%t, %t, %t), want (true, true, true)",
			skipped, claimed, rearmed)
	}
}

func TestStacklessCoroOrdinaryNetpollStates(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	runtime.StacklessCoroOrdinaryNetpollStatesForTest(fds[0])
}

func TestStacklessCoroSocketReadConcurrent(t *testing.T) {
	const readers = 32
	type result struct {
		index int
		n     int
		errno uintptr
		value byte
	}

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	baselineOperations := runtime.StacklessCoroOperationCountForTest()
	baselineWaiters := runtime.StacklessCoroNetpollWaiterCountForTest()
	fds := make([][2]int, readers)
	for i := range fds {
		pair, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		fds[i] = pair
		if err := syscall.SetNonblock(pair[0], true); err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(pair[0])
		defer syscall.Close(pair[1])
	}

	done := make(chan result, readers)
	for i := range fds {
		go func(index int) {
			state := 0
			n := -1
			errno := ^uintptr(0)
			buffer := make([]byte, 1)
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				switch state {
				case 0:
					state = 1
					runtime.SocketReadStacklessCoroForTest(ctx,
						fds[index][0], buffer, &n, &errno)
					return runtime.StacklessCoroActionWait
				case 1:
					state = 2
					return runtime.StacklessCoroActionComplete
				default:
					return runtime.StacklessCoroActionInvalid
				}
			})
			done <- result{index: index, n: n, errno: errno, value: buffer[0]}
		}(i)
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.StacklessCoroPollWaitCountForTest() != readers {
		if time.Now().After(deadline) {
			for i := range fds {
				syscall.Write(fds[i][1], []byte{byte(i)})
			}
			t.Fatalf("logical poll waiters = %d, want %d",
				runtime.StacklessCoroPollWaitCountForTest(), readers)
		}
		runtime.Gosched()
	}
	if operations := runtime.StacklessCoroOperationCountForTest(); operations != baselineOperations+readers {
		t.Fatalf("registered socket operations = %d, want %d",
			operations, baselineOperations+readers)
	}
	if waiters := runtime.StacklessCoroNetpollWaiterCountForTest(); waiters != baselineWaiters+readers {
		t.Fatalf("netpoll waiters = %d, want %d", waiters,
			baselineWaiters+readers)
	}

	runtime.GC()
	for i := range fds {
		if _, err := syscall.Write(fds[i][1], []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	timeout := time.After(5 * time.Second)
	for range readers {
		select {
		case got := <-done:
			if got.n != 1 || got.errno != 0 || got.value != byte(got.index) {
				t.Fatalf("socket %d read = (%d, %d, %d), want (1, 0, %d)",
					got.index, got.n, got.errno, got.value, byte(got.index))
			}
		case <-timeout:
			t.Fatal("timed out waiting for logical poll completions")
		}
	}
	if waiters := runtime.StacklessCoroPollWaitCountForTest(); waiters != 0 {
		t.Fatalf("logical poll waiters after completion = %d, want 0", waiters)
	}
	if operations := runtime.StacklessCoroOperationCountForTest(); operations != baselineOperations {
		t.Fatalf("registered operations after socket completion = %d, want %d",
			operations, baselineOperations)
	}
	if waiters := runtime.StacklessCoroNetpollWaiterCountForTest(); waiters != baselineWaiters {
		t.Fatalf("netpoll waiters after socket completion = %d, want %d",
			waiters, baselineWaiters)
	}
}

func TestStacklessCoroAsyncRead(t *testing.T) {
	fds := stacklessCoroPipe(t)
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	var state int
	var result uint64
	var errno uintptr
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			id := runtime.AsyncReadStacklessCoroForTest(ctx, fds[0], &result, &errno)
			packet := [2]uint64{id, 42}
			data := unsafe.Slice((*byte)(unsafe.Pointer(&packet[0])), int(unsafe.Sizeof(packet)))
			if _, err := syscall.Write(fds[1], data); err != nil {
				t.Fatal(err)
			}
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if result != 42 || errno != 0 {
		t.Fatalf("async result = (%d, %d), want (42, 0)", result, errno)
	}
}

func TestStacklessCoroAsyncBadCompletion(t *testing.T) {
	fds := stacklessCoroPipe(t)
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	var state int
	var result uint64
	var errno uintptr
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			id := runtime.AsyncReadStacklessCoroForTest(ctx, fds[0], &result, &errno)
			packet := [2]uint64{id + 1, 42}
			data := unsafe.Slice((*byte)(unsafe.Pointer(&packet[0])), int(unsafe.Sizeof(packet)))
			if _, err := syscall.Write(fds[1], data); err != nil {
				t.Fatal(err)
			}
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if result != 0 || errno != ^uintptr(0) {
		t.Fatalf("bad async completion = (%d, %d), want (0, %d)",
			result, errno, ^uintptr(0))
	}
}

func TestStacklessCoroAsyncSubmitError(t *testing.T) {
	var state int
	var result uint64
	var errno uintptr
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.FailAsyncStacklessCoroForTest(ctx, &result, &errno, 42)
			return runtime.StacklessCoroActionWait
		case 1:
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if result != 0 || errno != 42 {
		t.Fatalf("failed async submit = (%d, %d), want (0, 42)", result, errno)
	}
}

func stacklessCoroPipe(t *testing.T) [2]int {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		t.Fatal(err)
	}
	return fds
}

func stacklessCoroWaitForPollCount(want int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for runtime.StacklessCoroPollWaitCountForTest() != want {
		if time.Now().After(deadline) {
			return false
		}
		runtime.Gosched()
	}
	return true
}

type stacklessCoroWriteResult struct {
	err        error
	rescued    bool
	operations int
}

func stacklessCoroProgressWrite(fd int, progress <-chan struct{}, data string, beforeWrite func()) <-chan stacklessCoroWriteResult {
	done := make(chan stacklessCoroWriteResult, 1)
	go func() {
		rescued := false
		select {
		case <-progress:
		case <-time.After(5 * time.Second):
			// Keep a scheduling regression from hanging the runtime test.
			rescued = true
		}
		if beforeWrite != nil {
			beforeWrite()
		}
		operations := runtime.StacklessCoroOperationCountForTest()
		_, err := syscall.Write(fd, []byte(data))
		done <- stacklessCoroWriteResult{
			err: err, rescued: rescued, operations: operations,
		}
	}()
	return done
}
