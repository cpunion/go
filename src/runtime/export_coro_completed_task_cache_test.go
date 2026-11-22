// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

// Lock-ranking records the mutex address while the test publishes completed
// tasks. Put the fixture on the heap so stack growth cannot move the lock.
//
//go:noinline
func newCompletedTaskCacheTestScheduler() *stacklessCoroScheduler {
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankStacklessCoro)
	return s
}

type completedTaskNoPointers struct {
	task  stacklessCoroTask
	frame [2]uintptr
}
type completedTaskFirstPointer struct {
	task  stacklessCoroTask
	frame struct {
		p unsafe.Pointer
		n uintptr
	}
}
type completedTaskLastPointer struct {
	task  stacklessCoroTask
	frame struct {
		n uintptr
		p unsafe.Pointer
	}
}
type completedTaskBothPointers struct {
	task  stacklessCoroTask
	frame [2]unsafe.Pointer
}

type completedTaskWide[A, B, C, D any] struct {
	task  stacklessCoroTask
	frame struct {
		a A
		b B
		c C
		d D
	}
}

func completedTaskCacheTypesForTest() [4]*_type {
	return [4]*_type{abi.TypeFor[completedTaskNoPointers](), abi.TypeFor[completedTaskFirstPointer](),
		abi.TypeFor[completedTaskLastPointer](), abi.TypeFor[completedTaskBothPointers]()}
}

func completedTaskWideCacheTypesForTest() [4]*_type {
	type ptr = unsafe.Pointer
	return [4]*_type{
		abi.TypeFor[completedTaskWide[ptr, uintptr, uintptr, uintptr]](),
		abi.TypeFor[completedTaskWide[uintptr, uintptr, ptr, uintptr]](),
		abi.TypeFor[completedTaskWide[uintptr, uintptr, uintptr, ptr]](),
		abi.TypeFor[completedTaskWide[ptr, ptr, ptr, ptr]](),
	}
}

func completedTaskCacheNewForTest(typ *_type, bitmap int) *stacklessCoroTask {
	task := (*stacklessCoroTask)(newobject(typ))
	size := typ.Size_ - unsafe.Sizeof(*task)
	first := stacklessCoroTypedTaskCacheFirst
	if size == 32 {
		first = stacklessCoroWideTaskCacheFirst
	}
	task.frameSize = first - uint16(bitmap)
	task.state = stacklessCoroTaskComplete
	task.flags = stacklessCoroTaskTypedSpawn
	task.context.frame = add(unsafe.Pointer(task), unsafe.Sizeof(*task))
	for i := range int(size / unsafe.Sizeof(uintptr(0))) {
		p := add(task.context.frame, uintptr(i)*unsafe.Sizeof(uintptr(0)))
		if bitmap&(1<<i) != 0 {
			*(*unsafe.Pointer)(p) = unsafe.Pointer(new(uint64))
		} else {
			*(*uintptr)(p) = 0xfeed
		}
	}
	return task
}

func CheckStacklessCoroCompletedTaskCacheForTest() bool {
	if raceenabled {
		return true
	}
	s := newCompletedTaskCacheTestScheduler()
	for bitmap, typ := range completedTaskCacheTypesForTest() {
		if !validStacklessCoroTaskFrameType(typ, 16) || int(*typ.GCData>>6) != bitmap {
			return false
		}
		var tasks [70]*stacklessCoroTask
		for i := range tasks {
			task := completedTaskCacheNewForTest(typ, bitmap)
			clearStacklessCoroTaskFrame(task)
			payload := (*[2]uintptr)(add(unsafe.Pointer(task), unsafe.Sizeof(*task)))
			if *payload != [2]uintptr{} {
				return false
			}
			lock(&s.lock)
			s.recycleTaskLocked(task)
			unlock(&s.lock)
			tasks[i] = task
		}
		for round := 0; round < 3; round++ {
			for i := range tasks {
				lock(&s.lock)
				task, marker := takeStacklessCoroCompletedTask(typ, 16)
				unlock(&s.lock)
				if task == nil || task.next != nil || task.frameSize != marker || marker != stacklessCoroTypedTaskCacheFirst-uint16(bitmap) {
					return false
				}
				tasks[i] = task
			}
			for _, task := range tasks {
				task.state = stacklessCoroTaskComplete
				cacheStacklessCoroCompletedTask(task)
			}
		}
		for _, kind := range []int{0, 1, 2, 3} {
			task := completedTaskCacheNewForTest(typ, bitmap)
			embedded := add(unsafe.Pointer(task), unsafe.Sizeof(*task))
			embeddedBefore := *(*[2]uintptr)(embedded)
			switch kind {
			case 0:
				task.setParent(new(stacklessCoroTask))
			case 1:
				task.flags = 0
			case 2:
				task.goexit = true
			case 3:
				task.context.frame = unsafe.Pointer(new([2]uintptr))
			}
			before := *(*[2]uintptr)(task.context.frame)
			frame := task.context.frame
			clearStacklessCoroTaskFrame(task)
			if task.frameSize != stacklessCoroUncachedFrameLineage || *(*[2]uintptr)(frame) != before || *(*[2]uintptr)(embedded) != embeddedBefore {
				return false
			}
		}
	}
	return true
}

func CheckStacklessCoroWideCompletedTaskCacheForTest() bool {
	// Keep all layouts live in the shared cache together. The first wide
	// type has the same bitmap as a narrow type; the others cover both
	// bits in the second GC byte.
	types := completedTaskWideCacheTypesForTest()
	bitmaps := [...]int{1, 4, 8, 15}
	narrowType := abi.TypeFor[completedTaskFirstPointer]()
	narrow := completedTaskCacheNewForTest(narrowType, 1)
	clearStacklessCoroTaskFrame(narrow)
	cacheStacklessCoroCompletedTask(narrow)
	before := StacklessCoroSharedPayloadCacheBytesForTest()
	var tasks [len(types)]*stacklessCoroTask
	for i, typ := range types {
		task := completedTaskCacheNewForTest(typ, bitmaps[i])
		if !validStacklessCoroTaskFrameType(typ, 32) {
			return false
		}
		frame := (*[4]uintptr)(task.context.frame)
		clearStacklessCoroTaskFrame(task)
		if *frame != [4]uintptr{} {
			return false
		}
		cacheStacklessCoroCompletedTask(task)
		tasks[i] = task
	}
	if StacklessCoroSharedPayloadCacheBytesForTest()-before != uintptr(len(tasks))*(unsafe.Sizeof(*narrow)+32) {
		return false
	}
	for i, typ := range types {
		task, marker := takeStacklessCoroCompletedTask(typ, 32)
		if task != tasks[i] || marker != stacklessCoroWideTaskCacheFirst-uint16(bitmaps[i]) || task.next != nil {
			return false
		}
	}
	task, marker := takeStacklessCoroCompletedTask(narrowType, 16)
	return task == narrow && marker == stacklessCoroTypedTaskCacheFirst-1
}

func StacklessCoroCompletedTaskCacheTokenForTest() unsafe.Pointer {
	typ := abi.TypeFor[completedTaskLastPointer]()
	task := completedTaskCacheNewForTest(typ, 2)
	clearStacklessCoroTaskFrame(task)
	cacheStacklessCoroCompletedTask(task)
	return unsafe.Pointer(task)
}

func StacklessCoroSharedPayloadCacheBytesForTest() uintptr {
	lock(&stacklessCoroSharedOperations.lock)
	n := stacklessCoroSharedPayloadCacheBytes()
	unlock(&stacklessCoroSharedOperations.lock)
	return n
}

func StacklessCoroCompletedTaskCachePayloadForTest(wide bool) unsafe.Pointer {
	typ, bitmap, offset := abi.TypeFor[completedTaskLastPointer](), 2, uintptr(8)
	if wide {
		typ, bitmap, offset = completedTaskWideCacheTypesForTest()[2], 8, 24
	}
	task := completedTaskCacheNewForTest(typ, bitmap)
	payload := new([128]byte)
	*(*unsafe.Pointer)(add(task.context.frame, offset)) = unsafe.Pointer(payload)
	clearStacklessCoroTaskFrame(task)
	cacheStacklessCoroCompletedTask(task)
	return unsafe.Pointer(payload)
}

func StacklessCoroCompletedTaskCacheFillForTest(count int) (uint32, uintptr) {
	typ := abi.TypeFor[completedTaskLastPointer]()
	for range count {
		task := completedTaskCacheNewForTest(typ, 2)
		clearStacklessCoroTaskFrame(task)
		cacheStacklessCoroCompletedTask(task)
	}
	lock(&stacklessCoroSharedOperations.lock)
	counted := stacklessCoroCompletedTasks.generations[0].count + stacklessCoroCompletedTasks.generations[1].count
	bytes := stacklessCoroSharedPayloadCacheBytes()
	unlock(&stacklessCoroSharedOperations.lock)
	return counted, bytes
}

func CheckStacklessCoroCompletedTaskCacheConcurrentForTest() bool {
	probe := new(struct {
		ownership mutex
		active    map[*stacklessCoroTask]bool
		failed    atomic.Uint32
	})
	lockInit(&probe.ownership, lockRankStacklessCoro)
	probe.active = make(map[*stacklessCoroTask]bool)
	done := make(chan struct{}, 4)
	narrow := completedTaskCacheTypesForTest()
	wide := completedTaskWideCacheTypesForTest()
	types := new([8]*_type)
	copy(types[:4], narrow[:])
	copy(types[4:], wide[:])
	for worker := range 4 {
		go func() {
			s := newCompletedTaskCacheTestScheduler()
			for i := range 2000 {
				typ := types[(i+worker)%len(types)]
				size := typ.Size_ - unsafe.Sizeof(stacklessCoroTask{})
				lock(&s.lock)
				task, marker := takeStacklessCoroCompletedTask(typ, size)
				unlock(&s.lock)
				if task == nil {
					task = (*stacklessCoroTask)(newobject(typ))
				}
				lock(&probe.ownership)
				if probe.active[task] {
					probe.failed.Add(1)
				}
				probe.active[task] = true
				unlock(&probe.ownership)
				frame := add(unsafe.Pointer(task), unsafe.Sizeof(*task))
				for _, word := range unsafe.Slice((*uintptr)(frame), int(size/unsafe.Sizeof(uintptr(0)))) {
					if word != 0 {
						probe.failed.Add(1)
						break
					}
				}
				Gosched()
				task.context.frame = frame
				task.frameSize = marker
				task.flags = stacklessCoroTaskTypedSpawn
				task.state = stacklessCoroTaskComplete
				clearStacklessCoroTaskFrame(task)
				lock(&probe.ownership)
				delete(probe.active, task)
				unlock(&probe.ownership)
				lock(&s.lock)
				s.recycleTaskLocked(task)
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
	return probe.failed.Load() == 0 && len(probe.active) == 0 && StacklessCoroSharedPayloadCacheBytesForTest() <= 16<<20
}

func CheckStacklessCoroCompletedTaskCacheRaceNoopForTest() bool {
	if !raceenabled {
		return false
	}
	task, marker := takeStacklessCoroCompletedTask(abi.TypeFor[completedTaskLastPointer](), 16)
	return task == nil && marker == stacklessCoroUncachedFrameLineage
}

func StacklessCoroCompletedTaskCacheFatalForTest(mode string) {
	typ := abi.TypeFor[completedTaskLastPointer]()
	task := completedTaskCacheNewForTest(typ, 2)
	clearStacklessCoroTaskFrame(task)
	if mode == "admission" {
		task.state = stacklessCoroTaskRunning
		cacheStacklessCoroCompletedTask(task)
		return
	}
	cacheStacklessCoroCompletedTask(task)
	lock(&stacklessCoroSharedOperations.lock)
	g := &stacklessCoroCompletedTasks.generations[stacklessCoroChannelCache.young]
	switch mode {
	case "index":
		g.tops[2] = stacklessCoroTaskCacheGroups + 1
	case "head":
		g.groups[g.tops[2]].head = nil
	case "bitmap":
		g.groups[g.tops[2]].head.frameSize = stacklessCoroTypedTaskCacheFirst
	}
	unlock(&stacklessCoroSharedOperations.lock)
	takeStacklessCoroCompletedTask(typ, 16)
}
