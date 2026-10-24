// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

var stacklessCoroAccountingPollDescForTest pollDesc

func StacklessCoroNetpollReadyAccountingForTest(mode int32, state int) (delta int32, before, after uint32, runnable, queued bool) {
	if stacklessCoroNetpoll.started.Load() != 0 || stacklessCoroNetpoll.head.Load() != 0 {
		throw("runtime: coroutine netpoll accounting probe is not isolated")
	}
	before = netpollWaiters.Load()
	gp := Escape(new(g))
	switch state {
	case 0:
		stacklessCoroNetpoll.wake.Store(pdReady)
	case 1:
		stacklessCoroNetpoll.wake.Store(pdWait)
	case 2:
		stacklessCoroNetpoll.wake.Store(uintptr(unsafe.Pointer(gp)))
	default:
		throw("runtime: invalid coroutine netpoll accounting probe state")
	}
	// Use an unregistered descriptor so kernel readiness cannot race the probe.
	pd := &stacklessCoroAccountingPollDescForTest
	pd.rg.Store(pdNil)
	pd.wg.Store(pdNil)
	readOp := Escape(newStacklessCoroIOOperation())
	writeOp := Escape(newStacklessCoroIOOperation())
	if mode == 'r' || mode == 'r'+'w' {
		if waiting, err := netpollCoroArm(pd, readOp, 'r'); !waiting || err != pollNoError {
			throw("runtime: failed to arm accounting probe read")
		}
	}
	if mode == 'w' || mode == 'r'+'w' {
		if waiting, err := netpollCoroArm(pd, writeOp, 'w'); !waiting || err != pollNoError {
			throw("runtime: failed to arm accounting probe write")
		}
	}
	var toRun gList
	delta = netpollready(&toRun, pd, mode)
	runnable = !toRun.empty()
	if runnable {
		if toRun.pop() != gp || !toRun.empty() {
			throw("runtime: unexpected accounting probe runnable list")
		}
		// Match scheduler callers: an empty list must not carry a delta.
		netpollAdjustWaiters(delta)
	}
	after = netpollWaiters.Load()
	head := stacklessCoroNetpoll.head.Swap(0)
	queued = head == uintptr(unsafe.Pointer(readOp)) || head == uintptr(unsafe.Pointer(writeOp))
	if mode == 'r'+'w' && queued {
		first := (*stacklessCoroOperation)(unsafe.Pointer(head))
		second := uintptr(first.packet[stacklessCoroPollNextWord])
		queued = second != head && (second == uintptr(unsafe.Pointer(readOp)) || second == uintptr(unsafe.Pointer(writeOp)))
	}
	stacklessCoroNetpoll.wake.Store(pdNil)
	KeepAlive(readOp)
	KeepAlive(writeOp)
	KeepAlive(gp)
	return
}
