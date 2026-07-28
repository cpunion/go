// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
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

func TestStacklessCoroSpawn(t *testing.T) {
	const tasks = 100000
	var completed int
	var rootState int
	child := func(unsafe.Pointer) uint8 {
		completed++
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
			if completed != tasks {
				t.Fatalf("completed = %d, want %d", completed, tasks)
			}
			rootState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected root state %d", rootState)
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func TestStacklessCoroSleep(t *testing.T) {
	const delay = 5 * time.Millisecond
	var state int
	start := time.Now()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
			runtime.SleepStacklessCoroForTest(ctx, int64(delay))
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

	var rescued atomic.Bool
	rescue := time.AfterFunc(5*time.Second, func() {
		rescued.Store(true)
		syscall.Write(fds[1], []byte{'r'})
	})
	defer rescue.Stop()

	var writeErr error
	sibling := func(unsafe.Pointer) uint8 {
		_, writeErr = syscall.Write(fds[1], []byte{'p'})
		return runtime.StacklessCoroActionComplete
	}
	var state int
	buffer := make([]byte, 1)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			runtime.SpawnStacklessCoroForTest(ctx, sibling)
			if n := runtime.BlockingReadStacklessCoroForTest(ctx, fds[0], buffer); n != 1 {
				t.Fatalf("blocking read = %d, want 1", n)
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
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if rescued.Load() {
		t.Fatal("blocking foreign call stopped stackless sibling progress")
	}
	if got := string(buffer); got != "p" {
		t.Fatalf("blocking read = %q, want %q", got, "p")
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
