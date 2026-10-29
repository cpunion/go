// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/runtime/gc"
	"unsafe"
)

// Charge both generations together. This retains existing, individually
// allocated dead objects; it never combines independently live lifetimes.
const stacklessCoroChannelCacheBudget = 16 << 20

func stacklessCoroChannelCacheCharge() uintptr {
	size := unsafe.Sizeof(stacklessCoroChannelOperation{})
	charge := roundupsize(size, false)
	if size > gc.MinSizeForMallocHeader && size <= maxSmallSize-gc.MallocHeaderSize {
		charge += gc.MallocHeaderSize
	}
	return charge
}

func validStacklessCoroChannelCacheLocked() bool {
	young := &stacklessCoroSharedOperations.channels[0]
	old := &stacklessCoroSharedOperations.channels[1]
	limit := uint32(stacklessCoroChannelCacheBudget / stacklessCoroChannelCacheCharge())
	return (young.head == nil) == (young.count == 0) &&
		(old.head == nil) == (old.count == 0) &&
		young.count <= limit && old.count <= limit && young.count+old.count <= limit
}

func cacheableStacklessCoroChannel(op *stacklessCoroOperation) bool {
	if op == nil || op.hasResources() || op.waiter == nil ||
		!isCompactStacklessCoroSudog(op.waiter, unsafe.Pointer(op)) {
		return false
	}
	// A separately allocated full waiter is not a compact allocation, even
	// when it is attached to an otherwise resource-free operation.
	return op.id == 0 && op.scheduler == nil && op.task == nil &&
		op.received == nil && !op.send && !op.timerWait && !op.waiterActive &&
		op.next == nil && validReleasedStacklessCoroSudog(op, op.waiter)
}

func takeStacklessCoroChannelCacheLocked() *stacklessCoroOperation {
	if raceenabled {
		return nil
	}
	if !validStacklessCoroChannelCacheLocked() {
		throw("runtime: invalid stackless coroutine channel cache")
	}
	for i := range stacklessCoroSharedOperations.channels {
		g := &stacklessCoroSharedOperations.channels[i]
		if op := g.head; op != nil {
			g.head = op.next
			g.count--
			op.next = nil
			return op
		}
	}
	return nil
}

func cacheStacklessCoroChannelLocked(op *stacklessCoroOperation) {
	if raceenabled || !cacheableStacklessCoroChannel(op) {
		return
	}
	if !validStacklessCoroChannelCacheLocked() {
		throw("runtime: invalid stackless coroutine channel cache")
	}
	young := &stacklessCoroSharedOperations.channels[0]
	old := &stacklessCoroSharedOperations.channels[1]
	limit := uint32(stacklessCoroChannelCacheBudget / stacklessCoroChannelCacheCharge())
	if young.count+old.count == limit {
		return
	}
	op.next = young.head
	young.head = op
	young.count++
}

// Called by clearpools with the world stopped, before marking begins. The
// same mutex also excludes completion producers. As with the central sudog
// pool, disconnect every discarded link so one retained operation cannot
// keep an entire expired generation alive. Work is bounded by the byte cap.
// Freshly cached objects survive one GC; unused objects expire at the next.
func clearStacklessCoroChannelCache() {
	if raceenabled {
		return
	}
	lock(&stacklessCoroSharedOperations.lock)
	if !validStacklessCoroChannelCacheLocked() {
		throw("runtime: invalid stackless coroutine channel cache")
	}
	young := &stacklessCoroSharedOperations.channels[0]
	old := &stacklessCoroSharedOperations.channels[1]
	op := old.head
	for remaining := old.count; remaining != 0; remaining-- {
		if op == nil {
			throw("runtime: truncated stackless coroutine channel cache")
		}
		next := op.next
		op.next = nil
		op = next
	}
	if op != nil {
		throw("runtime: oversized stackless coroutine channel cache")
	}
	*old = *young
	young.head = nil
	young.count = 0
	unlock(&stacklessCoroSharedOperations.lock)
}
