// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroNetpollPopOneForTest() string {
	if stacklessCoroNetpoll.started.Load() != 0 ||
		stacklessCoroNetpoll.head.Load() != 0 ||
		stacklessCoroNetpoll.drained.Load() != 0 {
		return "netpoll parallel-pop probe is not isolated"
	}
	const n = 4
	ops := make([]*stacklessCoroOperation, n)
	for i := range ops {
		op := newStacklessCoroIOOperation()
		op.id = uint64(i + 1)
		ops[i] = op
		stacklessCoroNetpollReady(op)
	}
	seen := make([]bool, n)
	for range n {
		op := stacklessCoroNetpollPopOne(true)
		if op == nil {
			return "stacklessCoroNetpollPopOne returned nil early"
		}
		if op.resourceState().packet[stacklessCoroPollNextWord] != 0 {
			return "popped op retained non-zero next link"
		}
		idx := int(op.id - 1)
		if idx < 0 || idx >= n || seen[idx] {
			return "popped unexpected or duplicate op"
		}
		seen[idx] = true
	}
	if stacklessCoroNetpollPopOne(true) != nil || stacklessCoroNetpollPopOne(false) != nil {
		return "netpoll queue not empty after popping all ops"
	}
	// Verify empty Dekker re-arm restores wake from pdReady back to loopG without waking.
	fakeLoop := Escape(new(g))
	stacklessCoroNetpoll.loopG.Store(fakeLoop)
	stacklessCoroNetpoll.wake.Store(pdReady)
	stacklessCoroNetpollReleaseLoop()
	if got := stacklessCoroNetpoll.wake.Load(); got != uintptr(unsafe.Pointer(fakeLoop)) {
		stacklessCoroNetpoll.loopG.Store(nil)
		stacklessCoroNetpoll.wake.Store(pdNil)
		return "empty releaseLoop did not re-arm wake to loopG"
	}
	stacklessCoroNetpoll.loopG.Store(nil)
	stacklessCoroNetpoll.wake.Store(pdNil)
	KeepAlive(fakeLoop)
	KeepAlive(ops)
	return ""
}
