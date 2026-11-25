// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

func TestStacklessCoroRegisteredIOCount(t *testing.T) {
	before := runtime.StacklessCoroRegisteredIOCountForTest()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatalf("Pipe failed: %v", err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatalf("SetNonblock failed: %v", err)
	}

	buf := make([]byte, 4)
	var n int
	var errno uintptr
	var observed int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		state := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if state == 0 {
				state = 1
				runtime.SocketReadStacklessCoroForTest(ctx, fds[0], buf, &n, &errno)
				observed = runtime.StacklessCoroRegisteredIOCountForTest()
				return runtime.StacklessCoroActionWait
			}
			return runtime.StacklessCoroActionComplete
		})
	}()
	// Write payload so the registered socket read completes and decrements ioCount.
	if _, err := syscall.Write(fds[1], []byte("ping")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	<-done
	if errno != 0 || n != 4 || string(buf[:n]) != "ping" {
		t.Fatalf("socket read = (%d, %d, %q), want (4, 0, %q)", n, errno, string(buf[:n]), "ping")
	}
	if observed < before {
		t.Fatalf("observed ioCount = %d < baseline %d", observed, before)
	}
	if after := runtime.StacklessCoroRegisteredIOCountForTest(); after != before {
		t.Fatalf("ioCount after completion = %d, want %d", after, before)
	}
}
