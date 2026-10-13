// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroLateRootSignalForTest(spawned, concurrent bool) bool {
	s, task, op := newEarlyReadyStacklessCoroForTest()
	s.waiting(task, false)
	if spawned {
		// Model children that have already completed before the public
		// driver decides whether it needs to transfer to an owner.
		s.lifetime.Or(stacklessCoroLifetimeSpawned)
		s.rootDone = make(chan struct{})
	}
	task.resume = func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	}
	wake := s.wake

	// Split operation completion immediately before its final signal. Once
	// readyLocked publishes the task, another executor can finish the root
	// without waiting for that last producer access to the scheduler.
	scheduler, ready := clearStacklessCoroOperation(op)
	if scheduler != s || ready != task {
		return false
	}
	lock(&s.lock)
	s.readyLocked(task)
	cached := s.recycleOperationLocked(op)
	unlock(&s.lock)
	if !cached {
		cacheStacklessCoroSharedOperation(op)
	}
	s.run(false, false)
	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-start
		for range 64 {
			s.signal()
			Gosched()
		}
		close(done)
	}()
	if concurrent {
		close(start)
	}
	quiescent := finishStacklessCoroRootScheduler(s)
	if !concurrent {
		close(start)
	}
	<-done
	return !quiescent && s.wake == wake && len(wake) != 0
}
