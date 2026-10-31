// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !(386 || arm || mips || mipsle)

package runtime

import (
	"internal/goarch"
	"unsafe"
)

var (
	_ [goarch.PtrSize - 8]byte
	_ [8 - goarch.PtrSize]byte
)

// Channel operations need only a retained waiter. Allocate the other reusable
// resources with the first IO, timer, or select payload that needs them.
type stacklessCoroOperation struct {
	stacklessCoroOperationState
	// extra holds either reusable resources or a receive result, always as a
	// scanned pointer. A scalar flag distinguishes them; pointer bits and
	// full-width operation IDs do not encode the kind.
	extra unsafe.Pointer
}

const stacklessCoroExtraResources = 1

func (op *stacklessCoroOperation) hasResources() bool {
	return op.extra != nil && op.waiterState&stacklessCoroDirectReceived == 0
}

func (op *stacklessCoroOperation) resources() *stacklessCoroOperationResources {
	if !op.hasResources() {
		return nil
	}
	return op.resourceState()
}

// resourceState is used only after resources are known to exist, as with
// the previously promoted resource fields. It adds no branch to IO accesses.
func (op *stacklessCoroOperation) resourceState() *stacklessCoroOperationResources {
	return (*stacklessCoroOperationResources)(op.extra)
}

func (op *stacklessCoroOperation) setResources(resources *stacklessCoroOperationResources) {
	if op.waiterState&stacklessCoroDirectReceived != 0 {
		// Install the result's new typed root before replacing its old root.
		resources.received = (*bool)(op.extra)
		op.extra = unsafe.Pointer(resources)
		op.waiterState &^= stacklessCoroDirectReceived
	} else {
		op.extra = unsafe.Pointer(resources)
	}
}

func (op *stacklessCoroOperation) setReceived(received *bool) {
	if op.hasResources() {
		op.resourceState().received = received
	} else {
		op.extra = unsafe.Pointer(received)
		if received != nil {
			op.waiterState |= stacklessCoroDirectReceived
		} else {
			op.waiterState &^= stacklessCoroDirectReceived
		}
	}
}

func (op *stacklessCoroOperation) receivedResult() *bool {
	if op.waiterState&stacklessCoroDirectReceived != 0 {
		return (*bool)(op.extra)
	}
	if op.extra != nil {
		return op.resourceState().received
	}
	return nil
}

func (op *stacklessCoroOperation) clearReceived() {
	if op.waiterState&stacklessCoroDirectReceived != 0 {
		op.extra = nil
		op.waiterState &^= stacklessCoroDirectReceived
	} else if op.extra != nil {
		op.resourceState().received = nil
	}
}

const stacklessCoroChannelWaiterPadding = 0

// Direct IO keeps its original 176-byte combined allocation: 40+40+88 (168 bytes before allocator rounding).
type stacklessCoroOperationResources struct {
	stacklessCoroOperationResourceState
	fullWaiter *channelWaiter
}

type stacklessCoroWaiterResources struct {
	resources stacklessCoroOperationResources
	waiter    sudog
}

// The allocation kind is immutable. Both derived addresses stay within the
// original precisely typed allocation; no neighbouring object is inspected.
//
//go:nosplit
func (op *stacklessCoroOperation) retainedWaiter() *channelWaiter {
	// Constructors set at most one storage bit, and completion preserves it.
	// Test the common compact bit directly instead of decoding an enum.
	if op.waiterState&stacklessCoroWaiterCompact != 0 {
		return (*channelWaiter)(unsafe.Add(unsafe.Pointer(op), -int(unsafe.Offsetof(stacklessCoroChannelOperation{}.operation))))
	}
	if op.waiterState&stacklessCoroWaiterFull != 0 {
		return (*channelWaiter)(add(unsafe.Pointer(op), unsafe.Offsetof(stacklessCoroFullChannelOperation{}.waiter)))
	}
	if resources := op.resources(); resources != nil {
		return resources.fullWaiter
	}
	return nil
}

func (op *stacklessCoroOperation) initializeWaiter(waiter *channelWaiter) {
	if op.retainedWaiter() != waiter {
		throw("runtime: invalid inline channel waiter")
	}
}

// Called with an M held, like acquireSudog. Publish the complete typed object
// before the caller can release its M or publish an external channel source.
func (op *stacklessCoroOperation) newRetainedWaiter() *channelWaiter {
	if !op.hasResources() {
		storage := new(stacklessCoroWaiterResources)
		storage.resources.fullWaiter = storage.waiter.channelWaiter()
		op.setResources(&storage.resources)
	} else {
		op.resourceState().fullWaiter = new(sudog).channelWaiter()
	}
	return op.resourceState().fullWaiter
}
