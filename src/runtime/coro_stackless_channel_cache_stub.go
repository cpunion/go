// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !((darwin && arm64) || (linux && amd64))

package runtime

const stacklessCoroManagedChannelCacheCap = 1024

// Protected by stacklessCoroSharedOperations.lock. Keeps a small bounded
// two-generation burst cache on non-native targets (e.g. linux/arm64) so
// N=1000 channel bursts reuse operations without retaining large heaps across
// idle GC cycles or slowing 100k wake-drain with deep lists.
var stacklessCoroManagedChannelCache struct {
	heads [2]*stacklessCoroOperation
	count [2]uint32
	young uint8
}

func initStacklessCoroPayloadCache() {}

func cacheableStacklessCoroManagedChannel(op *stacklessCoroOperation) bool {
	if op == nil || op.hasResources() ||
		op.waiterState&stacklessCoroWaiterStorageMask != stacklessCoroWaiterCompact {
		return false
	}
	waiter := op.retainedWaiter()
	return op.id == 0 && op.scheduler() == nil && op.task == nil &&
		op.receivedResult() == nil && op.waiterState&stacklessCoroDirectReceived == 0 &&
		!op.registeredIO && !op.send && !op.timerWait && !op.waiterActive &&
		op.next == nil && validReleasedStacklessCoroSudog(op, waiter)
}

func takeStacklessCoroChannelCacheLocked() *stacklessCoroOperation {
	if raceenabled {
		return nil
	}
	for age := uint8(0); age < 2; age++ {
		gen := stacklessCoroManagedChannelCache.young ^ age
		if op := stacklessCoroManagedChannelCache.heads[gen]; op != nil {
			stacklessCoroManagedChannelCache.heads[gen] = op.next
			stacklessCoroManagedChannelCache.count[gen]--
			op.next = nil
			return op
		}
	}
	return nil
}

func cacheStacklessCoroChannelLocked(op *stacklessCoroOperation) {
	if raceenabled ||
		stacklessCoroManagedChannelCache.count[0]+stacklessCoroManagedChannelCache.count[1] >= stacklessCoroManagedChannelCacheCap {
		return
	}
	if !cacheableStacklessCoroManagedChannel(op) {
		return
	}
	young := stacklessCoroManagedChannelCache.young
	op.next = stacklessCoroManagedChannelCache.heads[young]
	stacklessCoroManagedChannelCache.heads[young] = op
	stacklessCoroManagedChannelCache.count[young]++
}

func clearStacklessCoroManagedChannelCache() {
	if raceenabled {
		return
	}
	lock(&stacklessCoroSharedOperations.lock)
	old := stacklessCoroManagedChannelCache.young ^ 1
	stacklessCoroManagedChannelCache.heads[old] = nil
	stacklessCoroManagedChannelCache.count[old] = 0
	stacklessCoroManagedChannelCache.young = old
	unlock(&stacklessCoroSharedOperations.lock)
}

func resetStacklessCoroManagedChannelCacheForTest() {
	lock(&stacklessCoroSharedOperations.lock)
	stacklessCoroManagedChannelCache.heads = [2]*stacklessCoroOperation{}
	stacklessCoroManagedChannelCache.count = [2]uint32{}
	stacklessCoroManagedChannelCache.young = 0
	unlock(&stacklessCoroSharedOperations.lock)
}
