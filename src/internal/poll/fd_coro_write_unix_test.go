// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package poll_test

import (
	"errors"
	"internal/asan"
	"internal/msan"
	. "internal/poll"
	"internal/race"
	"io"
	"syscall"
	"testing"
	"time"
)

func coroTestWriteLock(t *testing.T, fd *FD) {
	t.Helper()
	if locked, err := fd.CoroTestTryWriteLock(); !locked || err != nil {
		t.Fatalf("tryWriteLock = %v, %v, want true, nil", locked, err)
	}
}

func TestCoroWriteStartWithoutOperation(t *testing.T) {
	t.Run("contended", func(t *testing.T) {
		fd := newCoroTestFD(t, "file")
		fd.IsStream = true
		coroTestWriteLock(t, fd)
		state, err := fd.CoroWriteStart(nil, nil, new(int), new(uintptr))
		if state != CoroWriteFallback || err != nil {
			t.Fatalf("CoroWriteStart = %v, %v, want fallback, nil", state, err)
		}
		// Start must not release another writer's lock.
		if locked, err := fd.CoroTestTryWriteLock(); locked || err != nil {
			t.Fatalf("tryWriteLock = %v, %v, want false, nil", locked, err)
		}
		fd.CoroTestWriteUnlock()
		coroTestWriteLock(t, fd)
		fd.CoroTestWriteUnlock()
	})

	for _, test := range []struct {
		network string
		wantErr error
	}{
		{"file", ErrFileClosing},
		{"tcp", ErrNetClosing},
	} {
		t.Run("closed/"+test.network, func(t *testing.T) {
			fd := newCoroTestFD(t, test.network)
			fd.IsStream = true
			fd.CoroTestMarkClosed()
			state, err := fd.CoroWriteStart(nil, nil, new(int), new(uintptr))
			if state != CoroWriteDone || !errors.Is(err, test.wantErr) {
				t.Fatalf("CoroWriteStart = %v, %v, want done, %v", state, err, test.wantErr)
			}
		})
	}

	t.Run("nonstream", func(t *testing.T) {
		fd := newCoroTestFD(t, "udp")
		state, err := fd.CoroWriteStart(nil, []byte("packet"), new(int), new(uintptr))
		if state != CoroWriteFallback || err != nil {
			t.Fatalf("CoroWriteStart = %v, %v, want fallback, nil", state, err)
		}
		// The worker will acquire its own lock, so Start must release it.
		coroTestWriteLock(t, fd)
		fd.CoroTestWriteUnlock()
	})

	t.Run("deadline", func(t *testing.T) {
		var pipe [2]int
		if err := syscall.Pipe(pipe[:]); err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(pipe[0])
		if err := syscall.SetNonblock(pipe[1], true); err != nil {
			syscall.Close(pipe[1])
			t.Fatal(err)
		}
		fd := &FD{Sysfd: pipe[1], IsStream: true}
		if err := fd.Init("file", true); err != nil {
			syscall.Close(pipe[1])
			t.Fatal(err)
		}
		defer fd.Close()
		if err := fd.SetWriteDeadline(time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
		// Empty writes still observe prepareWrite errors. Unlike Read,
		// Write must not return success before checking the deadline.
		for _, p := range [][]byte{nil, []byte("data")} {
			state, err := fd.CoroWriteStart(nil, p, new(int), new(uintptr))
			if race.Enabled || msan.Enabled || asan.Enabled {
				// The worker performs prepareWrite after acquiring its own
				// lock, preserving the ordinary syscall instrumentation.
				if state != CoroWriteFallback || err != nil {
					t.Fatalf("CoroWriteStart(%q) = %v, %v, want fallback, nil", p, state, err)
				}
			} else if state != CoroWriteDone || !errors.Is(err, ErrDeadlineExceeded) {
				t.Fatalf("CoroWriteStart(%q) = %v, %v, want done, ErrDeadlineExceeded", p, state, err)
			}
			coroTestWriteLock(t, fd)
			fd.CoroTestWriteUnlock()
		}
	})
}

func TestCoroWriteFinish(t *testing.T) {
	pollTag := ^uintptr(0) ^ (^uintptr(0) >> 1)
	tests := []struct {
		name    string
		network string
		n       int
		status  uintptr
		wantN   int
		wantErr error
	}{
		{name: "success", network: "file", n: 3, wantN: 3},
		{name: "empty", network: "file"},
		{name: "errno", network: "file", n: -1, status: uintptr(syscall.EBADF), wantErr: syscall.EBADF},
		{name: "partial errno", network: "file", n: 3, status: uintptr(syscall.ENOSPC), wantN: 3, wantErr: syscall.ENOSPC},
		{name: "broken pipe", network: "file", status: uintptr(syscall.EPIPE), wantErr: syscall.EPIPE},
		{name: "zero progress", network: "file", status: pollTag >> 1, wantErr: io.ErrUnexpectedEOF},
		{name: "partial progress", network: "file", n: 3, status: pollTag >> 1, wantN: 3, wantErr: io.ErrUnexpectedEOF},
		{name: "deadline", network: "tcp", status: pollTag | 2, wantErr: ErrDeadlineExceeded},
		{name: "partial deadline", network: "tcp", n: 3, status: pollTag | 2, wantN: 3, wantErr: ErrDeadlineExceeded},
		{name: "file close", network: "file", status: pollTag | 1, wantErr: ErrFileClosing},
		{name: "network close", network: "tcp", status: pollTag | 1, wantErr: ErrNetClosing},
		{name: "partial close", network: "tcp", n: 3, status: pollTag | 1, wantN: 3, wantErr: ErrNetClosing},
		{name: "not pollable", network: "tcp", status: pollTag | 3, wantErr: ErrNotPollable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fd := newCoroTestFD(t, test.network)
			coroTestWriteLock(t, fd)
			// Reads and writes use independent locks. Finishing a write
			// must release only the write lock.
			if err := fd.CoroTestReadLock(); err != nil {
				t.Fatal(err)
			}
			n := test.n
			err := fd.CoroWriteFinish(&n, test.status)
			if n != test.wantN || !errors.Is(err, test.wantErr) {
				t.Fatalf("CoroWriteFinish = %d, %v, want %d, %v", n, err, test.wantN, test.wantErr)
			}
			coroTestWriteLock(t, fd)
			fd.CoroTestWriteUnlock()
			state, err := fd.CoroReadStart(nil, nil, new(int), new(uintptr))
			if state != CoroReadFallback || err != nil {
				t.Fatalf("CoroReadStart = %v, %v, want fallback, nil", state, err)
			}
			fd.CoroTestReadUnlock()
		})
	}
}
