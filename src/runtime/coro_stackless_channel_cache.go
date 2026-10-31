// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/runtime/gc"
	"unsafe"
)

const (
	stacklessCoroChannelCacheBudget    = 16 << 20
	stacklessCoroChannelCacheGroups    = 2048
	stacklessCoroChannelCacheGroupSize = 64
)

// Protected by stacklessCoroSharedOperations.lock. Short independently
// rooted lists bound both the GC mark dependency chain and the work needed
// to retire an idle generation. No live operations share an allocation.
type stacklessCoroChannelCacheGeneration struct {
	heads [stacklessCoroChannelCacheGroups]*stacklessCoroOperation
	count uint32
}

var stacklessCoroChannelCache struct {
	generations [2]stacklessCoroChannelCacheGeneration
	young       uint8
	charge      uint16
	capacity    uint32
}

// Allocation class and capacity are invariant after runtime initialization.
// Keep them in the existing scalar padding beside the generation selector.
func initStacklessCoroPayloadCache() {
	size := unsafe.Sizeof(stacklessCoroChannelOperation{})
	charge := roundupsize(size, false)
	if size > gc.MinSizeForMallocHeader && size <= maxSmallSize-gc.MallocHeaderSize {
		charge += gc.MallocHeaderSize
	}
	stacklessCoroChannelCache.charge = uint16(charge)
	limit := uint32((stacklessCoroChannelCacheBudget - unsafe.Sizeof(stacklessCoroChannelCache) - stacklessCoroTaskCacheMetadata()) / charge)
	stacklessCoroChannelCache.capacity = min(limit, stacklessCoroChannelCacheGroups*stacklessCoroChannelCacheGroupSize)
}

func stacklessCoroChannelCacheCharge() uintptr {
	return uintptr(stacklessCoroChannelCache.charge)
}

func stacklessCoroChannelCacheCapacity() uint32 {
	return stacklessCoroChannelCache.capacity
}

func stacklessCoroSharedPayloadCacheBytes() uintptr {
	return unsafe.Sizeof(stacklessCoroChannelCache) + stacklessCoroTaskCacheMetadata() +
		uintptr(stacklessCoroChannelCache.generations[0].count+stacklessCoroChannelCache.generations[1].count)*stacklessCoroChannelCacheCharge() +
		stacklessCoroTaskCachePayloadBytes()
}

func validStacklessCoroChannelCacheGeneration(g *stacklessCoroChannelCacheGeneration) bool {
	if g.count > stacklessCoroChannelCacheCapacity() {
		return false
	}
	if g.count == 0 {
		return g.heads[0] == nil
	}
	last := (g.count - 1) / stacklessCoroChannelCacheGroupSize
	return g.heads[0] != nil && g.heads[last] != nil &&
		(last+1 == stacklessCoroChannelCacheGroups || g.heads[last+1] == nil)
}

func validStacklessCoroChannelCacheLocked() bool {
	a := &stacklessCoroChannelCache.generations[0]
	b := &stacklessCoroChannelCache.generations[1]
	limit := stacklessCoroChannelCacheCapacity()
	return stacklessCoroChannelCache.young <= 1 &&
		validStacklessCoroChannelCacheGeneration(a) && validStacklessCoroChannelCacheGeneration(b) &&
		a.count+b.count <= limit && stacklessCoroSharedPayloadCacheBytes() <= stacklessCoroChannelCacheBudget
}

func cacheableStacklessCoroChannel(op *stacklessCoroOperation) bool {
	if op == nil || op.hasResources() ||
		op.waiterState&stacklessCoroWaiterStorageMask != stacklessCoroWaiterCompact {
		return false
	}
	// Immutable constructor provenance excludes separately allocated full
	// waiters. Only now derive the pointer within that typed allocation.
	waiter := op.retainedWaiter()
	return op.id == 0 && op.scheduler() == nil && op.task == nil &&
		op.receivedResult() == nil && op.waiterState&stacklessCoroDirectReceived == 0 && !op.send && !op.timerWait && !op.waiterActive &&
		op.next == nil && validReleasedStacklessCoroSudog(op, waiter)
}

func takeStacklessCoroChannelCacheLocked() *stacklessCoroOperation {
	if raceenabled {
		return nil
	}
	if !validStacklessCoroChannelCacheLocked() {
		throw("runtime: invalid stackless coroutine channel cache")
	}
	for age := uint8(0); age < 2; age++ {
		g := &stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young^age]
		if g.count != 0 {
			index := (g.count - 1) / stacklessCoroChannelCacheGroupSize
			op := g.heads[index]
			g.heads[index] = op.next
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
	young := &stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young]
	old := &stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young^1]
	if young.count+old.count == stacklessCoroChannelCacheCapacity() ||
		stacklessCoroSharedPayloadCacheBytes()+stacklessCoroChannelCacheCharge() > stacklessCoroChannelCacheBudget {
		return
	}
	index := young.count / stacklessCoroChannelCacheGroupSize
	op.next = young.heads[index]
	young.heads[index] = op
	young.count++
}

// Called by clearpools with the world stopped, before marking begins.
// Recently used operations survive one GC, then expire after an idle cycle.
// Retiring a generation touches at most 2048 heads, not every allocation.
// A stale interior operation can retain at most 63 other cleared objects;
// it cannot pin the entire expired population. Clear each head's link too.
func clearStacklessCoroChannelCache() {
	if raceenabled {
		return
	}
	lock(&stacklessCoroSharedOperations.lock)
	if !validStacklessCoroChannelCacheLocked() {
		throw("runtime: invalid stackless coroutine channel cache")
	}
	old := &stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young^1]
	groups := (old.count + stacklessCoroChannelCacheGroupSize - 1) / stacklessCoroChannelCacheGroupSize
	for i := uint32(0); i < groups; i++ {
		op := old.heads[i]
		if op == nil {
			throw("runtime: truncated stackless coroutine channel cache")
		}
		op.next = nil
		old.heads[i] = nil
	}
	old.count = 0
	clearStacklessCoroCompletedTasksLocked()
	stacklessCoroChannelCache.young ^= 1
	unlock(&stacklessCoroSharedOperations.lock)
}
