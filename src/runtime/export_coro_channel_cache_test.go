// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

type StacklessCoroChannelCacheStateForTest struct {
	Young, Old    uint32
	Charge, Limit uintptr
	Invalid       bool
}

func StacklessCoroChannelCacheForTest() StacklessCoroChannelCacheStateForTest {
	lock(&stacklessCoroSharedOperations.lock)
	state := StacklessCoroChannelCacheStateForTest{
		Young:  stacklessCoroSharedOperations.channels[0].count,
		Old:    stacklessCoroSharedOperations.channels[1].count,
		Charge: stacklessCoroChannelCacheCharge(), Limit: stacklessCoroChannelCacheBudget,
		Invalid: !validStacklessCoroChannelCacheLocked(),
	}
	for _, generation := range stacklessCoroSharedOperations.channels {
		count := uint32(0)
		for op := generation.head; op != nil && count <= generation.count; op = op.next {
			count++
			if op.hasResources() || op.waiter == nil ||
				!isCompactStacklessCoroSudog(op.waiter, unsafe.Pointer(op)) ||
				op.id != 0 || op.scheduler != nil || op.task != nil || op.received != nil ||
				op.send || op.timerWait || op.waiterActive ||
				!validReleasedStacklessCoroSudog(op, op.waiter) {
				state.Invalid = true
			}
		}
		state.Invalid = state.Invalid || count != generation.count
	}
	unlock(&stacklessCoroSharedOperations.lock)
	return state
}

func StacklessCoroChannelCacheHeadForTest() unsafe.Pointer {
	lock(&stacklessCoroSharedOperations.lock)
	head := stacklessCoroSharedOperations.channels[0].head
	unlock(&stacklessCoroSharedOperations.lock)
	return unsafe.Pointer(head)
}

func StacklessCoroReleasedOperationNextForTest(token unsafe.Pointer) unsafe.Pointer {
	lock(&stacklessCoroSharedOperations.lock)
	next := (*stacklessCoroOperation)(token).next
	unlock(&stacklessCoroSharedOperations.lock)
	return unsafe.Pointer(next)
}
