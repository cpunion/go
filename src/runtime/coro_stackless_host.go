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
	next         *stacklessCoroHost
	head, tail   *stacklessCoroTask
	wake         chan struct{}
	pending      *stacklessCoroTask
	call         func()
	deferred     func(any, bool, bool)
	pendingReady bool
	done         bool
	started      bool
	terminal     bool
	startG       *g
	startNext    *stacklessCoroHost
}

// Only an ordinary host allocates this extension. The native executor keeps
// the original three-pointer G state and its existing context size class.
type stacklessCoroHostGState struct {
	stacklessCoroGState
	host          *stacklessCoroHost
	ordinaryDepth uint32
	activeTask    *stacklessCoroTask
	parkedTask    *stacklessCoroTask
	parkedTrace   *stacklessCoroPanicTrace
}

// Fixed executors retain their original self pointer. An ordinary extension
// points to its blockingScheduler slot instead. Both are real GC-visible
// pointers into the owning allocation; no pointer bits are tagged.
func stacklessCoroHostState(gp *g) *stacklessCoroHostGState {
	state := gp.stacklessCoro
	if state == nil || state.native != unsafe.Pointer(&state.blockingScheduler) {
		return nil
	}
	return (*stacklessCoroHostGState)(unsafe.Pointer(state))
}

// The stopped host owns this suspended lineage even if an operation producer
// has made it runnable. Only the host can resume or recycle its source frames.
// Requiring its private receive excludes ordinary callbacks and active resumes.
func stacklessCoroParkedSource(gp *g) *stacklessCoroHostGState {
	state := stacklessCoroHostState(gp)
	if state == nil || state.host == nil || state.ordinaryDepth != 0 ||
		state.activeTask != nil || state.parkedTask == nil || gp.waiting == nil {
		return nil
	}
	wake := *(*uintptr)(unsafe.Pointer(&state.host.wake))
	if gp.waiting.channelUintptr() != wake {
		return nil
	}
	return state
}

func currentStacklessCoroHost() *stacklessCoroHost {
	if state := stacklessCoroHostState(getg()); state != nil {
		return state.host
	}
	return nil
}

func stacklessCoroOrdinaryHostCall(gp *g) bool {
	state := stacklessCoroHostState(gp)
	return state != nil && state.ordinaryDepth != 0
}

// Real panic values always have a nonnil task key. This private cold slot
// shares the existing optional metadata pointer without growing every scheduler.
func (s *stacklessCoroScheduler) hostsLocked() *stacklessCoroHost {
	value, _ := s.terminalValue(nil)
	hosts, _ := value.(*stacklessCoroHost)
	return hosts
}

func (s *stacklessCoroScheduler) taskHostLocked(task *stacklessCoroTask) *stacklessCoroHost {
	if s.terminalValues == nil {
		return nil
	}
	for task.parent != nil {
		task = task.parent
	}
	for h := s.hostsLocked(); h != nil; h = h.next {
		if h.root == task {
			return h
		}
	}
	return nil
}

func (s *stacklessCoroScheduler) taskHostCurrentLocked(task *stacklessCoroTask) bool {
	h := s.taskHostLocked(task)
	return h == nil || currentStacklessCoroHost() == h
}

// The source resume has already returned its terminal action. Unlike an
// ordinary-call handoff, no source code needs to run again on the new host.
// Keep the task and registry identity live until the host consumes the action.
// Requires s.lock and an independently spawned task without an existing host.
func (s *stacklessCoroScheduler) terminalHostLocked(task *stacklessCoroTask) *stacklessCoroHost {
	h := &stacklessCoroHost{scheduler: s, root: task, next: s.hostsLocked(),
		wake: make(chan struct{}, 1), pending: task, pendingReady: true, terminal: true}
	s.lifetime.Or(stacklessCoroLifetimeHostStarted)
	s.initialRootPrivate = false
	s.setTerminalValue(nil, h)
	task.state, task.resuming = stacklessCoroTaskWaiting, false
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	return h
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

// Called after the compiler's ordinary thunk has directly deferred the frozen
// source function. Preserve a pending Goexit even when a new panic overlays it.
func coroDeferInject(value any, panicking, exiting bool) {
	var trace, exitTrace *stacklessCoroPanicTrace
	if state := stacklessCoroHostState(getg()); state != nil && state.activeTask != nil {
		task := state.activeTask
		s := task.context.scheduler
		lock(&s.lock)
		trace, exitTrace = s.terminalTrace(task), s.terminalExitTrace(task)
		unlock(&s.lock)
	}
	if panicking {
		if exiting {
			defer func() { raiseStacklessCoroPanic(value, trace) }()
			raiseStacklessCoroGoexit(exitTrace)
		}
		raiseStacklessCoroPanic(value, trace)
	}
	if exiting {
		raiseStacklessCoroGoexit(exitTrace)
	}
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
	h := s.taskHostLocked(root)
	start := false
	if h == nil {
		h = &stacklessCoroHost{scheduler: s, root: root, next: s.hostsLocked()}
		s.lifetime.Or(stacklessCoroLifetimeHostStarted)
		if root == &s.root {
			h.wake = s.rootDone
			s.lifetime.Or(stacklessCoroLifetimePublicHost)
		} else {
			h.wake = make(chan struct{}, 1)
			start = true
		}
		s.setTerminalValue(nil, h)
		// A pull comparison retains its active leaf in head instead of an
		// ordinary queue. That leaf now belongs exclusively to this host.
		if stacklessCoroIsPullComparison(s) && s.head == task {
			s.head = nil
		}
	}
	if h.done || h.pending != nil || h.call != nil || h.deferred != nil || h.terminal {
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
		stacklessCoroHostStart(h)
	}
	return stacklessCoroActionHost
}

// Keep the host's entry closure in one non-inlined function. Its stable entry
// identifies a user goroutine from newproc1 through goexit, so goroutine counts
// and concurrent profiles use the same classification for its whole lifetime.
// Keep isSystemGoroutine and internal/trace.IsSystemGoroutine in sync.
//
//go:noinline
func stacklessCoroHostStart(h *stacklessCoroHost) {
	stacklessCoroRegisterHostStart(h)
	go func() { h.run() }()
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
	var previous *stacklessCoroHost
	h := s.hostsLocked()
	for h != nil && h.root != task {
		previous, h = h, h.next
	}
	if h == nil {
		return
	}
	if h.pending != nil || h.head != nil || h.tail != nil || h.done || h.terminal {
		throw("runtime: stackless coroutine host completed with active work")
	}
	// Set done and remove the identity before an independent task is recycled.
	h.done = true
	if previous != nil {
		previous.next = h.next
	} else if h.next != nil {
		s.setTerminalValue(nil, h.next)
	} else {
		s.clearTerminalValue(nil)
	}
	h.next = nil
	h.notifyLocked()
}

func (s *stacklessCoroScheduler) waitPublicRoot() {
	for !s.rootComplete() {
		lock(&s.lock)
		h := s.taskHostLocked(&s.root)
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
	gp := getg()
	previous := gp.stacklessCoro
	state := new(stacklessCoroHostGState)
	if previous != nil {
		if gp.stackIsFixed() {
			throw("runtime: ordinary host on fixed executor")
		}
		state.stacklessCoroGState = *previous
	}
	previousDefer := state.deferState
	state.native = unsafe.Pointer(&state.blockingScheduler)
	state.host = h
	gp.stacklessCoro = &state.stacklessCoroGState
	defer func() {
		if state.blockingScheduler != nil || state.deferState != previousDefer || state.ordinaryDepth != 0 || state.activeTask != nil || state.parkedTask != nil || state.parkedTrace != nil {
			throw("runtime: active state during stackless coroutine host finish")
		}
		gp.stacklessCoro = previous
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
			value, panicking := s.terminalValue(h.root)
			trace := s.terminalTrace(h.root)
			if h.root.terminal == stacklessCoroTerminalPanic && panicking {
				h.root.terminal = stacklessCoroTerminalNone
				s.clearTerminalValue(h.root)
				s.clearExitTrace(h.root)
				unlock(&s.lock)
				raiseStacklessCoroPanic(value, trace)
			}
			s.clearExitTrace(h.root)
			unlock(&s.lock)
		}
	}()
	h.drive()
}

func (h *stacklessCoroHost) drive() {
	s := h.scheduler
	state := stacklessCoroHostState(getg())
	for {
		lock(&s.lock)
		if h.done {
			state.parkedTask = nil
			state.parkedTrace = nil
			unlock(&s.lock)
			return
		}
		if h.pendingReady {
			state.parkedTask = nil
			state.parkedTrace = nil
			task, call, deferred := h.pending, h.call, h.deferred
			terminal := h.terminal
			h.pending, h.call, h.deferred, h.pendingReady = nil, nil, nil, false
			h.terminal = false
			if task.state != stacklessCoroTaskWaiting || task.resuming || task.readyPending {
				unlock(&s.lock)
				throw("runtime: invalid stackless coroutine host invocation")
			}
			task.state, task.resuming = stacklessCoroTaskRunning, true
			unlock(&s.lock)
			if raceenabled {
				raceacquire(unsafe.Pointer(task))
			}
			if terminal {
				s.terminate(task)
				throw("runtime: terminal coroutine host returned")
			}
			h.invoke(task, call, deferred)
			continue
		}
		ready := h.head != nil
		if !ready && state.parkedTask != nil && state.parkedTrace == nil {
			// Native cleanup can wait after its incoming panic or Goexit
			// released the original frames. Capture those source positions
			// while the metadata is locked, outside the profiler's walk.
			// Ordinary waits retain their live frames without a snapshot.
			for task := state.parkedTask; task != nil; task = task.parent {
				if s.terminalSourceTrace(task) != nil || s.deferTrace(task) != nil {
					state.parkedTrace = captureStacklessCoroPanicTrace(state.parkedTask, nil, 0, nil)
					break
				}
			}
		}
		unlock(&s.lock)
		if ready {
			// Drop the old leaf before any frame can resume or be recycled.
			// The no-work return identifies the new suspended leaf, including
			// structured children selected during this episode.
			state.parkedTask = nil
			state.parkedTrace = nil
			state.parkedTask = s.runTasks(false, false, false, false)
		} else {
			<-h.wake
		}
	}
}

func (h *stacklessCoroHost) invoke(task *stacklessCoroTask, call func(), deferred func(any, bool, bool)) {
	s := h.scheduler
	state := stacklessCoroHostState(getg())
	if state == nil || state.host != h {
		throw("runtime: missing ordinary host call state")
	}
	oldDefer := state.deferState
	oldTask := state.activeTask
	state.deferState = nil
	state.activeTask = task
	state.ordinaryDepth++
	returned := false
	defer func() {
		var value any
		var p *_panic
		panicking, exiting := false, false
		if !returned {
			p = getg()._panic
			if p != nil && !p.goexit {
				value, panicking = recover(), true
			} else {
				exiting = true
			}
		}
		state.ordinaryDepth--
		state.deferState = oldDefer
		state.activeTask = oldTask
		if panicking {
			recordStacklessCoroPanic(s, task, value, true, p)
		}
		lock(&s.lock)
		if task.state != stacklessCoroTaskRunning || !task.resuming || task.readyPending {
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine host return")
		}
		if exiting {
			trace := captureStacklessCoroPanicTrace(task, p, 0, nil)
			if trace != nil {
				trace.goexit = true
				p.coroTrace = trace
			}
			s.setExitTrace(task, trace)
			task.terminal = stacklessCoroTerminalNone
			s.clearTerminalValue(task)
			task.goexit = true
		} else if returned && deferred != nil {
			// An injected incoming panic returns normally only when the
			// direct source defer recovered it. Goexit remains sticky.
			task.terminal = stacklessCoroTerminalNone
			s.clearTerminalValue(task)
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
		value, _ := s.terminalValue(task)
		panicking, exiting := task.terminal == stacklessCoroTerminalPanic, task.goexit
		unlock(&s.lock)
		deferred(value, panicking, exiting)
	} else {
		call()
	}
	returned = true
}
