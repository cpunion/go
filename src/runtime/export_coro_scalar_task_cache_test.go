// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

func scalarTaskCacheChunkForTest() *stacklessCoroScalarTaskChunk {
	chunk := new(stacklessCoroScalarTaskChunk)
	scalarTaskCacheInitializeForTest(chunk)
	return chunk
}
func scalarTaskCacheInitializeForTest(chunk *stacklessCoroScalarTaskChunk) {
	for i := range chunk.tasks {
		task := initializeStacklessCoroTask(&chunk.tasks[i], unsafe.Pointer(&chunk.frames[i]), stacklessCoroScalarTaskFrameTestResume, nil)
		task.frameSize = stacklessCoroScalarTaskFrameMarker(task, task.context.frame)
		task.flags = stacklessCoroTaskTypedSpawn
		task.state = stacklessCoroTaskRunning
		chunk.frames[i] = uint32(0xfeed + i)
	}
}
func scalarTaskCacheCompleteForTest(s *stacklessCoroScheduler, chunk *stacklessCoroScalarTaskChunk, index int) {
	task := &chunk.tasks[index]
	task.state = stacklessCoroTaskComplete
	clearStacklessCoroTaskFrame(task)
	s.recycleTaskLocked(task)
}
func scalarTaskCacheWholeForTest(s *stacklessCoroScheduler, chunk *stacklessCoroScalarTaskChunk) {
	lock(&s.lock)
	for i := 3; i >= 0; i-- {
		scalarTaskCacheCompleteForTest(s, chunk, i)
	}
	unlock(&s.lock)
}
func CheckStacklessCoroScalarTaskCacheForTest() bool {
	if raceenabled {
		return true
	}
	if unsafe.Sizeof(stacklessCoroTask{}) != 48 || unsafe.Sizeof(stacklessCoroScalarTaskChunk{}) != 208 {
		return false
	}
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankStacklessCoro)
	var chunks [48]*stacklessCoroScalarTaskChunk
	for i := range chunks {
		chunk := scalarTaskCacheChunkForTest()
		chunks[i] = chunk
		lock(&s.lock)
		for _, j := range [4]int{2, 0, 3, 1} {
			if chunk.tasks[j].frameSize != stacklessCoroScalarTaskCacheFirst-uint16(j) {
				unlock(&s.lock)
				return false
			}
			scalarTaskCacheCompleteForTest(s, chunk, j)
			if j != 1 && i == 0 && takeStacklessCoroCompletedScalarTaskChunk() != nil {
				unlock(&s.lock)
				return false
			}
		}
		unlock(&s.lock)
	}
	for round := 0; round < 3; round++ {
		seen := make(map[*stacklessCoroScalarTaskChunk]bool)
		for i := range chunks {
			chunk := takeStacklessCoroCompletedScalarTaskChunk()
			if chunk == nil || seen[chunk] || chunk.frames != [4]uint32{} {
				return false
			}
			seen[chunk] = true
			for j := range chunk.tasks {
				member := &chunk.tasks[j]
				if member.resume != nil || member.parent != nil || member.next != nil || member.state != stacklessCoroTaskNew || member.terminal != stacklessCoroTerminalNone || member.goexit || member.resuming || member.readyPending || member.flags != 0 || member.frameSize != 0 || member.context.frame != nil || member.context.scheduler != nil {
					return false
				}
			}
			chunks[i] = chunk
		}
		if takeStacklessCoroCompletedScalarTaskChunk() != nil {
			return false
		}
		lock(&stacklessCoroSharedOperations.lock)
		used := stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young].used
		unlock(&stacklessCoroSharedOperations.lock)
		if used != 3 {
			return false
		}
		for _, chunk := range chunks {
			scalarTaskCacheInitializeForTest(chunk)
			scalarTaskCacheWholeForTest(s, chunk)
		}
	}
	for range chunks {
		if takeStacklessCoroCompletedScalarTaskChunk() == nil {
			return false
		}
	}
	for kind := 0; kind < 7; kind++ {
		chunk := scalarTaskCacheChunkForTest()
		bad := &chunk.tasks[0]
		var switched *uint32
		switch kind {
		case 0:
			bad.parent = new(stacklessCoroTask)
		case 1:
			bad.flags = 0
		case 2:
			bad.goexit = true
		case 3:
			bad.terminal = stacklessCoroTerminalPanic
		case 4:
			switched = new(uint32)
			*switched = 0xbeef
			bad.context.frame = unsafe.Pointer(switched)
		case 5:
			bad.frameSize = stacklessCoroUncachedFrameLineage
		case 6:
			bad.resuming = true
		}
		lock(&s.lock)
		for i := range chunk.tasks {
			chunk.tasks[i].state = stacklessCoroTaskComplete
			clearStacklessCoroTaskFrame(&chunk.tasks[i])
		}
		cacheStacklessCoroCompletedScalarTaskChunk(&chunk.tasks[3])
		unlock(&s.lock)
		if takeStacklessCoroCompletedScalarTaskChunk() != nil {
			return false
		}
		if kind < 5 && chunk.frames[0] != 0xfeed {
			return false
		}
		if switched != nil && *switched != 0xbeef {
			return false
		}
	}
	return true
}

func StacklessCoroScalarTaskCachePopulationForTest(n int) ([]unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) {
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankStacklessCoro)
	tokens := make([]unsafe.Pointer, n*4)
	for i := 0; i < n; i++ {
		chunk := scalarTaskCacheChunkForTest()
		for j := range chunk.tasks {
			tokens[i*4+j] = unsafe.Pointer(&chunk.tasks[j])
		}
		scalarTaskCacheWholeForTest(s, chunk)
	}
	lock(&stacklessCoroSharedOperations.lock)
	g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young]
	head := unsafe.Pointer(g.groups[g.tops[stacklessCoroScalarTaskCacheClass]].head)
	unlock(&stacklessCoroSharedOperations.lock)
	return tokens, head, tokens[(n-8)*4+2]
}
func StacklessCoroScalarTaskCacheNextForTest(p unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer((*stacklessCoroTask)(p).next)
}
func StacklessCoroScalarTaskCacheFillForTest(n int) (uint32, uintptr) {
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankStacklessCoro)
	for range n {
		scalarTaskCacheWholeForTest(s, scalarTaskCacheChunkForTest())
	}
	lock(&stacklessCoroSharedOperations.lock)
	counted := stacklessCoroCompletedTasks.generations[0].count + stacklessCoroCompletedTasks.generations[1].count
	bytes := stacklessCoroSharedPayloadCacheBytes()
	unlock(&stacklessCoroSharedOperations.lock)
	return counted, bytes
}
func StacklessCoroScalarTaskCacheFillChannelsForTest() uintptr {
	for {
		op := newStacklessCoroChannelOperation()
		lock(&stacklessCoroSharedOperations.lock)
		before := stacklessCoroSharedPayloadCacheBytes()
		cacheStacklessCoroChannelLocked(op)
		after := stacklessCoroSharedPayloadCacheBytes()
		unlock(&stacklessCoroSharedOperations.lock)
		if after == before {
			return after
		}
	}
}
func CheckStacklessCoroScalarTaskCacheConcurrentForTest() bool {
	if raceenabled {
		return true
	}
	probe := new(struct {
		ownership mutex
		active    map[*stacklessCoroScalarTaskChunk]bool
		failed    atomic.Uint32
	})
	lockInit(&probe.ownership, lockRankStacklessCoro)
	probe.active = make(map[*stacklessCoroScalarTaskChunk]bool)
	done := make(chan struct{}, 4)
	for range 4 {
		go func() {
			s := new(stacklessCoroScheduler)
			lockInit(&s.lock, lockRankStacklessCoro)
			for range 1000 {
				chunk := takeStacklessCoroCompletedScalarTaskChunk()
				if chunk == nil {
					chunk = new(stacklessCoroScalarTaskChunk)
				}
				lock(&probe.ownership)
				if probe.active[chunk] {
					probe.failed.Store(1)
				}
				probe.active[chunk] = true
				unlock(&probe.ownership)
				scalarTaskCacheInitializeForTest(chunk)
				lock(&s.lock)
				for i := 0; i < 3; i++ {
					scalarTaskCacheCompleteForTest(s, chunk, i)
				}
				unlock(&s.lock)
				Gosched()
				lock(&probe.ownership)
				delete(probe.active, chunk)
				unlock(&probe.ownership)
				lock(&s.lock)
				scalarTaskCacheCompleteForTest(s, chunk, 3)
				unlock(&s.lock)
			}
			done <- struct{}{}
		}()
	}
	for range 20 {
		GC()
	}
	for range 4 {
		<-done
	}
	return probe.failed.Load() == 0 && StacklessCoroSharedPayloadCacheBytesForTest() <= 16<<20
}
func CheckStacklessCoroScalarTaskCacheRaceNoopForTest() bool {
	chunk := new(stacklessCoroScalarTaskChunk)
	return raceenabled && takeStacklessCoroCompletedScalarTaskChunk() == nil && stacklessCoroScalarTaskFrameMarker(&chunk.tasks[0], unsafe.Pointer(&chunk.frames[0])) == 0
}
func StacklessCoroScalarTaskCacheFatalForTest(mode string) {
	if mode == "origin" {
		chunk := new(stacklessCoroScalarTaskChunk)
		stacklessCoroScalarTaskFrameMarker(&chunk.tasks[0], unsafe.Pointer(&chunk.tasks[1]))
		return
	}
	if mode == "admission" {
		task := new(stacklessCoroTask)
		task.frameSize = stacklessCoroScalarTaskCacheFirst
		task.state = stacklessCoroTaskRunning
		cacheStacklessCoroCompletedScalarTaskChunk(task)
		return
	}
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankStacklessCoro)
	scalarTaskCacheWholeForTest(s, scalarTaskCacheChunkForTest())
	lock(&stacklessCoroSharedOperations.lock)
	g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young]
	switch mode {
	case "index":
		g.tops[4] = stacklessCoroTaskCacheGroups + 1
	case "head":
		g.groups[g.tops[4]].head = nil
	}
	unlock(&stacklessCoroSharedOperations.lock)
	takeStacklessCoroCompletedScalarTaskChunk()
}
