// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && !coropullcompare && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroLocalFusedAwaitForTest(allocation uint8,
	short, runnable, locked bool, mode string) bool {
	parentResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionWait
	})
	childResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	parent := new(stacklessCoroFusedResumeFrameHeader)
	parent.parent = unsafe.Pointer(new(stacklessCoroFusedFrameHeader))
	parent.marker = allocation | stacklessCoroFusedFrameChunkValidated |
		stacklessCoroFusedFrameParent | stacklessCoroFusedFrameResume
	parent.resume = stacklessCoroResumeIdentity(childResume)
	if short {
		parent.marker |= stacklessCoroFusedFrameShortChunk
	}
	child := new(stacklessCoroFusedResumeFrameHeader)
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankLeafRank)
	task := &s.root
	task.resume = parentResume
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	task.flags = stacklessCoroTaskFusedFrames | stacklessCoroTaskFusedPending |
		stacklessCoroTaskFusedChunk
	task.context.scheduler, task.context.frame = s, unsafe.Pointer(parent)
	// Unrelated shared lists must survive even when the child is uncached.
	free, reserved := new(stacklessCoroTask), new(stacklessCoroTask)
	s.freeTasks, s.reservedTasks = free, reserved
	var owner *stacklessCoroTask
	var sibling *stacklessCoroTask
	if runnable {
		sibling = &stacklessCoroTask{state: stacklessCoroTaskRunnable}
		s.head, s.tail = sibling, sibling
		s.runnableState.Store(1)
	}
	switch mode {
	case "pending-only", "cached-child":
		task.flags &^= stacklessCoroTaskFusedChunk
		if mode == "cached-child" {
			owner = &stacklessCoroTask{
				parent: task, resume: childResume, flags: stacklessCoroTaskCacheFrame,
				context: stacklessCoroContext{frame: unsafe.Pointer(child)},
			}
			reserved.next = owner
		}
	case "owned-parent":
		parent.marker &^= stacklessCoroFusedFrameAllocationMask
		parent.owner = &stacklessCoroTask{flags: stacklessCoroTaskCacheFrame}
		parent.owner.context.frame = unsafe.Pointer(parent)
	case "zero-allocation":
		parent.marker &^= stacklessCoroFusedFrameAllocationMask
	case "unlinked-parent":
		parent.parent = nil
	case "unvalidated-parent":
		parent.marker &^= stacklessCoroFusedFrameChunkValidated
	case "same-resume":
		childResume = parentResume
	case "requested":
		task.flags |= stacklessCoroTaskFusedRequested
	case "missing-pending":
		task.flags &^= stacklessCoroTaskFusedPending
	case "waiting":
		task.state = stacklessCoroTaskWaiting
	case "not-resuming":
		task.resuming = false
	case "ready":
		task.readyPending = true
	case "switch":
		task.flags |= stacklessCoroTaskSwitchPending
	case "panic":
		task.terminal = stacklessCoroTerminalPanic
	case "goexit":
		task.goexit = true
	case "nil-resume":
		task.resume = nil
	case "child-parent":
		child.parent = unsafe.Pointer(parent)
	case "child-owner":
		child.owner = new(stacklessCoroTask)
	case "child-marker":
		child.marker = 1
	case "child-resume":
		child.resume = stacklessCoroResumeIdentity(parentResume)
	case "invalid-marker":
		parent.marker |= stacklessCoroFusedFrameAllocationMask
	}
	wantParent := *parent
	wantMarker := parent.marker&(stacklessCoroFusedFrameChunkValidated|
		stacklessCoroFusedFrameShortChunk) | stacklessCoroFusedFrameParent
	if mode != "same-resume" {
		wantMarker |= stacklessCoroFusedFrameResume
	}
	switch {
	case mode == "cached-child":
		wantMarker = stacklessCoroFusedFrameParent | stacklessCoroFusedFrameResume
	case mode == "pending-only":
		wantMarker = stacklessCoroFusedFrameParent | stacklessCoroFusedFrameResume |
			stacklessCoroFusedFrameDirectFirst
	default:
		current := parent.marker & stacklessCoroFusedFrameAllocationMask
		next := current + 1
		if current == stacklessCoroFusedFrameDirectLast ||
			current == stacklessCoroFusedFrameChunkLastFor(parent.marker) {
			next = stacklessCoroFusedFrameChunkFirst
		}
		wantMarker |= next
	}
	if locked {
		lock(&s.lock)
	}
	action := coroAwaitFusedFrame(unsafe.Pointer(&task.context), unsafe.Pointer(child), childResume)
	if locked {
		unlock(&s.lock)
	}
	wantAction := stacklessCoroActionSwitch
	wantFlags := stacklessCoroTaskFusedFrames | stacklessCoroTaskSwitchPending
	if runnable {
		wantAction, wantFlags = stacklessCoroActionYield, stacklessCoroTaskFusedFrames
	}
	if action != wantAction || task.flags != wantFlags ||
		task.context.frame != unsafe.Pointer(child) ||
		stacklessCoroResumeIdentity(task.resume) != stacklessCoroResumeIdentity(childResume) ||
		child.parent != unsafe.Pointer(parent) || child.owner != owner || child.marker != wantMarker ||
		(mode == "same-resume" && child.resume != nil) ||
		(mode != "same-resume" && child.resume != stacklessCoroResumeIdentity(parentResume)) ||
		*parent != wantParent || s.freeTasks != free || s.reservedTasks != reserved ||
		reserved.next != nil || (owner != nil && (owner.parent != nil || owner.next != nil)) ||
		s.head != sibling || s.tail != sibling ||
		s.runnableState.Load() != uint32(bool2int(runnable)) {
		return false
	}
	// Check that completion can recover the exact parent and resume identity,
	// including its own saved grandparent resume, without leaving GC links.
	restoreStacklessCoroFusedParent(&task.context, task, &child.stacklessCoroFusedFrameHeader)
	return task.context.frame == unsafe.Pointer(parent) && *parent == wantParent &&
		stacklessCoroResumeIdentity(task.resume) == stacklessCoroResumeIdentity(parentResume) &&
		child.parent == nil && child.owner == nil && child.marker == 0 && child.resume == nil
}
