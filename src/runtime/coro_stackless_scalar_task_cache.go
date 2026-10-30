// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

const stacklessCoroScalarTaskCacheClass = 4

// The208B scalar chunk occupies one exact allocator class on native targets.
var (
	_ [208 - unsafe.Sizeof(stacklessCoroScalarTaskChunk{})]byte
	_ [unsafe.Sizeof(stacklessCoroScalarTaskChunk{}) - 208]byte
)

const stacklessCoroScalarTaskCacheGroupSize = stacklessCoroTaskCacheGroupSize / stacklessCoroTaskChunkSize

// frame is non-nil here only for an unused slot of an existing scalar chunk.
// Its provenance comes from the reservation allocator, not from guessing
// whether two arbitrary objects happen to be adjacent.
func stacklessCoroScalarTaskFrameMarker(task *stacklessCoroTask, frame unsafe.Pointer) uint16 {
	if frame == nil || raceenabled {
		return 0
	}
	distance := uintptr(frame) - uintptr(unsafe.Pointer(task))
	const first = unsafe.Offsetof(stacklessCoroScalarTaskChunk{}.frames)
	const stride = unsafe.Sizeof(stacklessCoroTask{}) - unsafe.Sizeof(uint32(0))
	if distance > first || (first-distance)%stride != 0 || (first-distance)/stride >= stacklessCoroTaskChunkSize {
		throw("runtime: invalid stackless coroutine scalar-task origin")
	}
	return stacklessCoroScalarTaskCacheFirst - uint16((first-distance)/stride)
}

func stacklessCoroScalarTaskChunkFor(task *stacklessCoroTask) (*stacklessCoroScalarTaskChunk, uintptr) {
	index := uintptr(stacklessCoroScalarTaskCacheFirst - task.frameSize)
	return (*stacklessCoroScalarTaskChunk)(unsafe.Add(unsafe.Pointer(task), -int(index*unsafe.Sizeof(*task)))), index
}

func clearStacklessCoroScalarTaskFrame(task *stacklessCoroTask) {
	chunk, index := stacklessCoroScalarTaskChunkFor(task)
	if task.flags == stacklessCoroTaskTypedSpawn && task.parent == nil &&
		task.state == stacklessCoroTaskComplete && task.terminal == stacklessCoroTerminalNone &&
		!task.goexit && task.context.frame == unsafe.Pointer(&chunk.frames[index]) {
		chunk.frames[index] = 0
	} else {
		// Structured results, reservations, exceptional exits and switched
		// frames keep their original lifetime and invalidate whole-chunk reuse.
		task.frameSize = stacklessCoroUncachedFrameLineage
	}
}

func takeStacklessCoroCompletedScalarTaskChunk() *stacklessCoroScalarTaskChunk {
	if raceenabled {
		return nil
	}
	lock(&stacklessCoroSharedOperations.lock)
	for age := uint8(0); age < 2; age++ {
		g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young^age]
		index := g.tops[stacklessCoroScalarTaskCacheClass]
		if index == 0 {
			continue
		}
		if index > g.used || index > stacklessCoroTaskCacheGroups {
			throw("runtime: invalid stackless coroutine scalar-task group")
		}
		group := &g.groups[index]
		task := group.head
		if task == nil || group.count == 0 || group.count > stacklessCoroScalarTaskCacheGroupSize ||
			g.count == 0 || g.bytes < uint32(unsafe.Sizeof(stacklessCoroScalarTaskChunk{})) ||
			task.frameSize != stacklessCoroScalarTaskCacheFirst {
			throw("runtime: invalid stackless coroutine scalar-task cache")
		}
		group.head = task.next
		task.next = nil
		task.frameSize = 0
		group.count--
		g.count--
		g.bytes -= uint32(unsafe.Sizeof(stacklessCoroScalarTaskChunk{}))
		if group.count == 0 {
			g.tops[stacklessCoroScalarTaskCacheClass] = group.next
			group.next = g.free
			g.free = index
		}
		unlock(&stacklessCoroSharedOperations.lock)
		return (*stacklessCoroScalarTaskChunk)(unsafe.Pointer(task))
	}
	unlock(&stacklessCoroSharedOperations.lock)
	return nil
}

// The scheduler lock protects all four members from their original allocation
// through completion. Only the last normal completion can publish the chunk;
// no member is retained independently by an idle scheduler cache.
func cacheStacklessCoroCompletedScalarTaskChunk(task *stacklessCoroTask) {
	if raceenabled || !isStacklessCoroScalarTaskCacheMarker(task.frameSize) ||
		task.state != stacklessCoroTaskComplete || task.resume != nil || task.context.frame != nil ||
		task.parent != nil || task.context.scheduler != nil || task.next != nil {
		throw("runtime: invalid stackless coroutine scalar-task admission")
	}
	chunk, _ := stacklessCoroScalarTaskChunkFor(task)
	for i := range chunk.tasks {
		member := &chunk.tasks[i]
		if member.frameSize != stacklessCoroScalarTaskCacheFirst-uint16(i) ||
			member.state != stacklessCoroTaskComplete || member.resume != nil || member.parent != nil ||
			member.next != nil || member.terminal != stacklessCoroTerminalNone || member.goexit ||
			member.resuming || member.readyPending || member.flags != 0 ||
			member.context.frame != nil || member.context.scheduler != nil {
			return
		}
	}
	lock(&stacklessCoroSharedOperations.lock)
	if stacklessCoroSharedPayloadCacheBytes()+unsafe.Sizeof(*chunk) > stacklessCoroChannelCacheBudget {
		unlock(&stacklessCoroSharedOperations.lock)
		return
	}
	g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young]
	index := g.tops[stacklessCoroScalarTaskCacheClass]
	if index == 0 || g.groups[index].count == stacklessCoroScalarTaskCacheGroupSize {
		if g.free != 0 {
			index = g.free
			g.free = g.groups[index].next
		} else if g.used < stacklessCoroTaskCacheGroups {
			g.used++
			index = g.used
		} else {
			unlock(&stacklessCoroSharedOperations.lock)
			return
		}
		g.groups[index].next = g.tops[stacklessCoroScalarTaskCacheClass]
		g.tops[stacklessCoroScalarTaskCacheClass] = index
	}
	for i := range chunk.tasks {
		chunk.tasks[i] = stacklessCoroTask{}
	}
	head := &chunk.tasks[0]
	head.frameSize = stacklessCoroScalarTaskCacheFirst
	group := &g.groups[index]
	head.next = group.head
	group.head = head
	group.count++
	g.count++
	g.bytes += uint32(unsafe.Sizeof(*chunk))
	unlock(&stacklessCoroSharedOperations.lock)
}
