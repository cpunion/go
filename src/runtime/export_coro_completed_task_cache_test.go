// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
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
			cacheStacklessCoroCompletedTask(task)
			tasks[i] = task
		}
		for round := 0; round < 3; round++ {
			for i := range tasks {
				task, marker := takeStacklessCoroCompletedTask(typ, 16)
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
		for _, kind := range []int{0, 1, 2} {
			task := completedTaskCacheNewForTest(typ, bitmap)
			switch kind {
			case 0:
				task.parent = new(stacklessCoroTask)
			case 1:
				task.flags = 0
			case 2:
				task.goexit = true
			}
			before := *(*[2]uintptr)(task.context.frame)
			frame := task.context.frame
			clearStacklessCoroTaskFrame(task)
			if task.frameSize != stacklessCoroUncachedFrameLineage || *(*[2]uintptr)(frame) != before {
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
