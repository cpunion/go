// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

// A host belongs to one logical goroutine, including all of its structured
// children. Once an opaque call needs an ordinary Go stack, subsequent resumes
// stay on that Go goroutine. Independent go statements acquire separate hosts.
// The public logical root uses its original caller rather than a worker G.
type stacklessCoroHost struct {
	scheduler    *stacklessCoroScheduler
	root         *stacklessCoroTask
	head, tail   *stacklessCoroTask
	wake         chan struct{}
	pending      *stacklessCoroTask
	call         func()
	deferred     func(any, bool, bool)
	pendingReady bool
	done         bool
	started      bool
}

func (s *stacklessCoroScheduler) taskHostLocked(task *stacklessCoroTask) *stacklessCoroHost {
	if s.hosts == nil {
		return nil
	}
	for task.parent != nil {
		task = task.parent
	}
	return s.hosts[task]
}

func (s *stacklessCoroScheduler) taskHostCurrentLocked(task *stacklessCoroTask) bool {
	h := s.taskHostLocked(task)
	state := getg().stacklessCoro
	return h == nil || state != nil && state.host == h
}

func (h *stacklessCoroHost) notifyLocked() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *stacklessCoroHost) readyLocked(task *stacklessCoroTask) {
	if h.done || task.next != nil {
		throw("runtime: invalid stackless coroutine host publication")
	}
	if h.tail == nil {
		h.head = task
	} else {
		h.tail.next = task
	}
	h.tail = task
	h.notifyLocked()
}

func (h *stacklessCoroHost) takeLocked() *stacklessCoroTask {
	task := h.head
	if task == nil {
		return nil
	}
	h.head = task.next
	if h.head == nil {
		h.tail = nil
	}
	task.next = nil
	return task
}

// coroHostCall is a handoff, not an inline invocation. The initiating resume
// must return its action before any Go code in call can execute. That barrier
// also protects the parent that is still returning Wait on another executor.
func coroHostCall(ctx unsafe.Pointer, call func()) uint8 {
	if call == nil {
		throw("runtime: nil stackless coroutine host call")
	}
	return prepareStacklessCoroHostCall(ctx, call, nil)
}

// A compiler-generated ordinary thunk registers the frozen source target
// directly with defer, then injects the supplied panic/Goexit. An extra wrapper
// around the source target would incorrectly remove its direct recover right.
func coroHostDefer(ctx unsafe.Pointer, call func(any, bool, bool)) uint8 {
	if call == nil {
		throw("runtime: nil stackless coroutine host defer")
	}
	return prepareStacklessCoroHostCall(ctx, nil, call)
}

func prepareStacklessCoroHostCall(ctx unsafe.Pointer, call func(), deferred func(any, bool, bool)) uint8 {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine host call")
	}
	s := context.scheduler
	s.prepareReplacementExecutors()
	// A different task may have won executor preparation. rootDone must have
	// been published before a public host borrows it as its buffered doorbell.
	for s.executorState.Load() == stacklessCoroExecutorStatePreparing {
		Gosched()
	}
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming || task.readyPending ||
		(deferred == nil && (task.terminal != stacklessCoroTerminalNone || task.goexit)) {
		unlock(&s.lock)
		throw("runtime: stackless coroutine host call outside resume")
	}
	root := task
	for root.parent != nil {
		root = root.parent
	}
	if s.hosts == nil {
		s.hosts = make(map[*stacklessCoroTask]*stacklessCoroHost)
	}
	h := s.hosts[root]
	start := false
	if h == nil {
		h = &stacklessCoroHost{scheduler: s, root: root}
		s.lifetime.Or(stacklessCoroLifetimeHostStarted)
		if root == &s.root {
			h.wake = s.rootDone
			s.lifetime.Or(stacklessCoroLifetimePublicHost)
		} else {
			h.wake = make(chan struct{}, 1)
			start = true
		}
		s.hosts[root] = h
		// A pull comparison retains its active leaf in head instead of an
		// ordinary queue. That leaf now belongs exclusively to this host.
		if stacklessCoroIsPullComparison(s) && s.head == task {
			s.head = nil
		}
	}
	if h.done || h.pending != nil || h.call != nil || h.deferred != nil {
		unlock(&s.lock)
		throw("runtime: overlapping stackless coroutine host calls")
	}
	s.initialRootPrivate = false
	h.pending = task
	h.call = call
	h.deferred = deferred
	h.pendingReady = false
	task.state = stacklessCoroTaskWaiting
	unlock(&s.lock)
	if start {
		go h.run()
	}
	return stacklessCoroActionHost
}

func (s *stacklessCoroScheduler) hostHandoff(task *stacklessCoroTask) {
	lock(&s.lock)
	h := s.taskHostLocked(task)
	if h == nil || h.pending != task || h.pendingReady ||
		task.state != stacklessCoroTaskWaiting || !task.resuming || task.readyPending {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine host handoff")
	}
	task.resuming = false
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	h.pendingReady = true
	h.notifyLocked()
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) completeHostLocked(task *stacklessCoroTask) {
	h := s.hosts[task]
	if h == nil {
		return
	}
	if h.pending != nil || h.head != nil || h.tail != nil || h.done {
		throw("runtime: stackless coroutine host completed with active work")
	}
	// Set done and remove the identity before an independent task is recycled.
	h.done = true
	delete(s.hosts, task)
	h.notifyLocked()
}

func (s *stacklessCoroScheduler) waitPublicRoot() {
	for !s.rootComplete() {
		lock(&s.lock)
		h := s.hosts[&s.root]
		start := h != nil && !h.started
		unlock(&s.lock)
		if start {
			h.run()
		} else {
			<-s.rootDone
		}
	}
}

func (h *stacklessCoroHost) run() {
	s := h.scheduler
	lock(&s.lock)
	if h.started {
		unlock(&s.lock)
		throw("runtime: duplicate stackless coroutine host driver")
	}
	h.started = true
	unlock(&s.lock)
	scope := enterStacklessCoroRunScope()
	previous := scope.state.host
	scope.state.host = h
	defer func() {
		scope.state.host = previous
		scope.leave()
	}()
	h.driveThroughGoexit()
}

// Physical Goexit cannot be recovered. Each new Goexit unwinds to a fresh
// driver activation, which runs the remaining logical cleanup on the same G.
func (h *stacklessCoroHost) driveThroughGoexit() {
	defer func() {
		p := getg()._panic
		if p == nil || !p.goexit {
			return
		}
		lock(&h.scheduler.lock)
		done := h.done
		unlock(&h.scheduler.lock)
		if !done {
			h.driveThroughGoexit()
		}
		if h.root == &h.scheduler.root {
			// Reproduce a remaining logical panic over the pending physical
			// Goexit. Consume it once even when several Goexits are unwinding.
			s := h.scheduler
			lock(&s.lock)
			value, panicking := s.terminalValues[h.root]
			if h.root.terminal == stacklessCoroTerminalPanic && panicking {
				h.root.terminal = stacklessCoroTerminalNone
				delete(s.terminalValues, h.root)
				unlock(&s.lock)
				panic(value)
			}
			unlock(&s.lock)
		}
	}()
	h.drive()
}

func (h *stacklessCoroHost) drive() {
	s := h.scheduler
	for {
		lock(&s.lock)
		if h.done {
			unlock(&s.lock)
			return
		}
		if h.pendingReady {
			task, call, deferred := h.pending, h.call, h.deferred
			h.pending, h.call, h.deferred, h.pendingReady = nil, nil, nil, false
			if task.state != stacklessCoroTaskWaiting || task.resuming || task.readyPending {
				unlock(&s.lock)
				throw("runtime: invalid stackless coroutine host invocation")
			}
			task.state, task.resuming = stacklessCoroTaskRunning, true
			unlock(&s.lock)
			if raceenabled {
				raceacquire(unsafe.Pointer(task))
			}
			h.invoke(task, call, deferred)
			continue
		}
		ready := h.head != nil
		unlock(&s.lock)
		if ready {
			s.runTasks(false, false, false, false)
		} else {
			<-h.wake
		}
	}
}

func (h *stacklessCoroHost) invoke(task *stacklessCoroTask, call func(), deferred func(any, bool, bool)) {
	s := h.scheduler
	state := getg().stacklessCoro
	oldDefer := state.deferTask
	state.deferTask = nil
	state.ordinaryDepth++
	returned := false
	defer func() {
		var value any
		panicking, exiting := false, false
		if !returned {
			p := getg()._panic
			if p != nil && !p.goexit {
				value, panicking = recover(), true
			} else {
				exiting = true
			}
		}
		state.ordinaryDepth--
		state.deferTask = oldDefer
		if panicking {
			recordStacklessCoroPanic(s, task, value, true)
		}
		lock(&s.lock)
		if task.state != stacklessCoroTaskRunning || !task.resuming || task.readyPending {
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine host return")
		}
		if exiting {
			task.terminal = stacklessCoroTerminalNone
			delete(s.terminalValues, task)
			task.goexit = true
		} else if returned && deferred != nil {
			// An injected incoming panic returns normally only when the
			// direct source defer recovered it. Goexit remains sticky.
			task.terminal = stacklessCoroTerminalNone
			delete(s.terminalValues, task)
		}
		if task.terminal != stacklessCoroTerminalNone || task.goexit {
			s.discardFusedFramesLocked(task)
			task.flags &^= stacklessCoroTaskFusedTransitionMask
			s.cancelReservedFrameTasksLocked(task)
		}
		task.context.scheduler = nil
		task.resuming = false
		s.readyLocked(task)
		unlock(&s.lock)
	}()
	if deferred != nil {
		lock(&s.lock)
		value := s.terminalValues[task]
		panicking, exiting := task.terminal == stacklessCoroTerminalPanic, task.goexit
		unlock(&s.lock)
		deferred(value, panicking, exiting)
	} else {
		call()
	}
	returned = true
}
