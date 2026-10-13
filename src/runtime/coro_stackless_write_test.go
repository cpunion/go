// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"bytes"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroWriteResult(t *testing.T) {
	for _, test := range []struct {
		name          string
		size, written int
		count         int32
		want          int
		status        uintptr
		done          bool
	}{
		{name: "partial", size: 8, written: 2, count: 3, want: 5},
		{name: "complete", size: 8, written: 2, count: 6, want: 8, done: true},
		{name: "error", size: 8, written: 2, count: -int32(syscall.EPIPE), want: 2, status: uintptr(syscall.EPIPE), done: true},
		{name: "no progress", size: 8, written: 2, want: 2, status: (^uintptr(0) ^ (^uintptr(0) >> 1)) >> 1, done: true},
		{name: "empty", done: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			n, status, done := runtime.StacklessCoroWriteResultForTest(test.size, test.written, test.count)
			if n != test.want || status != test.status || done != test.done {
				t.Fatalf("write result = (%d, %d, %t), want (%d, %d, %t)",
					n, status, done, test.want, test.status, test.done)
			}
		})
	}
}

func TestStacklessCoroSocketDuplex(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetsockoptInt(fds[0], syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4096); err != nil {
		t.Fatal(err)
	}
	descriptor, errno := runtime.OpenStacklessCoroPollDescForTest(fds[0])
	if errno != 0 {
		t.Fatal(syscall.Errno(errno))
	}
	defer runtime.CloseStacklessCoroPollDescForTest(descriptor)
	defer runtime.UnblockStacklessCoroPollDescForTest(descriptor)
	before := runtime.StacklessCoroPollWaitCountForTest()
	payload := bytes.Repeat([]byte("duplex"), 1<<16)
	peerDone := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for runtime.StacklessCoroPollWaitCountForTest() != before+2 {
			if time.Now().After(deadline) {
				syscall.Shutdown(fds[0], syscall.SHUT_RDWR)
				syscall.Shutdown(fds[1], syscall.SHUT_RDWR)
				peerDone <- false
				return
			}
			runtime.Gosched()
		}
		runtime.GC()
		_, writeErr := syscall.Write(fds[1], []byte("r"))
		got := make([]byte, len(payload))
		offset := 0
		for offset != len(got) {
			n, err := syscall.Read(fds[1], got[offset:])
			if err == syscall.EINTR {
				continue
			}
			if err != nil || n == 0 {
				break
			}
			offset += n
		}
		peerDone <- writeErr == nil && bytes.Equal(got[:offset], payload)
	}()
	var writeN, readN int
	var writeStatus, readStatus uintptr
	var readBuffer [1]byte
	var readState, state int
	readDone := false
	sibling := func(ctx unsafe.Pointer) uint8 {
		if readState == 0 {
			readState = 1
			runtime.SocketReadWithPollDescStacklessCoroForTest(ctx, descriptor, fds[0], readBuffer[:], &readN, &readStatus)
			return runtime.StacklessCoroActionWait
		}
		readDone = true
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if state == 0 {
			state = 1
			runtime.SpawnStacklessCoroForTest(ctx, sibling)
			runtime.SocketWriteWithPollDescStacklessCoroForTest(ctx, descriptor, fds[0], payload, &writeN, &writeStatus)
			return runtime.StacklessCoroActionWait
		}
		if !readDone {
			return runtime.StacklessCoroActionYield
		}
		return runtime.StacklessCoroActionComplete
	})
	if !<-peerDone || readN != 1 || readStatus != 0 || readBuffer[0] != 'r' || writeN != len(payload) || writeStatus != 0 {
		t.Fatalf("duplex: read (%d, %d, %q), write (%d, %d)", readN, readStatus, readBuffer, writeN, writeStatus)
	}
	if after := runtime.StacklessCoroPollWaitCountForTest(); after != before {
		t.Fatalf("poll waiters = %d, want %d", after, before)
	}
}

func TestStacklessCoroFileWriteError(t *testing.T) {
	for _, buffer := range [][]byte{nil, []byte("x")} {
		var n int
		var status uintptr
		started := false
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if !started {
				started = true
				runtime.FileWriteStacklessCoroForTest(ctx, -1, buffer, &n, &status)
				return runtime.StacklessCoroActionWait
			}
			return runtime.StacklessCoroActionComplete
		})
		if n != 0 || status != uintptr(syscall.EBADF) {
			t.Fatalf("write %d bytes to invalid fd = (%d, %d), want (0, EBADF)", len(buffer), n, status)
		}
	}
}

func TestStacklessCoroFileWriteLarge(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) < 8 {
		t.Skip("requires a large virtual address range")
	}
	// The null device does not fault in the anonymous read-only pages. This
	// exercises the actual stream chunk boundary without committing a GiB of
	// physical memory or depending on available disk space.
	buffer, err := syscall.Mmap(-1, 0, 1<<30+1, syscall.PROT_READ, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}
	defer syscall.Munmap(buffer)
	fd, err := syscall.Open("/dev/null", syscall.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	var n int
	var status uintptr
	started := false
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if !started {
			started = true
			runtime.FileWriteStacklessCoroForTest(ctx, fd, buffer, &n, &status)
			return runtime.StacklessCoroActionWait
		}
		return runtime.StacklessCoroActionComplete
	})
	if n != len(buffer) || status != 0 {
		t.Fatalf("large write = (%d, %d), want (%d, 0)", n, status, len(buffer))
	}
}

// Both a blocking descriptor and a nonblocking, poll-backed descriptor must
// let a logical sibling release the reader that makes the write complete.
func TestStacklessCoroWriteProgress(t *testing.T) {
	for _, pollable := range []bool{false, true} {
		name := "blocking"
		if pollable {
			name = "poll"
		}
		t.Run(name, func(t *testing.T) {
			oldProcs := runtime.GOMAXPROCS(1)
			defer runtime.GOMAXPROCS(oldProcs)
			fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fds[0])
			defer syscall.Close(fds[1])
			if err := syscall.SetsockoptInt(fds[0], syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4096); err != nil {
				t.Fatal(err)
			}
			var descriptor unsafe.Pointer
			if pollable {
				if err := syscall.SetNonblock(fds[0], true); err != nil {
					t.Fatal(err)
				}
				var errno int
				descriptor, errno = runtime.OpenStacklessCoroPollDescForTest(fds[0])
				if errno != 0 {
					t.Fatal(syscall.Errno(errno))
				}
				defer runtime.CloseStacklessCoroPollDescForTest(descriptor)
				defer runtime.UnblockStacklessCoroPollDescForTest(descriptor)
			}
			payload := bytes.Repeat([]byte("coro write"), 1<<16)
			progress := make(chan struct{})
			readDone := make(chan []byte, 1)
			go func() {
				<-progress
				runtime.GC()
				got := make([]byte, len(payload))
				offset := 0
				for offset != len(got) {
					n, err := syscall.Read(fds[1], got[offset:])
					if err == syscall.EINTR {
						continue
					}
					if err != nil || n == 0 {
						break
					}
					offset += n
				}
				readDone <- got[:offset]
			}()
			var rescued atomic.Bool
			rescue := time.AfterFunc(5*time.Second, func() {
				rescued.Store(true)
				syscall.Shutdown(fds[0], syscall.SHUT_RDWR)
				syscall.Shutdown(fds[1], syscall.SHUT_RDWR)
			})
			defer rescue.Stop()
			var n, state int
			var status uintptr
			before := runtime.StacklessCoroOperationCountForTest()
			runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
				if state == 0 {
					state = 1
					runtime.SpawnStacklessCoroForTest(ctx, func(unsafe.Pointer) uint8 {
						close(progress)
						return runtime.StacklessCoroActionComplete
					})
					if pollable {
						runtime.SocketWriteWithPollDescStacklessCoroForTest(ctx, descriptor, fds[0], payload, &n, &status)
					} else {
						runtime.FileWriteStacklessCoroForTest(ctx, fds[0], payload, &n, &status)
					}
					return runtime.StacklessCoroActionWait
				}
				return runtime.StacklessCoroActionComplete
			})
			got := <-readDone
			if rescued.Load() || n != len(payload) || status != 0 || !bytes.Equal(got, payload) {
				t.Fatalf("write = (%d, %d), read %d bytes, rescued %t", n, status, len(got), rescued.Load())
			}
			if after := runtime.StacklessCoroOperationCountForTest(); after != before {
				t.Fatalf("registered operations = %d, want %d", after, before)
			}
		})
	}
}

func TestStacklessCoroSocketWriteDeadline(t *testing.T) {
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
		t.Fatal(syscall.Errno(errno))
	}
	defer runtime.CloseStacklessCoroPollDescForTest(descriptor)
	defer runtime.UnblockStacklessCoroPollDescForTest(descriptor)
	runtime.ExpireWriteStacklessCoroPollDescForTest(descriptor)
	for _, buffer := range [][]byte{nil, []byte("deadline")} {
		var n int
		var status uintptr
		started := false
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if !started {
				started = true
				runtime.SocketWriteWithPollDescStacklessCoroForTest(ctx, descriptor, fds[0], buffer, &n, &status)
				return runtime.StacklessCoroActionWait
			}
			return runtime.StacklessCoroActionComplete
		})
		if n != 0 || status != runtime.StacklessCoroPollErrorStatusForTest(2) {
			t.Fatalf("expired write = (%d, %d), want (0, deadline)", n, status)
		}
	}
}
