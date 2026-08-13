// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package poll_test

import (
	"errors"
	. "internal/poll"
	"io"
	"syscall"
	"testing"
)

func newCoroTestFD(t *testing.T, network string) *FD {
	t.Helper()
	fd := &FD{Sysfd: -1, ZeroReadIsEOF: true}
	if err := fd.Init(network, false); err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestCoroReadStartWithoutOperation(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		fd := newCoroTestFD(t, "file")
		state, err := fd.CoroReadStart(nil, nil, new(int), new(uintptr))
		if state != CoroReadDone || err != nil {
			t.Fatalf("CoroReadStart = %v, %v, want done, nil", state, err)
		}
		// A second start verifies that the empty path released the read lock.
		state, err = fd.CoroReadStart(nil, nil, new(int), new(uintptr))
		if state != CoroReadDone || err != nil {
			t.Fatalf("second CoroReadStart = %v, %v, want done, nil", state, err)
		}
	})

	t.Run("contended", func(t *testing.T) {
		fd := newCoroTestFD(t, "file")
		if err := fd.CoroTestReadLock(); err != nil {
			t.Fatal(err)
		}
		defer fd.CoroTestReadUnlock()
		state, err := fd.CoroReadStart(nil, nil, new(int), new(uintptr))
		if state != CoroReadFallback || err != nil {
			t.Fatalf("CoroReadStart = %v, %v, want fallback, nil", state, err)
		}
	})

	t.Run("closed", func(t *testing.T) {
		fd := newCoroTestFD(t, "file")
		fd.CoroTestMarkClosed()
		state, err := fd.CoroReadStart(nil, nil, new(int), new(uintptr))
		if state != CoroReadDone || !errors.Is(err, ErrFileClosing) {
			t.Fatalf("CoroReadStart = %v, %v, want done, ErrFileClosing", state, err)
		}
	})
}

func TestCoroReadFinish(t *testing.T) {
	pollError := func(code uintptr) uintptr {
		return code | (^uintptr(0) ^ (^uintptr(0) >> 1))
	}
	tests := []struct {
		name    string
		network string
		n       int
		status  uintptr
		wantN   int
		wantErr error
	}{
		{name: "success", network: "file", n: 3, wantN: 3},
		{name: "eof", network: "file", wantErr: io.EOF},
		{name: "errno", network: "file", n: -1, status: uintptr(syscall.EBADF), wantErr: syscall.EBADF},
		{name: "deadline", network: "tcp", n: -1, status: pollError(2), wantErr: ErrDeadlineExceeded},
		{name: "file close", network: "file", n: -1, status: pollError(1), wantErr: ErrFileClosing},
		{name: "network close", network: "tcp", n: -1, status: pollError(1), wantErr: ErrNetClosing},
		{name: "not pollable", network: "tcp", n: -1, status: pollError(3), wantErr: ErrNotPollable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fd := newCoroTestFD(t, test.network)
			if err := fd.CoroTestReadLock(); err != nil {
				t.Fatal(err)
			}
			n := test.n
			err := fd.CoroReadFinish(&n, test.status)
			if n != test.wantN || !errors.Is(err, test.wantErr) {
				t.Fatalf("CoroReadFinish = %d, %v, want %d, %v",
					n, err, test.wantN, test.wantErr)
			}
			// Finish must release the retained read lock.
			if err := fd.CoroTestReadLock(); err != nil {
				t.Fatal(err)
			}
			fd.CoroTestReadUnlock()
		})
	}
}
