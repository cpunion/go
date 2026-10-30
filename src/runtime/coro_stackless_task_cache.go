// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

const (
	stacklessCoroTaskCacheGroups     = 2048
	stacklessCoroTaskCacheGroupSize  = 64
	stacklessCoroTaskCacheFrameBytes = 16
)

// Only independently allocated six-word tasks followed by a two-word frame
// enter this pool. The four lists distinguish every possible frame pointer
// bitmap. Compiler eligibility excludes addresses into these private frames.
// Each reused allocation therefore retains its original precise GC layout.
// Active tasks never share storage.
type stacklessCoroTaskCacheGroup struct {
	head  *stacklessCoroTask
	next  uint16
	count uint8
}

type stacklessCoroTaskCacheGeneration struct {
	groups [stacklessCoroTaskCacheGroups + 1]stacklessCoroTaskCacheGroup
	tops   [4]uint16
	used   uint16
	free   uint16
	count  uint32
}

// Protected by stacklessCoroSharedOperations.lock. Metadata and both task
// and channel payloads share the existing sixteen-MiB global budget.
var stacklessCoroCompletedTasks struct {
	generations [2]stacklessCoroTaskCacheGeneration
}

func stacklessCoroTaskCacheMetadata() uintptr {
	return unsafe.Sizeof(stacklessCoroCompletedTasks)
}

func stacklessCoroTaskCachePayloadBytes() uintptr {
	return uintptr(stacklessCoroCompletedTasks.generations[0].count+
		stacklessCoroCompletedTasks.generations[1].count) *
		(unsafe.Sizeof(stacklessCoroTask{}) + stacklessCoroTaskCacheFrameBytes)
}

func takeStacklessCoroCompletedTask(typ *_type, size uintptr) (*stacklessCoroTask, uint16) {
	if raceenabled || size != stacklessCoroTaskCacheFrameBytes {
		return nil, stacklessCoroUncachedFrameLineage
	}
	bitmap := uint8(*typ.GCData >> 6)
	marker := stacklessCoroTypedTaskCacheFirst - uint16(bitmap)
	lock(&stacklessCoroSharedOperations.lock)
	for age := uint8(0); age < 2; age++ {
		g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young^age]
		index := g.tops[bitmap]
		if index == 0 {
			continue
		}
		if index > g.used || index > stacklessCoroTaskCacheGroups {
			throw("runtime: invalid stackless coroutine completed-task group")
		}
		group := &g.groups[index]
		task := group.head
		if task == nil || group.count == 0 || group.count > stacklessCoroTaskCacheGroupSize ||
			g.count == 0 || task.frameSize != marker {
			throw("runtime: invalid stackless coroutine completed-task cache")
		}
		group.head = task.next
		task.next = nil
		group.count--
		g.count--
		if group.count == 0 {
			g.tops[bitmap] = group.next
			group.next = g.free
			g.free = index
		}
		unlock(&stacklessCoroSharedOperations.lock)
		return task, marker
	}
	unlock(&stacklessCoroSharedOperations.lock)
	return nil, marker
}

func cacheStacklessCoroCompletedTask(task *stacklessCoroTask) {
	if raceenabled || !isStacklessCoroTypedTaskCacheMarker(task.frameSize) ||
		task.state != stacklessCoroTaskComplete || task.resume != nil ||
		task.context.frame != nil || task.context.scheduler != nil || task.next != nil {
		throw("runtime: invalid stackless coroutine completed-task admission")
	}
	lock(&stacklessCoroSharedOperations.lock)
	if stacklessCoroSharedPayloadCacheBytes()+unsafe.Sizeof(stacklessCoroTask{})+
		stacklessCoroTaskCacheFrameBytes > stacklessCoroChannelCacheBudget {
		task.frameSize = 0
		unlock(&stacklessCoroSharedOperations.lock)
		return
	}
	g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young]
	bitmap := stacklessCoroTypedTaskCacheFirst - task.frameSize
	index := g.tops[bitmap]
	if index == 0 || g.groups[index].count == stacklessCoroTaskCacheGroupSize {
		if g.free != 0 {
			index = g.free
			g.free = g.groups[index].next
		} else if g.used < stacklessCoroTaskCacheGroups {
			g.used++
			index = g.used
		} else {
			task.frameSize = 0
			unlock(&stacklessCoroSharedOperations.lock)
			return
		}
		g.groups[index].next = g.tops[bitmap]
		g.tops[bitmap] = index
	}
	group := &g.groups[index]
	task.state = stacklessCoroTaskNew
	task.next = group.head
	group.head = task
	group.count++
	g.count++
	unlock(&stacklessCoroSharedOperations.lock)
}

// Called before the shared young index rotates, with the world stopped and
// the shared pool lock held. Retiring a generation touches at most 2048 heads.
// A stale interior entry can retain at most 63 other fully cleared tasks.
func clearStacklessCoroCompletedTasksLocked() {
	old := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young^1]
	for i := uint16(1); i <= old.used; i++ {
		group := &old.groups[i]
		if group.head != nil {
			group.head.next = nil
		}
		*group = stacklessCoroTaskCacheGroup{}
	}
	old.tops = [4]uint16{}
	old.count, old.used, old.free = 0, 0, 0
}
