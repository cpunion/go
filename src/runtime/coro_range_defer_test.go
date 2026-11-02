// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/testenv"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// Exercise the anchor under real structured task transitions. These are
// runtime ownership tests; compiler admission and native stack gates have
// separate contracts.
func TestStacklessCoroRangeDeferStructured(t *testing.T) {
	for _, test := range []struct {
		name        string
		initial     uint8
		replacement bool
		want        any
		terminal    uint8
	}{
		{"return", runtime.StacklessCoroActionComplete, false, nil, runtime.StacklessCoroActionComplete},
		{"panic", runtime.StacklessCoroActionPanic, false, "original", runtime.StacklessCoroActionComplete},
		{"Goexit", runtime.StacklessCoroActionGoexit, false, nil, runtime.StacklessCoroActionGoexit},
		{"replacement-panic", runtime.StacklessCoroActionPanic, true, "replacement", runtime.StacklessCoroActionComplete},
		{"panic-during-Goexit", runtime.StacklessCoroActionGoexit, true, "replacement", runtime.StacklessCoroActionGoexit},
	} {
		t.Run(test.name, func(t *testing.T) {
			type result struct {
				recovered any
				terminal  uint8
				order     []int
				distinct  bool
				released  bool
			}
			results := make(chan result, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				state := 0
				var anchor any
				var token unsafe.Pointer
				var got result
				child := func(ctx unsafe.Pointer) uint8 {
					got.distinct = runtime.DeferTokenStacklessCoroForTest(ctx) != token &&
						runtime.OwnerStacklessCoroRangeDeferForTest(anchor) == token
					// Registration runs while the enclosing task is Waiting.
					runtime.PushStacklessCoroRangeDeferForTest(anchor, func() {
						got.order = append(got.order, 1)
						got.recovered = runtime.DeferRecoverStacklessCoroForTest(token)
					})
					runtime.PushStacklessCoroRangeDeferForTest(anchor, func() {
						got.order = append(got.order, 2)
						if test.replacement {
							runtime.DeferPanicStacklessCoroForTest(token, "replacement")
						}
					})
					return runtime.StacklessCoroActionComplete
				}
				runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
					if state == 0 {
						state = 1
						anchor = runtime.NewStacklessCoroRangeDeferAtForTest(ctx)
						token = runtime.OwnerStacklessCoroRangeDeferForTest(anchor)
						runtime.AwaitStacklessCoroForTest(ctx, child)
						return runtime.StacklessCoroActionWait
					}
					switch test.initial {
					case runtime.StacklessCoroActionPanic:
						runtime.PanicStacklessCoroForTest(ctx, "original")
					case runtime.StacklessCoroActionGoexit:
						runtime.GoexitStacklessCoroForTest(ctx)
					}
					for {
						fn, ok := runtime.NextStacklessCoroRangeDeferForTest(ctx, anchor)
						if !ok {
							break
						}
						fn()
					}
					got.terminal = runtime.TerminalActionStacklessCoroForTest(ctx)
					if got.terminal == runtime.StacklessCoroActionInvalid {
						// No pending terminal action follows the compiler's
						// normal-completion edge after cleanup.
						got.terminal = runtime.StacklessCoroActionComplete
					}
					got.released = runtime.StacklessCoroRangeDeferReleasedForTest(anchor)
					results <- got
					return got.terminal
				})
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("structured range cleanup did not finish")
			}
			select {
			case got := <-results:
				if got.recovered != test.want || got.terminal != test.terminal ||
					!slices.Equal(got.order, []int{2, 1}) || !got.distinct || !got.released {
					t.Fatalf("structured cleanup = %+v, want recovered=%v terminal=%d", got, test.want, test.terminal)
				}
			default:
				t.Fatal("structured range cleanup lost its outcome")
			}
		})
	}
}

func TestStacklessCoroRangeDeferRemaining(t *testing.T) {
	a, ctx := runtime.NewStacklessCoroRangeDeferForTest()
	first := runtime.OwnerStacklessCoroRangeDeferForTest(a)
	if first == nil || runtime.OwnerStacklessCoroRangeDeferForTest(a) != first {
		t.Fatal("unstable outer owner")
	}
	var order []int
	runtime.PushStacklessCoroRangeDeferForTest(a, func() { order = append(order, 1) })
	runtime.PushStacklessCoroRangeDeferForTest(a, nil)
	runtime.PushStacklessCoroRangeDeferForTest(a, func() { panic("replacement") })
	panics := 0
	for {
		fn, ok := runtime.NextStacklessCoroRangeDeferForTest(ctx, a)
		if !ok {
			break
		}
		func() {
			defer func() {
				if recover() != nil {
					panics++
				}
			}()
			fn()
		}()
		runtime.GC()
	}
	if panics != 2 || !slices.Equal(order, []int{1}) {
		t.Fatalf("remaining cleanup lost: panics=%d order=%v", panics, order)
	}
	if !runtime.StacklessCoroRangeDeferReleasedForTest(a) {
		t.Fatal("drained escaped anchor retained its owner or callback state")
	}
}

func TestStacklessCoroRangeDeferConcurrent(t *testing.T) {
	a, ctx := runtime.NewStacklessCoroRangeDeferForTest()
	const workers, each = 16, 32
	var wg sync.WaitGroup
	seen := make([]int, workers*each)
	for worker := range workers {
		wg.Go(func() {
			for i := range each {
				value := worker*each + i
				runtime.PushStacklessCoroRangeDeferForTest(a, func() { seen[value]++ })
			}
		})
	}
	wg.Wait()
	runtime.GC()
	count := 0
	for {
		fn, ok := runtime.NextStacklessCoroRangeDeferForTest(ctx, a)
		if !ok {
			break
		}
		fn()
		count++
	}
	if count != len(seen) {
		t.Fatalf("registrations=%d want=%d", count, len(seen))
	}
	for i, n := range seen {
		if n != 1 {
			t.Fatalf("registration%d invoked%d times", i, n)
		}
	}
	if !runtime.StacklessCoroRangeDeferReleasedForTest(a) {
		t.Fatal("concurrent anchor retained state")
	}
}

func TestStacklessCoroRangeDeferRoots(t *testing.T) {
	a, ctx := runtime.NewStacklessCoroRangeDeferForTest()
	type object struct {
		child *object
		value int
		pad   [4096]byte
	}
	got := 0
	register := func(value int) {
		p := &object{child: &object{value: value}}
		runtime.PushStacklessCoroRangeDeferForTest(a, func() { got = p.child.value })
	}
	register(73)
	runtime.GC()
	fn, ok := runtime.NextStacklessCoroRangeDeferForTest(ctx, a)
	if !ok || fn == nil {
		t.Fatal("lost rooted callback")
	}
	runtime.GC()
	fn()
	if got != 73 || !runtime.StacklessCoroRangeDeferReleasedForTest(a) {
		t.Fatal("callback root or owner release lost")
	}
	if fn, ok := runtime.NextStacklessCoroRangeDeferForTest(ctx, a); ok || fn != nil {
		t.Fatal("completed anchor restarted")
	}
	empty, emptyCtx := runtime.NewStacklessCoroRangeDeferForTest()
	if fn, ok := runtime.NextStacklessCoroRangeDeferForTest(emptyCtx, empty); ok || fn != nil || !runtime.StacklessCoroRangeDeferReleasedForTest(empty) {
		t.Fatal("empty anchor retained owner")
	}
}

func TestStacklessCoroRangeDeferInvalid(t *testing.T) {
	if mode := os.Getenv("GO_CORO_RANGE_DEFER_INVALID"); mode != "" {
		runtime.StacklessCoroRangeDeferInvalidForTest(mode)
		t.Fatal("invalid owner/registration accepted")
	}
	for _, mode := range []string{"late-push", "late-owner", "wrong-task", "wrong-frame", "wrong-resume", "bad-anchor"} {
		t.Run(mode, func(t *testing.T) {
			cmd := testenv.Command(t, os.Args[0], "-test.run=^TestStacklessCoroRangeDeferInvalid$")
			cmd.Env = append(cmd.Environ(), "GO_CORO_RANGE_DEFER_INVALID="+mode)
			out, err := cmd.CombinedOutput()
			want := "fatal error: runtime: invalid stackless coroutine range defer owner"
			if strings.HasPrefix(mode, "late-") {
				want = "fatal error: defer after range func returned"
			} else if mode == "bad-anchor" {
				want = "fatal error: runtime: invalid stackless coroutine range defer anchor"
			}
			if err == nil || !strings.Contains(string(out), want) {
				t.Fatalf("invalid case did not fail with %q: %v\n%s", want, err, out)
			}
		})
	}
}
