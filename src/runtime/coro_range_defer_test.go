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
)

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
