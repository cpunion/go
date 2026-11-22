// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

const (
	stacklessCoroTaskCacheGroups      = 2048
	stacklessCoroTaskCacheGroupSize   = 64
	stacklessCoroScalarTaskCacheClass = stacklessCoroTypedTaskCacheFirst - stacklessCoroTypedTaskCacheLast + 1
	stacklessCoroTaskCacheClasses     = stacklessCoroScalarTaskCacheClass + 1
)

// Each typed class pairs an allocation size with a complete pointer bitmap.
// The compiler excludes addresses into these private frames, and the GC uses
// the allocation's original layout across reuse. A separate class retains
// fully completed four-member scalar chunks.
type stacklessCoroTaskCacheGroup struct {
	head  *stacklessCoroTask
	next  uint16
	count uint8
}

type stacklessCoroTaskCacheGeneration struct {
	groups [stacklessCoroTaskCacheGroups + 1]stacklessCoroTaskCacheGroup
	tops   [stacklessCoroTaskCacheClasses]uint16
	used   uint16
	free   uint16
	count  uint32
	bytes  uint32
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
	return uintptr(stacklessCoroCompletedTasks.generations[0].bytes +
		stacklessCoroCompletedTasks.generations[1].bytes)
}

func takeStacklessCoroCompletedTask(typ *_type, size uintptr) (*stacklessCoroTask, uint16) {
	if raceenabled || size != 16 && size != 32 {
		return nil, stacklessCoroUncachedFrameLineage
	}
	class := uint16(*typ.GCData >> 6)
	if size == 32 {
		// The six-word task header leaves two frame bits in the first
		// byte. A second byte exists only if the scan extends past it.
		if typ.PtrBytes > 64 {
			class |= uint16(*addb(typ.GCData, 1)&3) << 2
		}
		class += 1 << 2
	}
	marker := stacklessCoroTypedTaskCacheFirst - class
	charge := uint32(unsafe.Sizeof(stacklessCoroTask{}) + size)
	lock(&stacklessCoroSharedOperations.lock)
	for age := uint8(0); age < 2; age++ {
		g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young^age]
		index := g.tops[class]
		if index == 0 {
			continue
		}
		if index > g.used || index > stacklessCoroTaskCacheGroups {
			throw("runtime: invalid stackless coroutine completed-task group")
		}
		group := &g.groups[index]
		task := group.head
		if task == nil || group.count == 0 || group.count > stacklessCoroTaskCacheGroupSize ||
			g.count == 0 || g.bytes < charge || task.frameSize != marker {
			throw("runtime: invalid stackless coroutine completed-task cache")
		}
		group.head = task.next
		task.next = nil
		group.count--
		g.count--
		g.bytes -= charge
		if group.count == 0 {
			g.tops[class] = group.next
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
	charge := uint32(unsafe.Sizeof(*task) + stacklessCoroCachedTaskFrameSize(task.frameSize))
	lock(&stacklessCoroSharedOperations.lock)
	if stacklessCoroSharedPayloadCacheBytes()+uintptr(charge) > stacklessCoroChannelCacheBudget {
		task.frameSize = 0
		unlock(&stacklessCoroSharedOperations.lock)
		return
	}
	g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young]
	class := stacklessCoroTypedTaskCacheFirst - task.frameSize
	index := g.tops[class]
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
		g.groups[index].next = g.tops[class]
		g.tops[class] = index
	}
	group := &g.groups[index]
	task.state = stacklessCoroTaskNew
	task.next = group.head
	group.head = task
	group.count++
	g.count++
	g.bytes += charge
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
	clear(old.tops[:])
	old.count, old.used, old.free = 0, 0, 0
	old.bytes = 0
}
