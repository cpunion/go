// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

type coroReceivedFrameForTest struct {
	value     uint64
	ok        bool
	finalized *atomic.Uint32
}

type coroReceivedAllocationForTest struct {
	before bool
	frame  coroReceivedFrameForTest
}

func finalizeCoroReceivedForTest(p *coroReceivedAllocationForTest) {
	p.frame.finalized.Add(1)
}

// Return only the operation or its queued waiter. No independent frame/result
// pointer survives into the collections in the caller.
//
//go:noinline
func newCoroReceivedRootForTest(kind int, waiterView bool, finalized *atomic.Uint32) unsafe.Pointer {
	s := newStacklessCoroTestScheduler()
	p := &coroReceivedAllocationForTest{frame: coroReceivedFrameForTest{value: 73, finalized: finalized}}
	SetFinalizer(p, finalizeCoroReceivedForTest)
	task := &stacklessCoroTask{state: stacklessCoroTaskWaiting}
	task.context.scheduler = s
	task.context.frame = unsafe.Pointer(&p.frame)
	op := newStacklessCoroChannelOperation()
	if kind == 3 {
		op = new(stacklessCoroOperation)
	} else if kind == 4 {
		op = newStacklessCoroIOOperation()
	}
	op.task = task
	op.id = ^uint64(0) - 111
	received := &p.frame.ok
	if kind == 1 {
		other := &coroReceivedAllocationForTest{frame: coroReceivedFrameForTest{value: 73, finalized: finalized}}
		SetFinalizer(other, finalizeCoroReceivedForTest)
		received = &other.frame.ok
	} else if kind == 2 {
		received = &p.before
	}
	op.setReceived(received)
	// Plain-origin storage must promote an active direct result into its
	// new waiter/resource header before replacing the scanned pointer slot.
	waiter := newStacklessCoroSudog(op, nil)
	waiter.coro.set(unsafe.Pointer(op))
	if waiterView {
		return unsafe.Pointer(op.retainedWaiter())
	}
	return unsafe.Pointer(op)
}

func StacklessCoroReceivedStorageGCForTest(kind int, waiterView bool) (bool, unsafe.Pointer, uint32) {
	finalized := new(atomic.Uint32)
	token := newCoroReceivedRootForTest(kind, waiterView, finalized)
	GC()
	GC()
	GC()
	op := (*stacklessCoroOperation)(token)
	if waiterView {
		op = (*stacklessCoroOperation)((*channelWaiter)(token).coro.get())
	}
	valid := finalized.Load() == 0 && op.id == ^uint64(0)-111 && op.scheduler() != nil
	valid = valid && op.receivedResult() != nil
	valid = valid && (unsafe.Sizeof(uintptr(0)) != 8 || op.hasResources() == (kind >= 3))
	frame := (*coroReceivedFrameForTest)(op.task.context.frame)
	valid = valid && frame.value == 73
	received := op.receivedResult()
	*received = true
	if kind == 0 || kind >= 3 {
		valid = valid && frame.ok
	} else if kind == 2 {
		p := (*coroReceivedAllocationForTest)(unsafe.Add(op.task.context.frame, -int(unsafe.Offsetof(coroReceivedAllocationForTest{}.frame))))
		valid = valid && p.before && !frame.ok
	} else {
		valid = valid && !frame.ok
	}
	op.retainedWaiter().coro.clear()
	releaseStacklessCoroSudog(unsafe.Pointer(op), op.retainedWaiter())
	clearStacklessCoroOperation(op)
	valid = valid && op.task == nil && op.id == 0 && op.receivedResult() == nil && op.waiterState&stacklessCoroDirectReceived == 0
	if op.hasResources() {
		valid = valid && op.resourceState().received == nil
	}
	expected := uint32(1)
	if kind == 1 {
		expected = 2
	}
	return valid, unsafe.Pointer(finalized), expected
}

func StacklessCoroReceivedStorageFinalizedForTest(token unsafe.Pointer) uint32 {
	return (*atomic.Uint32)(token).Load()
}
