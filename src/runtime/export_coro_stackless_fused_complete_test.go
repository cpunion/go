// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroUncachedFusedCompletionForTest(runnable,
	heterogeneous, fusedParent, shortChunk, chunk, locked bool) bool {
	parentResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionWait
	})
	childResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	s := newStacklessCoroTestScheduler()
	lockInit(&s.lock, lockRankStacklessCoro)
	task := &s.root
	task.resume = childResume
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	task.flags = stacklessCoroTaskFusedFrames
	task.context.scheduler = s
	parent := new(stacklessCoroFusedFrameHeader)
	frame := new(stacklessCoroFusedFrameHeader)
	var resumeFrame *stacklessCoroFusedResumeFrameHeader
	if heterogeneous {
		resumeFrame = new(stacklessCoroFusedResumeFrameHeader)
		frame = &resumeFrame.stacklessCoroFusedFrameHeader
	}
	frame.parent = unsafe.Pointer(parent)
	frame.marker = stacklessCoroFusedFrameDirectFirst
	if chunk {
		frame.marker = stacklessCoroFusedFrameChunkFirst |
			stacklessCoroFusedFrameChunkValidated
	}
	if shortChunk {
		frame.marker |= stacklessCoroFusedFrameShortChunk
	}
	if heterogeneous {
		frame.marker |= stacklessCoroFusedFrameResume
		resumeFrame.resume = stacklessCoroResumeIdentity(parentResume)
	}
	if fusedParent {
		frame.marker |= stacklessCoroFusedFrameParent
		parent.parent = unsafe.Pointer(new(stacklessCoroFusedFrameHeader))
	}
	task.context.frame = unsafe.Pointer(frame)
	var sibling *stacklessCoroTask
	if runnable {
		sibling = new(stacklessCoroTask)
		sibling.state = stacklessCoroTaskRunnable
		s.head, s.tail = sibling, sibling
		s.runnableState.Store(1)
	}
	if locked {
		// No shared cache owner exists. Completion must not acquire this lock;
		// the external subprocess test bounds a regression that would block.
		lock(&s.lock)
	}
	action := coroCompleteFusedFrame(unsafe.Pointer(&task.context))
	if locked {
		unlock(&s.lock)
	}
	wantAction := stacklessCoroActionSwitch
	if runnable {
		wantAction = stacklessCoroActionYield
	}
	wantResume := childResume
	if heterogeneous {
		wantResume = parentResume
	}
	return action == wantAction && task.context.frame == unsafe.Pointer(parent) &&
		stacklessCoroResumeIdentity(task.resume) ==
			stacklessCoroResumeIdentity(wantResume) &&
		task.hasFlag(stacklessCoroTaskFusedFrames) == fusedParent &&
		task.hasFlag(stacklessCoroTaskSwitchPending) == !runnable &&
		task.state == stacklessCoroTaskRunning && task.resuming &&
		frame.parent == nil && frame.owner == nil && frame.marker == 0 &&
		(resumeFrame == nil || resumeFrame.resume == nil) &&
		s.head == sibling && s.tail == sibling &&
		s.freeTasks == nil && s.reservedTasks == nil
}
