// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

// A stacklessCoroResume executes one state-machine transition. The context is
// valid only for the duration of the call and must not be retained.
type stacklessCoroResume func(unsafe.Pointer) uint8

const (
	stacklessCoroActionInvalid uint8 = iota
	stacklessCoroActionYield
	stacklessCoroActionWait
	stacklessCoroActionComplete
)

type stacklessCoroTaskState uint8

const (
	stacklessCoroTaskNew stacklessCoroTaskState = iota
	stacklessCoroTaskRunnable
	stacklessCoroTaskRunning
	stacklessCoroTaskWaiting
	stacklessCoroTaskComplete
)

type stacklessCoroTask struct {
	resume stacklessCoroResume
	parent *stacklessCoroTask
	next   *stacklessCoroTask
	state  stacklessCoroTaskState
}

// stacklessCoroScheduler owns every continuation it runs. Producers enqueue
// work through scheduler operations; they never invoke a continuation.
type stacklessCoroScheduler struct {
	lock    mutex
	head    *stacklessCoroTask
	tail    *stacklessCoroTask
	current *stacklessCoroTask
	root    *stacklessCoroTask
	wake    chan struct{}
}

// A stacklessCoroOperation is owned by the runtime registry until its source
// publishes a terminal fact. Source callbacks carry only id.
type stacklessCoroOperation struct {
	id        uint64
	scheduler *stacklessCoroScheduler
	task      *stacklessCoroTask
	timer     *timer
	fd        int32
	buffer    []byte
	call      func()
	n         *int
	errno     *uintptr
	poll      bool
	valueOut  *uint64
	async     bool
	next      *stacklessCoroOperation
	workNext  *stacklessCoroOperation
}

var stacklessCoroOperations struct {
	lock mutex
	next uint64
	head *stacklessCoroOperation
}

func init() {
	lockInit(&stacklessCoroOperations.lock, lockRankLeafRank)
}

// coroRun drives one stackless logical goroutine and its children. The
// compiler emits calls to coroRun only for coroutine root adapters.
func coroRun(resume stacklessCoroResume) {
	if resume == nil {
		throw("runtime: nil stackless coroutine resume function")
	}
	s := &stacklessCoroScheduler{wake: make(chan struct{}, 1)}
	lockInit(&s.lock, lockRankLeafRank)
	root := &stacklessCoroTask{resume: resume}
	s.root = root
	s.ready(root, false)

	if coroRunOnNativeStack(s) {
		return
	}
	s.run(false)
}

func (s *stacklessCoroScheduler) run(native bool) {
	for !s.rootComplete() {
		task := s.take()
		if task == nil {
			<-s.wake
			continue
		}
		s.current = task
		action := task.resume(unsafe.Pointer(s))
		s.current = nil

		switch action {
		case stacklessCoroActionYield:
			s.yield(task)
			if !native {
				// Cooperate with the host scheduler when this target has no
				// native executor implementation.
				Gosched()
			}
		case stacklessCoroActionWait:
			s.waiting(task)
		case stacklessCoroActionComplete:
			s.complete(task)
		default:
			throw("runtime: invalid stackless coroutine action")
		}
	}
}

func (s *stacklessCoroScheduler) rootComplete() bool {
	lock(&s.lock)
	complete := s.root.state == stacklessCoroTaskComplete
	unlock(&s.lock)
	return complete
}

// coroAwait transfers execution to child. Completion makes the current task
// runnable; it does not call the parent continuation.
func coroAwait(ctx unsafe.Pointer, child stacklessCoroResume) {
	s := (*stacklessCoroScheduler)(ctx)
	if s == nil || s.current == nil || child == nil {
		throw("runtime: invalid stackless coroutine await")
	}
	parent := s.current
	lock(&s.lock)
	if parent.state != stacklessCoroTaskRunning {
		unlock(&s.lock)
		throw("runtime: stackless coroutine await outside resume")
	}
	parent.state = stacklessCoroTaskWaiting
	s.readyLocked(&stacklessCoroTask{resume: child, parent: parent})
	unlock(&s.lock)
}

// coroSpawn enqueues an independent logical goroutine. The current task keeps
// running until it returns its next scheduler action.
func coroSpawn(ctx unsafe.Pointer, child stacklessCoroResume) {
	s := (*stacklessCoroScheduler)(ctx)
	if s == nil || s.current == nil || child == nil {
		throw("runtime: invalid stackless coroutine spawn")
	}
	lock(&s.lock)
	if s.current.state != stacklessCoroTaskRunning {
		unlock(&s.lock)
		throw("runtime: stackless coroutine spawn outside resume")
	}
	s.readyLocked(&stacklessCoroTask{resume: child})
	unlock(&s.lock)
}

// coroSleep starts a timer operation for the current logical goroutine.
func coroSleep(ctx unsafe.Pointer, ns int64) {
	s := (*stacklessCoroScheduler)(ctx)
	task := s.startOperation("sleep")

	op := &stacklessCoroOperation{scheduler: s, task: task}
	op.id = registerStacklessCoroOperation(op)
	t := new(timer)
	t.init(stacklessCoroTimerReady, op.id)
	op.timer = t

	when := nanotime()
	if ns > 0 {
		when += ns
		if when < 0 {
			when = maxWhen
		}
	}
	t.reset(when, 0)
}

//go:nosplit
func coroEnterForeign() {
	mp := getg().m
	if mp.incgo {
		throw("runtime: nested stackless coroutine foreign call")
	}
	osPreemptExtEnter(mp)
	mp.incgo = true
	mp.ncgo++
}

//go:nosplit
func coroExitForeign() {
	mp := getg().m
	if !mp.incgo || mp.ncgo == 0 {
		throw("runtime: unmatched stackless coroutine foreign call")
	}
	mp.incgo = false
	mp.ncgo--
	osPreemptExtExit(mp)
}

//go:nosplit
func coroEnterBlocking() {
	entersyscallblock()
	coroEnterForeign()
}

//go:nosplit
func coroExitBlocking() {
	coroExitForeign()
	exitsyscall()
}

func (s *stacklessCoroScheduler) startOperation(name string) *stacklessCoroTask {
	if s == nil || s.current == nil {
		throw("runtime: invalid stackless coroutine " + name)
	}
	task := s.current
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning {
		unlock(&s.lock)
		throw("runtime: stackless coroutine operation outside resume")
	}
	task.state = stacklessCoroTaskWaiting
	unlock(&s.lock)
	return task
}

func (s *stacklessCoroScheduler) ready(task *stacklessCoroTask, signal bool) {
	lock(&s.lock)
	s.readyLocked(task)
	unlock(&s.lock)
	if signal {
		s.signal()
	}
}

func (s *stacklessCoroScheduler) readyLocked(task *stacklessCoroTask) {
	switch task.state {
	case stacklessCoroTaskNew, stacklessCoroTaskRunning, stacklessCoroTaskWaiting:
	default:
		throw("runtime: invalid stackless coroutine ready transition")
	}
	task.state = stacklessCoroTaskRunnable
	task.next = nil
	if s.tail == nil {
		s.head = task
	} else {
		s.tail.next = task
	}
	s.tail = task
}

func (s *stacklessCoroScheduler) take() *stacklessCoroTask {
	lock(&s.lock)
	task := s.head
	if task == nil {
		unlock(&s.lock)
		return nil
	}
	s.head = task.next
	if s.head == nil {
		s.tail = nil
	}
	task.next = nil
	if task.state != stacklessCoroTaskRunnable {
		unlock(&s.lock)
		throw("runtime: dequeued non-runnable stackless coroutine")
	}
	task.state = stacklessCoroTaskRunning
	unlock(&s.lock)
	return task
}

func (s *stacklessCoroScheduler) yield(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine yield")
	}
	s.readyLocked(task)
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) waiting(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskWaiting &&
		task.state != stacklessCoroTaskRunnable {
		unlock(&s.lock)
		throw("runtime: stackless coroutine wait without operation")
	}
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) complete(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine completion")
	}
	task.state = stacklessCoroTaskComplete
	task.resume = nil
	parent := task.parent
	task.parent = nil
	if parent == nil {
		unlock(&s.lock)
		return
	}
	if parent.state != stacklessCoroTaskWaiting {
		unlock(&s.lock)
		throw("runtime: stackless coroutine completed for non-waiting parent")
	}
	s.readyLocked(parent)
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func registerStacklessCoroOperation(op *stacklessCoroOperation) uint64 {
	lock(&stacklessCoroOperations.lock)
	stacklessCoroOperations.next++
	if stacklessCoroOperations.next == 0 {
		stacklessCoroOperations.next++
	}
	op.id = stacklessCoroOperations.next
	op.next = stacklessCoroOperations.head
	stacklessCoroOperations.head = op
	unlock(&stacklessCoroOperations.lock)
	return op.id
}

func takeStacklessCoroOperation(id uint64) *stacklessCoroOperation {
	lock(&stacklessCoroOperations.lock)
	link := &stacklessCoroOperations.head
	for *link != nil && (*link).id != id {
		link = &(*link).next
	}
	op := *link
	if op != nil {
		*link = op.next
		op.next = nil
	}
	unlock(&stacklessCoroOperations.lock)
	return op
}

func stacklessCoroTimerReady(arg any, _ uintptr, _ int64) {
	id, ok := arg.(uint64)
	if !ok {
		throw("runtime: invalid stackless coroutine timer ID")
	}
	op := takeStacklessCoroOperation(id)
	if op == nil {
		return
	}
	op.timer = nil
	op.scheduler.ready(op.task, true)
}

func findStacklessCoroOperation(id uint64) *stacklessCoroOperation {
	lock(&stacklessCoroOperations.lock)
	for op := stacklessCoroOperations.head; op != nil; op = op.next {
		if op.id == id {
			unlock(&stacklessCoroOperations.lock)
			return op
		}
	}
	unlock(&stacklessCoroOperations.lock)
	return nil
}
