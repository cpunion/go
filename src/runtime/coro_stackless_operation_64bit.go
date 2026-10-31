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
	*stacklessCoroOperationResources
}

const stacklessCoroExtraResources = 1

func (op *stacklessCoroOperation) hasResources() bool {
	return op.stacklessCoroOperationResources != nil
}

func (op *stacklessCoroOperation) resources() *stacklessCoroOperationResources {
	return op.stacklessCoroOperationResources
}

func (op *stacklessCoroOperation) setResources(resources *stacklessCoroOperationResources) {
	op.stacklessCoroOperationResources = resources
}

const stacklessCoroChannelWaiterPadding = 0

// Direct IO keeps its original 176-byte combined allocation: 56+32+88.
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
	switch op.waiterState & stacklessCoroWaiterStorageMask {
	case stacklessCoroWaiterCompact:
		return (*channelWaiter)(unsafe.Add(unsafe.Pointer(op), -int(unsafe.Offsetof(stacklessCoroChannelOperation{}.operation))))
	case stacklessCoroWaiterFull:
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
		op.fullWaiter = new(sudog).channelWaiter()
	}
	return op.fullWaiter
}
