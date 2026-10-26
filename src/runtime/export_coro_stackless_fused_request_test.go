// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && !coropullcompare && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroLocalFusedRequestForTest(chunkCompatible,
	fusedParent, runnable, locked bool) bool {
	s := newStacklessCoroTestScheduler()
	lockInit(&s.lock, lockRankStacklessCoro)
	task := &s.root
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	task.context.scheduler = s
	if fusedParent {
		task.flags = stacklessCoroTaskFusedFrames
	}
	var sibling *stacklessCoroTask
	if runnable {
		sibling = new(stacklessCoroTask)
		sibling.state = stacklessCoroTaskRunnable
		s.head, s.tail = sibling, sibling
		s.runnableState.Store(1)
	}
	want := task.flags | stacklessCoroTaskFusedRequested
	if chunkCompatible {
		want |= stacklessCoroTaskFusedChunk
	}
	if locked {
		lock(&s.lock)
	}
	coroRequestFusedFrame(unsafe.Pointer(&task.context), chunkCompatible)
	if locked {
		unlock(&s.lock)
	}
	return task.flags == want && task.state == stacklessCoroTaskRunning &&
		task.resuming && !task.readyPending && !task.goexit &&
		task.terminal == stacklessCoroTerminalNone &&
		s.head == sibling && s.tail == sibling &&
		s.runnableState.Load() == uint32(bool2int(runnable))
}
