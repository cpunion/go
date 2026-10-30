// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

type StacklessCoroChannelCacheStateForTest struct {
	Young, Old              uint32
	Charge, Limit, Metadata uintptr
	Capacity                uint32
	Invalid                 bool
}

func StacklessCoroChannelCacheForTest() StacklessCoroChannelCacheStateForTest {
	lock(&stacklessCoroSharedOperations.lock)
	state := StacklessCoroChannelCacheStateForTest{
		Young:  stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young].count,
		Old:    stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young^1].count,
		Charge: stacklessCoroChannelCacheCharge(), Limit: stacklessCoroChannelCacheBudget,
		Metadata: unsafe.Sizeof(stacklessCoroChannelCache) + stacklessCoroTaskCacheMetadata(), Capacity: stacklessCoroChannelCacheCapacity(),
		Invalid: !validStacklessCoroChannelCacheLocked(),
	}
	for gi := range stacklessCoroChannelCache.generations {
		generation := &stacklessCoroChannelCache.generations[gi]
		count := uint32(0)
		for index, head := range generation.heads {
			groupCount := uint32(0)
			for op := head; op != nil && groupCount <= stacklessCoroChannelCacheGroupSize; op = op.next {
				groupCount++
				count++
				if op.hasResources() || op.waiter == nil ||
					!isCompactStacklessCoroSudog(op.waiter, unsafe.Pointer(op)) ||
					op.id != 0 || op.scheduler != nil || op.task != nil || op.received != nil ||
					op.send || op.timerWait || op.waiterActive ||
					!validReleasedStacklessCoroSudog(op, op.waiter) {
					state.Invalid = true
				}
			}
			want := min(uint32(stacklessCoroChannelCacheGroupSize), generation.count-min(generation.count, uint32(index)*stacklessCoroChannelCacheGroupSize))
			state.Invalid = state.Invalid || groupCount != want
		}
		state.Invalid = state.Invalid || count != generation.count
	}
	unlock(&stacklessCoroSharedOperations.lock)
	return state
}

func StacklessCoroChannelCacheHeadForTest() unsafe.Pointer {
	lock(&stacklessCoroSharedOperations.lock)
	g := &stacklessCoroChannelCache.generations[stacklessCoroChannelCache.young]
	var head *stacklessCoroOperation
	if g.count != 0 {
		head = g.heads[(g.count-1)/stacklessCoroChannelCacheGroupSize]
	}
	unlock(&stacklessCoroSharedOperations.lock)
	return unsafe.Pointer(head)
}

func StacklessCoroReleasedOperationNextForTest(token unsafe.Pointer) unsafe.Pointer {
	lock(&stacklessCoroSharedOperations.lock)
	next := (*stacklessCoroOperation)(token).next
	unlock(&stacklessCoroSharedOperations.lock)
	return unsafe.Pointer(next)
}
