// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

func CheckStacklessCoroDirectLinkLayoutForTest() (taskSize, chunkSize uintptr, is64Bit bool) {
	return unsafe.Sizeof(stacklessCoroTask{}), unsafe.Sizeof(stacklessCoroScalarTaskChunk{}), goarch.PtrSize == 8
}

func CheckStacklessCoroDirectLinkTaggingForTest() bool {
	parent := new(stacklessCoroTask)
	child := new(stacklessCoroTask)
	rec := new(stacklessCoroGoroutine)

	// Nil source must store literal nil, never nil+1.
	parent.setSource(nil)
	if parent.parentOrSource != nil || parent.parent() != nil || parent.source() != nil {
		return false
	}

	// Setting a source record keeps parent() == nil and round-trips source().
	parent.setSource(rec)
	if parent.parentOrSource == nil || parent.parent() != nil || parent.source() != rec {
		return false
	}

	// Clearing source resets parentOrSource to nil.
	parent.setSource(nil)
	if parent.parentOrSource != nil || parent.parent() != nil || parent.source() != nil {
		return false
	}

	// Setting a parent keeps source() == nil and round-trips parent().
	child.setParent(parent)
	if child.parent() != parent || child.source() != nil {
		return false
	}
	child.setParent(nil)
	if child.parentOrSource != nil || child.parent() != nil || child.source() != nil {
		return false
	}
	return true
}

var stacklessCoroDirectLinkGCFreed atomic.Bool

func markStacklessCoroDirectLinkGCFreed(*stacklessCoroGoroutine) {
	stacklessCoroDirectLinkGCFreed.Store(true)
}

type StacklessCoroDirectLinkGCHandle struct {
	task stacklessCoroTask
}

func NewStacklessCoroDirectLinkGCHandleForTest() (*StacklessCoroDirectLinkGCHandle, uint64) {
	stacklessCoroDirectLinkGCFreed.Store(false)
	h := new(StacklessCoroDirectLinkGCHandle)
	r := &stacklessCoroGoroutine{goid: 0x123456789abcdef}
	SetFinalizer(r, markStacklessCoroDirectLinkGCFreed)
	h.task.setSource(r)
	return h, r.goid
}

func StacklessCoroDirectLinkGCFreedForTest() bool {
	return stacklessCoroDirectLinkGCFreed.Load()
}

func (h *StacklessCoroDirectLinkGCHandle) SourceGoid() uint64 {
	if r := h.task.source(); r != nil {
		return r.goid
	}
	return 0
}

func (h *StacklessCoroDirectLinkGCHandle) ClearSource() bool {
	h.task.setSource(nil)
	return h.task.parentOrSource == nil
}

func CheckStacklessCoroDirectLinkLifecycleForTest() bool {
	root := new(stacklessCoroTask)
	child := new(stacklessCoroTask)
	child.setParent(root)
	rec := &stacklessCoroGoroutine{root: root, leaf: root}

	lock(&stacklessCoroGoroutines.lock)
	baseRecords := stacklessCoroGoroutines.records.Load()
	baseUnhosted := stacklessCoroGoroutines.unhosted.Load()
	stacklessCoroInsertGoroutine(rec)
	stacklessCoroGoroutines.unhosted.Add(1)
	matched := root.source() == rec && root.parent() == nil && stacklessCoroGoroutineFor(root) == rec
	unlock(&stacklessCoroGoroutines.lock)
	if !matched {
		return false
	}

	gp := getg()
	stacklessCoroSetLogicalLeaf(child, gp)
	if rec.leaf != child || rec.owner.Load() != gp {
		return false
	}
	stacklessCoroClearLogicalOwner(child)
	if rec.owner.Load() != nil {
		return false
	}

	// Completing a structured child restores r.leaf to parent and keeps root linked.
	stacklessCoroCompleteLogicalFrame(child)
	child.setParent(nil)
	if rec.leaf != root || root.source() != rec || child.parentOrSource != nil {
		return false
	}

	// Completing root unlinks from registry and clears root.parentOrSource.
	stacklessCoroCompleteLogicalFrame(root)
	if root.parentOrSource != nil || root.source() != nil || root.parent() != nil {
		return false
	}

	lock(&stacklessCoroGoroutines.lock)
	ok := stacklessCoroGoroutineFor(root) == nil &&
		stacklessCoroGoroutines.records.Load() == baseRecords &&
		stacklessCoroGoroutines.unhosted.Load() == baseUnhosted
	unlock(&stacklessCoroGoroutines.lock)
	return ok
}

func CheckStacklessCoroDirectLinkRecordRecyclingForTest() string {
	var savedTask, savedPublic *stacklessCoroGoroutine
	lock(&stacklessCoroGoroutines.lock)
	if stacklessCoroGoroutines.taskFreeCount >= stacklessCoroTaskCacheSize && stacklessCoroGoroutines.taskFree != nil {
		savedTask = stacklessCoroGoroutines.taskFree
		stacklessCoroGoroutines.taskFree = savedTask.next
		savedTask.next = nil
		stacklessCoroGoroutines.taskFreeCount--
	}
	if stacklessCoroGoroutines.publicFreeCount >= stacklessCoroWarmExecutorCount && stacklessCoroGoroutines.publicFree != nil {
		savedPublic = stacklessCoroGoroutines.publicFree
		stacklessCoroGoroutines.publicFree = savedPublic.next
		savedPublic.next = nil
		stacklessCoroGoroutines.publicFreeCount--
	}
	unlock(&stacklessCoroGoroutines.lock)

	fail := ""

	// Case 1: unhosted, non-public, owner == nil -> unlinked, source cleared, zeroed, recycled to taskFree.
	if fail == "" {
		task1 := new(stacklessCoroTask)
		rec1 := &stacklessCoroGoroutine{root: task1, leaf: task1, goid: 1001, parentGoid: 1, gopc: 0x1234}
		lock(&stacklessCoroGoroutines.lock)
		baseRecords := stacklessCoroGoroutines.records.Load()
		baseUnhosted := stacklessCoroGoroutines.unhosted.Load()
		baseTaskFreeCount := stacklessCoroGoroutines.taskFreeCount
		stacklessCoroInsertGoroutine(rec1)
		stacklessCoroGoroutines.unhosted.Add(1)
		unlock(&stacklessCoroGoroutines.lock)

		stacklessCoroCompleteLogicalFrame(task1)

		lock(&stacklessCoroGoroutines.lock)
		if task1.parentOrSource != nil || task1.source() != nil {
			fail = "case1: task.parentOrSource was not cleared"
		} else if stacklessCoroGoroutineFor(task1) != nil {
			fail = "case1: record remained in registry bucket"
		} else if stacklessCoroGoroutines.records.Load() != baseRecords || stacklessCoroGoroutines.unhosted.Load() != baseUnhosted {
			fail = "case1: records/unhosted count mismatch"
		} else if stacklessCoroGoroutines.taskFree != rec1 || stacklessCoroGoroutines.taskFreeCount != baseTaskFreeCount+1 {
			fail = "case1: reusable record was not pushed to taskFree"
		} else {
			stacklessCoroGoroutines.taskFree = rec1.next
			rec1.next = nil
			stacklessCoroGoroutines.taskFreeCount--
			if rec1.root != nil || rec1.leaf != nil || rec1.goid != 0 || rec1.parentGoid != 0 || rec1.gopc != 0 || rec1.owner.Load() != nil || rec1.public || rec1.hosted {
				fail = "case1: recycled record was not zeroed"
			}
		}
		unlock(&stacklessCoroGoroutines.lock)
	}

	// Case 2: owner != nil -> unlinked, source cleared, zeroed, NOT pushed to taskFree.
	if fail == "" {
		task2 := new(stacklessCoroTask)
		rec2 := &stacklessCoroGoroutine{root: task2, leaf: task2, goid: 1002}
		rec2.owner.Store(getg())
		lock(&stacklessCoroGoroutines.lock)
		baseRecords := stacklessCoroGoroutines.records.Load()
		baseUnhosted := stacklessCoroGoroutines.unhosted.Load()
		baseTaskFree := stacklessCoroGoroutines.taskFree
		baseTaskFreeCount := stacklessCoroGoroutines.taskFreeCount
		stacklessCoroInsertGoroutine(rec2)
		stacklessCoroGoroutines.unhosted.Add(1)
		unlock(&stacklessCoroGoroutines.lock)

		stacklessCoroCompleteLogicalFrame(task2)

		lock(&stacklessCoroGoroutines.lock)
		if task2.parentOrSource != nil || task2.source() != nil {
			fail = "case2: task.parentOrSource was not cleared"
		} else if stacklessCoroGoroutineFor(task2) != nil {
			fail = "case2: record remained in registry bucket"
		} else if stacklessCoroGoroutines.records.Load() != baseRecords || stacklessCoroGoroutines.unhosted.Load() != baseUnhosted {
			fail = "case2: records/unhosted count mismatch"
		} else if stacklessCoroGoroutines.taskFree != baseTaskFree || stacklessCoroGoroutines.taskFreeCount != baseTaskFreeCount {
			fail = "case2: record with non-nil owner was wrongly pushed to taskFree"
		} else if rec2.root != nil || rec2.leaf != nil || rec2.goid != 0 || rec2.owner.Load() != nil {
			fail = "case2: completed record was not zeroed"
		}
		unlock(&stacklessCoroGoroutines.lock)
	}

	// Case 3: hosted == true -> unlinked, source cleared, zeroed, NOT pushed to taskFree.
	if fail == "" {
		task3 := new(stacklessCoroTask)
		rec3 := &stacklessCoroGoroutine{root: task3, leaf: task3, goid: 1003, hosted: true, hostG: getg()}
		lock(&stacklessCoroGoroutines.lock)
		baseRecords := stacklessCoroGoroutines.records.Load()
		baseUnhosted := stacklessCoroGoroutines.unhosted.Load()
		baseTaskFree := stacklessCoroGoroutines.taskFree
		baseTaskFreeCount := stacklessCoroGoroutines.taskFreeCount
		stacklessCoroInsertGoroutine(rec3)
		unlock(&stacklessCoroGoroutines.lock)

		stacklessCoroCompleteLogicalFrame(task3)

		lock(&stacklessCoroGoroutines.lock)
		if task3.parentOrSource != nil || task3.source() != nil {
			fail = "case3: task.parentOrSource was not cleared"
		} else if stacklessCoroGoroutineFor(task3) != nil {
			fail = "case3: record remained in registry bucket"
		} else if stacklessCoroGoroutines.records.Load() != baseRecords || stacklessCoroGoroutines.unhosted.Load() != baseUnhosted {
			fail = "case3: records/unhosted count mismatch"
		} else if stacklessCoroGoroutines.taskFree != baseTaskFree || stacklessCoroGoroutines.taskFreeCount != baseTaskFreeCount {
			fail = "case3: hosted record was wrongly pushed to taskFree"
		} else if rec3.root != nil || rec3.leaf != nil || rec3.goid != 0 || rec3.hosted || rec3.hostG != nil {
			fail = "case3: completed hosted record was not zeroed"
		}
		unlock(&stacklessCoroGoroutines.lock)
	}

	// Case 4: public == true -> unlinked from both buckets, source cleared, recycled to publicFree only.
	if fail == "" {
		gp := getg()
		task4 := new(stacklessCoroTask)
		pcs := make([]uintptr, 2, maxCPUProfStack)
		pcs[0], pcs[1] = 0x1111, 0x2222
		rec4 := &stacklessCoroGoroutine{root: task4, leaf: task4, goid: 1004, public: true, publicG: gp, profileCaller: pcs}
		lock(&stacklessCoroGoroutines.lock)
		baseRecords := stacklessCoroGoroutines.records.Load()
		baseUnhosted := stacklessCoroGoroutines.unhosted.Load()
		baseTaskFree := stacklessCoroGoroutines.taskFree
		baseTaskFreeCount := stacklessCoroGoroutines.taskFreeCount
		basePublicFreeCount := stacklessCoroGoroutines.publicFreeCount
		stacklessCoroInsertGoroutine(rec4)
		pb := stacklessCoroPublicGoroutineBucket(gp)
		rec4.publicNext, *pb = *pb, rec4
		unlock(&stacklessCoroGoroutines.lock)

		stacklessCoroCompleteLogicalFrame(task4)

		lock(&stacklessCoroGoroutines.lock)
		inPublicBucket := false
		for cur := *stacklessCoroPublicGoroutineBucket(gp); cur != nil; cur = cur.publicNext {
			if cur == rec4 {
				inPublicBucket = true
				break
			}
		}
		if task4.parentOrSource != nil || task4.source() != nil {
			fail = "case4: task.parentOrSource was not cleared"
		} else if stacklessCoroGoroutineFor(task4) != nil || inPublicBucket {
			fail = "case4: public record remained in registry or public bucket"
		} else if stacklessCoroGoroutines.records.Load() != baseRecords || stacklessCoroGoroutines.unhosted.Load() != baseUnhosted {
			fail = "case4: records/unhosted count mismatch"
		} else if stacklessCoroGoroutines.taskFree != baseTaskFree || stacklessCoroGoroutines.taskFreeCount != baseTaskFreeCount {
			fail = "case4: public record was wrongly pushed to taskFree"
		} else if stacklessCoroGoroutines.publicFree != rec4 || stacklessCoroGoroutines.publicFreeCount != basePublicFreeCount+1 {
			fail = "case4: public record was not pushed to publicFree"
		} else {
			stacklessCoroGoroutines.publicFree = rec4.next
			rec4.next = nil
			stacklessCoroGoroutines.publicFreeCount--
			if rec4.root != nil || rec4.leaf != nil || rec4.goid != 0 || rec4.public || rec4.publicG != nil ||
				len(rec4.profileCaller) != 0 || cap(rec4.profileCaller) != maxCPUProfStack ||
				rec4.profileCaller[:2][0] != 0 || rec4.profileCaller[:2][1] != 0 {
				fail = "case4: recycled public record was not cleared properly"
			}
		}
		unlock(&stacklessCoroGoroutines.lock)
	}

	lock(&stacklessCoroGoroutines.lock)
	if savedTask != nil {
		savedTask.next = stacklessCoroGoroutines.taskFree
		stacklessCoroGoroutines.taskFree = savedTask
		stacklessCoroGoroutines.taskFreeCount++
	}
	if savedPublic != nil {
		savedPublic.next = stacklessCoroGoroutines.publicFree
		stacklessCoroGoroutines.publicFree = savedPublic
		stacklessCoroGoroutines.publicFreeCount++
	}
	unlock(&stacklessCoroGoroutines.lock)
	return fail
}
