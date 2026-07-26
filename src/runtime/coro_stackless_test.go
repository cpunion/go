// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
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

func TestStacklessCoroOperationRegistry(t *testing.T) {
	if !runtime.CheckStacklessCoroOperationRegistryForTest() {
		t.Fatal("operation registry did not reject stale or duplicate completion")
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
	go func() {
		time.Sleep(5 * time.Millisecond)
		_, _ = syscall.Write(fds[1], []byte("file"))
	}()

	var state, n int
	var errno uintptr
	buffer := make([]byte, 4)
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			state = 1
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

func TestStacklessCoroSocketRead(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		_, _ = syscall.Write(fds[1], []byte("poll"))
	}()

	var state, n int
	var errno uintptr
	buffer := make([]byte, 4)
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
