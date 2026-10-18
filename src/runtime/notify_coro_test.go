// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"context"
	"internal/testenv"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroNotifyList(t *testing.T) {
	if mode := os.Getenv("GO_TEST_CORO_NOTIFY"); mode != "" {
		switch mode {
		case "fast":
			testStacklessCoroNotifyFast(t)
		case "signal", "broadcast", "profile", "wrap", "missing":
			testStacklessCoroNotifyMixed(t, mode)
		case "siblings":
			testStacklessCoroNotifySiblings(t)
		case "race":
			testStacklessCoroNotifyRace(t)
		default:
			t.Fatalf("unknown notification case %q", mode)
		}
		return
	}
	// Child processes isolate scheduler configurations, but their coverage
	// counters do not belong to this test process's profile. Exercise the
	// valid state transitions here as well.
	t.Run("direct", func(t *testing.T) {
		testStacklessCoroNotifyFast(t)
		for _, mode := range []string{"signal", "broadcast", "profile", "wrap", "missing"} {
			testStacklessCoroNotifyMixed(t, mode)
		}
		testStacklessCoroNotifySiblings(t)
		testStacklessCoroNotifyRace(t)
	})
	for _, procs := range []int{1, 4} {
		for _, mode := range []string{"fast", "signal", "broadcast", "profile", "wrap", "missing", "siblings", "race"} {
			t.Run(mode+"/P"+strconv.Itoa(procs), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := testenv.CommandContext(t, ctx, os.Args[0], "-test.run=^TestStacklessCoroNotifyList$")
				cmd.Env = append(cmd.Environ(), "GO_TEST_CORO_NOTIFY="+mode,
					"GOMAXPROCS="+strconv.Itoa(procs))
				if data, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("notification case: %v\n%s", err, data)
				}
			})
		}
	}
}

func testStacklessCoroNotifyFast(t *testing.T) {
	var list runtime.NotifyListStacklessCoroForTest
	ctx := runtime.NewStacklessCoroSemaphoreContextForTest()
	var state unsafe.Pointer
	for _, broadcast := range []bool{false, true} {
		if allocs := testing.AllocsPerRun(100, func() {
			ticket := list.Add()
			if broadcast {
				list.Broadcast()
			} else {
				list.Signal()
			}
			if list.WaitCoro(ctx, ticket, &state) || state != nil {
				t.Fatal("pre-notified ticket retained a waiter")
			}
		}); allocs != 0 {
			t.Fatalf("pre-notified ticket allocated %v times", allocs)
		}
	}
	list.Signal()
	list.Broadcast()
	for _, broadcast := range []bool{false, true} {
		ticket := list.Add()
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if !list.NotifyBeforeQueue(ctx, ticket, broadcast) {
				panic("notification before insertion retained waiting state")
			}
			return runtime.StacklessCoroActionComplete
		})
	}
}

func waitStacklessCoroNotifyQueue(list *runtime.NotifyListStacklessCoroForTest, ordinary, logical int) {
	for {
		o, l := list.Waiters()
		if o == ordinary && l == logical {
			return
		}
		runtime.Gosched()
	}
}

func testStacklessCoroNotifyMixed(t *testing.T, mode string) {
	var list runtime.NotifyListStacklessCoroForTest
	if mode == "wrap" {
		list.Reset(^uint32(0) - 1)
	}
	if mode == "profile" {
		runtime.SetBlockProfileRate(1)
		defer runtime.SetBlockProfileRate(0)
	}
	first, second, third := list.Add(), list.Add(), list.Add()
	done := make(chan uint32, 3)
	go func() { list.Wait(first); done <- first }()
	waitStacklessCoroNotifyQueue(&list, 1, 0)
	if mode == "missing" {
		// Consume the middle ticket before its task starts. The last waiter
		// must not consume that notification or prevent the middle returning.
		list.Signal()
		if got := <-done; got != first {
			t.Fatal("wrong first ticket")
		}
		list.Signal()
	}
	logical := func(ticket uint32) {
		var state unsafe.Pointer
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if list.WaitCoro(ctx, ticket, &state) {
				if mode == "profile" && !runtime.NotifyProfileStacklessCoroForTest(state) {
					panic("notification waiter has no block profiling state")
				}
				return runtime.StacklessCoroActionWait
			}
			if state != nil {
				panic("notification wait retained state")
			}
			return runtime.StacklessCoroActionComplete
		})
		done <- ticket
	}
	// Enqueue out of ticket order to exercise the NotifyOne scan and tail.
	go logical(third)
	ordinary := 1
	if mode == "missing" {
		ordinary = 0
	}
	waitStacklessCoroNotifyQueue(&list, ordinary, 1)
	go logical(second)
	if mode == "missing" {
		if got := <-done; got != second {
			t.Fatal("missing ticket did not return immediately")
		}
		list.Signal()
		if got := <-done; got != third {
			t.Fatal("wrong last ticket")
		}
	} else {
		waitStacklessCoroNotifyQueue(&list, 1, 2)
		runtime.GC()
		if mode == "broadcast" || mode == "profile" {
			list.Broadcast()
			seen := make(map[uint32]bool)
			for range 3 {
				seen[<-done] = true
			}
			if len(seen) != 3 {
				t.Fatal("broadcast did not notify every ticket exactly once")
			}
		} else {
			for _, ticket := range []uint32{first, second, third} {
				list.Signal()
				if got := <-done; got != ticket {
					t.Fatalf("notified ticket %d, want %d", got, ticket)
				}
			}
		}
	}
	waitStacklessCoroNotifyQueue(&list, 0, 0)
	list.Signal()
	list.Broadcast()
	if mode == "profile" {
		var records []runtime.BlockProfileRecord
		for {
			n, _ := runtime.BlockProfile(nil)
			records = make([]runtime.BlockProfileRecord, n)
			if n, ok := runtime.BlockProfile(records); ok {
				records = records[:n]
				break
			}
		}
		if len(records) == 0 {
			t.Fatal("logical notification wait did not record a block event")
		}
		for _, record := range records {
			frames := runtime.CallersFrames(record.Stack())
			for {
				frame, more := frames.Next()
				if strings.HasPrefix(frame.Function, "runtime.coroNotifyListWait") ||
					frame.Function == "runtime.(*stacklessCoroNotifyWaiter).finish" {
					t.Errorf("block profile exposes notification implementation frame %s", frame.Function)
				}
				if !more {
					break
				}
			}
		}
	}
}

func testStacklessCoroNotifySiblings(t *testing.T) {
	const count = 128
	var list runtime.NotifyListStacklessCoroForTest
	done := make(chan struct{})
	before := runtime.NumGoroutine()
	go func() {
		first := true
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if first {
				first = false
				for range count {
					ticket := list.Add()
					var state unsafe.Pointer
					runtime.SpawnStacklessCoroForTest(ctx, func(ctx unsafe.Pointer) uint8 {
						if list.WaitCoro(ctx, ticket, &state) {
							return runtime.StacklessCoroActionWait
						}
						return runtime.StacklessCoroActionComplete
					})
				}
			}
			return runtime.StacklessCoroActionComplete
		})
		close(done)
	}()
	waitStacklessCoroNotifyQueue(&list, 0, count)
	if delta := runtime.NumGoroutine() - before; delta >= count/2 {
		t.Fatalf("logical waiters created %d goroutines", delta)
	}
	runtime.GC()
	// An unrelated ordinary timer and notifier must still run on one P.
	<-time.After(time.Millisecond)
	list.Broadcast()
	<-done
	waitStacklessCoroNotifyQueue(&list, 0, 0)
}

func testStacklessCoroNotifyRace(t *testing.T) {
	var list runtime.NotifyListStacklessCoroForTest
	for range 200 {
		ticket := list.Add()
		done := make(chan struct{})
		go func() {
			list.Signal()
			close(done)
		}()
		var state unsafe.Pointer
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if list.WaitCoro(ctx, ticket, &state) {
				// Yield the executor while a notifier can publish readiness
				// before this resume has returned its wait action.
				runtime.Gosched()
				return runtime.StacklessCoroActionWait
			}
			return runtime.StacklessCoroActionComplete
		})
		<-done
		if state != nil {
			t.Fatal("racing notification retained state")
		}
	}
}
