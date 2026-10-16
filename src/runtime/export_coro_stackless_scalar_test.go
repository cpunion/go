// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func newScalarFrameSchedulerForTest() (*stacklessCoroScheduler, stacklessCoroResume) {
	s := &stacklessCoroScheduler{
		cachedFrameTasks: stacklessCoroTaskCacheSize,
		cachedFrameBytes: 4 * stacklessCoroTaskCacheSize,
	}
	lockInit(&s.lock, lockRankLeafRank)
	s.root.context.scheduler = s
	s.root.state = stacklessCoroTaskRunning
	s.root.resuming = true
	resume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	return s, resume
}

func CheckStacklessCoroScalarFramesForTest(size uintptr, count int) (paired int, valid bool) {
	s, resume := newScalarFrameSchedulerForTest()
	frames := make([]unsafe.Pointer, count)
	for i := range frames {
		frame := coroTakeFrame(unsafe.Pointer(&s.root.context), resume, size)
		frames[i] = frame
		if frame != nil {
			if size != 4 || size >= unsafe.Sizeof(uintptr(0)) ||
				*(*uint32)(frame) != 0 || s.reservedTasks.context.frame != frame ||
				s.reservedTasks.flags != 0 {
				return paired, false
			}
			for _, previous := range frames[:i] {
				if previous == frame {
					return paired, false
				}
			}
			*(*uint32)(frame) = uint32(i + 1)
			paired++
		}
	}
	// Interior scalar pointers must retain both their storage and the typed
	// task allocation while the generated frame is the remaining owner.
	marker := new([2]uint64)
	marker[0] = 0x7363616c6172
	s.root.context.frame = unsafe.Pointer(marker)
	s = nil
	GC()
	for i, frame := range frames {
		if frame != nil && *(*uint32)(frame) != uint32(i+1) {
			return paired, false
		}
	}
	if paired != 0 {
		// The first paired reservation owns element zero of the first chunk.
		// Only the scalar pointers above keep this chunk and its parent alive.
		first := frames[stacklessCoroDirectBatchedTaskCount]
		offset := stacklessCoroTaskChunkSize * unsafe.Sizeof(stacklessCoroTask{})
		owner := (*stacklessCoroTask)(unsafe.Add(first, -int(offset)))
		if owner.context.frame != first || owner.parent == nil ||
			owner.parent.context.frame == nil ||
			(*[2]uint64)(owner.parent.context.frame)[0] != 0x7363616c6172 {
			return paired, false
		}
	}
	KeepAlive(frames)
	return paired, true
}

func CheckStacklessCoroScalarFrameMixForTest() bool {
	s, resume := newScalarFrameSchedulerForTest()
	ctx := unsafe.Pointer(&s.root.context)
	for range stacklessCoroDirectBatchedTaskCount {
		if coroTakeFrame(ctx, resume, 4) != nil {
			return false
		}
	}
	if coroTakeFrame(ctx, resume, 4) == nil {
		return false
	}
	value := new(int)
	*value = 37
	pointerFrame := &struct{ value *int }{value}
	lock(&s.lock)
	task := s.newTaskLocked(unsafe.Pointer(pointerFrame), resume, nil)
	unlock(&s.lock)
	if task.flags != 0 || task.context.frame != unsafe.Pointer(pointerFrame) ||
		coroTakeFrame(ctx, resume, unsafe.Sizeof(pointerFrame)) != nil {
		return false
	}
	last := coroTakeFrame(ctx, resume, 4)
	if last == nil || *(*uint32)(last) != 0 ||
		s.batchedTaskCounts>>stacklessCoroFreeBatchedTaskCountShift != 0 {
		return false
	}
	GC()
	return (*struct{ value *int })(task.context.frame).value == value &&
		*(*struct{ value *int })(task.context.frame).value == 37
}

func CheckStacklessCoroDiscardScalarFramesForTest() bool {
	s, resume := newScalarFrameSchedulerForTest()
	ctx := unsafe.Pointer(&s.root.context)
	for range stacklessCoroDirectBatchedTaskCount + 1 {
		coroTakeFrame(ctx, resume, 4)
	}
	var unused []*stacklessCoroTask
	for task := s.freeTasks; task != nil; task = task.next {
		if task.frameSize != stacklessCoroFreeBatchedTask || task.context.frame == nil {
			return false
		}
		unused = append(unused, task)
	}
	if len(unused) != stacklessCoroTaskChunkSize-1 {
		return false
	}
	s.discardFreeBatchedTasks()
	if s.freeTasks != nil ||
		s.overflowTaskCounts&stacklessCoroFreeBatchedTaskPresent != 0 ||
		s.batchedTaskCounts>>stacklessCoroFreeBatchedTaskCountShift != 0 {
		return false
	}
	for _, task := range unused {
		if task.context.frame != nil || task.next != nil ||
			task.flags != stacklessCoroTaskFreeChunk {
			return false
		}
	}
	return true
}
