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

func completedTaskCacheTypesForTest() [4]*_type {
	return [4]*_type{abi.TypeFor[completedTaskNoPointers](), abi.TypeFor[completedTaskFirstPointer](),
		abi.TypeFor[completedTaskLastPointer](), abi.TypeFor[completedTaskBothPointers]()}
}

func completedTaskCacheNewForTest(typ *_type, bitmap int) *stacklessCoroTask {
	task := (*stacklessCoroTask)(newobject(typ))
	task.frameSize = stacklessCoroTypedTaskCacheFirst - uint16(bitmap)
	task.state = stacklessCoroTaskComplete
	task.flags = stacklessCoroTaskTypedSpawn
	task.context.frame = add(unsafe.Pointer(task), unsafe.Sizeof(*task))
	for i := 0; i < 2; i++ {
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
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankStacklessCoro)
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
				task.parent = new(stacklessCoroTask)
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

func StacklessCoroCompletedTaskCachePayloadForTest() unsafe.Pointer {
	task := completedTaskCacheNewForTest(abi.TypeFor[completedTaskLastPointer](), 2)
	payload := new([128]byte)
	*(*unsafe.Pointer)(add(task.context.frame, 8)) = unsafe.Pointer(payload)
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
	types := completedTaskCacheTypesForTest()
	for worker := range 4 {
		go func() {
			s := new(stacklessCoroScheduler)
			lockInit(&s.lock, lockRankStacklessCoro)
			for i := range 2000 {
				bitmap := (i + worker) % 4
				lock(&s.lock)
				task, marker := takeStacklessCoroCompletedTask(types[bitmap], 16)
				unlock(&s.lock)
				if task == nil {
					task = (*stacklessCoroTask)(newobject(types[bitmap]))
				}
				lock(&probe.ownership)
				if probe.active[task] {
					probe.failed.Add(1)
				}
				probe.active[task] = true
				unlock(&probe.ownership)
				frame := add(unsafe.Pointer(task), unsafe.Sizeof(*task))
				if *(*[2]uintptr)(frame) != [2]uintptr{} {
					probe.failed.Add(1)
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
