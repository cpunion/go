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
	if got, want := runtime.StacklessCoroTaskSizeForTest(), 6*ptrSize; got != want {
		t.Fatalf("stackless coroutine task size = %d, want %d", got, want)
	}
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
	value := -1
	chosen := -1
	received := false
	cases := runtime.NewStacklessCoroSelectCasesForTest(
		[]any{first, second},
		[]unsafe.Pointer{unsafe.Pointer(&value), unsafe.Pointer(&value)}, 0)
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
			if chosen != 1 || !received || value != 61 {
				t.Fatalf("blocked select = (%d, %t, %d), want (1, true, 61)",
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
	var id uint64
	var progressed, canceled bool
	progress := func(unsafe.Pointer) uint8 {
		progressed = true
		canceled = runtime.CancelSleepStacklessCoroForTest(id)
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SpawnStacklessCoroForTest(ctx, progress)
			id = runtime.StartSleepStacklessCoroForTest(ctx, int64(delay))
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
	var state int
	var id uint64
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			id = runtime.StartSleepStacklessCoroForTest(ctx, int64(time.Hour))
			if !runtime.CancelSleepStacklessCoroForTest(id) {
				t.Fatal("canceling live timer failed")
			}
			if runtime.CancelSleepStacklessCoroForTest(id) {
				t.Fatal("canceling stale timer succeeded")
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
		state := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				id := runtime.StartSleepStacklessCoroForTest(ctx, int64(time.Hour))
				tokens[0] = runtime.StacklessCoroOperationTokenForTest(ctx)
				if !runtime.CancelSleepStacklessCoroForTest(id) {
					t.Fatal("canceling operation reuse timer failed")
				}
				state = 1
				return runtime.StacklessCoroActionWait
			case 1:
				cacheCount(t, ctx)
				runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
				tokens[1] = runtime.StacklessCoroOperationTokenForTest(ctx)
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
		ids := make([]uint64, operations)
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
							ids[index] = runtime.StartSleepStacklessCoroForTest(
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
				for _, id := range ids {
					if !runtime.CancelSleepStacklessCoroForTest(id) {
						t.Fatalf("canceling bounded operation %d failed", id)
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
	writeDone := stacklessCoroProgressWrite(fds[1], progress, "file")

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
	if n != 4 || errno != 0 || string(buffer) != "file" {
		t.Fatalf("read = (%d, %d, %q), want (4, 0, %q)",
			n, errno, buffer, "file")
	}
}

func TestStacklessCoroFileReadError(t *testing.T) {
	var state, n int
	var errno uintptr
	buffer := make([]byte, 1)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.FileReadStacklessCoroForTest(ctx, -1, buffer, &n, &errno)
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
	writeDone := stacklessCoroProgressWrite(fds[1], progress, "poll")

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
	if n != 4 || errno != 0 || string(buffer) != "poll" {
		t.Fatalf("read = (%d, %d, %q), want (4, 0, %q)",
			n, errno, buffer, "poll")
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

type stacklessCoroWriteResult struct {
	err     error
	rescued bool
}

func stacklessCoroProgressWrite(fd int, progress <-chan struct{}, data string) <-chan stacklessCoroWriteResult {
	done := make(chan stacklessCoroWriteResult, 1)
	go func() {
		rescued := false
		select {
		case <-progress:
		case <-time.After(5 * time.Second):
			// Keep a scheduling regression from hanging the runtime test.
			rescued = true
		}
		_, err := syscall.Write(fd, []byte(data))
		done <- stacklessCoroWriteResult{err: err, rescued: rescued}
	}()
	return done
}
