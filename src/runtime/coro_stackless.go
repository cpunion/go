// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// A stacklessCoroResume executes one state-machine transition. The context is
// valid only for the duration of the call and must not be retained.
type stacklessCoroResume func(unsafe.Pointer) uint8

const (
	stacklessCoroActionInvalid uint8 = iota
	stacklessCoroActionYield
	stacklessCoroActionWait
	stacklessCoroActionComplete
)

const stacklessCoroExecutorCount = 4

type stacklessCoroTaskState uint8

const (
	stacklessCoroTaskNew stacklessCoroTaskState = iota
	stacklessCoroTaskRunnable
	stacklessCoroTaskRunning
	stacklessCoroTaskWaiting
	stacklessCoroTaskComplete
)

type stacklessCoroTask struct {
	resume  stacklessCoroResume
	parent  *stacklessCoroTask
	next    *stacklessCoroTask
	state   stacklessCoroTaskState
	context stacklessCoroContext
}

type stacklessCoroContext struct {
	scheduler *stacklessCoroScheduler
	task      *stacklessCoroTask
}

// stacklessCoroScheduler owns every continuation it runs. Producers enqueue
// work through scheduler operations; they never invoke a continuation.
type stacklessCoroScheduler struct {
	lock           mutex
	head           *stacklessCoroTask
	tail           *stacklessCoroTask
	root           *stacklessCoroTask
	wake           chan struct{}
	executorWake   chan struct{}
	executorDone   chan struct{}
	executorsReady atomic.Uint32
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
	packet    [2]uint64
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
	s := &stacklessCoroScheduler{
		wake:         make(chan struct{}, stacklessCoroExecutorCount),
		executorWake: make(chan struct{}, stacklessCoroExecutorCount-1),
		executorDone: make(chan struct{}, stacklessCoroExecutorCount-1),
	}
	lockInit(&s.lock, lockRankLeafRank)
	root := &stacklessCoroTask{resume: resume}
	s.root = root
	s.ready(root, false)

	if coroRunOnNativeStack(s) {
		s.stopReplacementExecutors()
		return
	}
	s.run(false)
	s.stopReplacementExecutors()
}

func (s *stacklessCoroScheduler) run(native bool) {
	for !s.rootComplete() {
		task := s.take()
		if task == nil {
			<-s.wake
			continue
		}
		task.context.scheduler = s
		task.context.task = task
		action := task.resume(unsafe.Pointer(&task.context))
		task.context.scheduler = nil
		task.context.task = nil

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
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil ||
		child == nil {
		throw("runtime: invalid stackless coroutine await")
	}
	s := context.scheduler
	parent := context.task
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
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil ||
		child == nil {
		throw("runtime: invalid stackless coroutine spawn")
	}
	s := context.scheduler
	lock(&s.lock)
	if context.task.state != stacklessCoroTaskRunning {
		unlock(&s.lock)
		throw("runtime: stackless coroutine spawn outside resume")
	}
	s.readyLocked(&stacklessCoroTask{resume: child})
	unlock(&s.lock)
	s.prepareReplacementExecutors()
}

// coroSleep starts a timer operation for the current logical goroutine.
func coroSleep(ctx unsafe.Pointer, ns int64) {
	startStacklessCoroTimer(ctx, ns)
}

func startStacklessCoroTimer(ctx unsafe.Pointer, ns int64) uint64 {
	s, task := stacklessCoroStartOperation(ctx, "sleep")

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
	return op.id
}

func cancelStacklessCoroTimer(id uint64) bool {
	op := takeStacklessCoroOperation(id)
	if op == nil {
		return false
	}
	if op.timer == nil {
		throw("runtime: canceled stackless coroutine operation is not a timer")
	}
	op.timer.stop()
	op.timer = nil
	op.scheduler.ready(op.task, true)
	return true
}

//go:nosplit
func coroEnterForeign() {
	gp := getg()
	mp := gp.m
	if mp.incgo || gp.nocgocallback {
		throw("runtime: nested stackless coroutine foreign call")
	}
	osPreemptExtEnter(mp)
	mp.ncgocall++
	if mp.cgoCallers != nil {
		mp.cgoCallers[0] = 0
	}
	gp.nocgocallback = true
	mp.incgo = true
	mp.ncgo++
}

//go:nosplit
func coroExitForeign() {
	gp := getg()
	mp := gp.m
	if !mp.incgo || mp.ncgo == 0 || !gp.nocgocallback {
		throw("runtime: unmatched stackless coroutine foreign call")
	}
	gp.nocgocallback = false
	mp.incgo = false
	mp.ncgo--
	osPreemptExtExit(mp)
	if sys.DITSupported {
		ditEnabled := sys.DITEnabled()
		if !gp.ditWanted && ditEnabled {
			sys.DisableDIT()
		} else if gp.ditWanted && !ditEnabled {
			sys.EnableDIT()
		}
	}
}

//go:nosplit
func coroEnterBlocking(ctx unsafe.Pointer) {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil {
		throw("runtime: invalid stackless coroutine blocking call")
	}
	context.scheduler.wakeReplacementExecutor()
	// coroExitBlocking returns through a different helper frame. Save the
	// continuation frame so exitsyscall never refers to an unwound entry
	// frame. This is the same reentry protocol used by cgo callbacks.
	pc := sys.GetCallerPC()
	sp := sys.GetCallerSP()
	bp := getcallerfp()
	reentersyscall(pc, sp, bp)
	coroEnterForeign()
}

//go:nosplit
func coroExitBlocking() {
	coroExitForeign()
	exitsyscall()
}

func stacklessCoroStartOperation(ctx unsafe.Pointer, name string) (*stacklessCoroScheduler, *stacklessCoroTask) {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil {
		throw("runtime: invalid stackless coroutine " + name)
	}
	s := context.scheduler
	task := context.task
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning {
		unlock(&s.lock)
		throw("runtime: stackless coroutine operation outside resume")
	}
	task.state = stacklessCoroTaskWaiting
	unlock(&s.lock)
	return s, task
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
		s.signalAll()
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

func (s *stacklessCoroScheduler) signalAll() {
	for range stacklessCoroExecutorCount {
		s.signal()
	}
}

func (s *stacklessCoroScheduler) prepareReplacementExecutors() {
	if !s.executorsReady.CompareAndSwap(0, 1) {
		return
	}

	for range stacklessCoroExecutorCount - 1 {
		go s.replacementExecutor()
	}
}

func (s *stacklessCoroScheduler) replacementExecutor() {
	<-s.executorWake
	if !s.rootComplete() {
		if !coroRunOnNativeStack(s) {
			s.run(false)
		}
	}
	s.executorDone <- struct{}{}
}

//go:nosplit
func (s *stacklessCoroScheduler) wakeReplacementExecutor() {
	if s == nil || s.executorsReady.Load() == 0 {
		return
	}
	select {
	case s.executorWake <- struct{}{}:
	default:
	}
}

func (s *stacklessCoroScheduler) stopReplacementExecutors() {
	if s.executorsReady.Load() == 0 {
		return
	}
	for range stacklessCoroExecutorCount - 1 {
		s.wakeReplacementExecutor()
	}
	s.signalAll()
	for range stacklessCoroExecutorCount - 1 {
		<-s.executorDone
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
