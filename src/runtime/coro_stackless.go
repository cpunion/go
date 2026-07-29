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
	stacklessCoroActionPanic
	stacklessCoroActionGoexit
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
	resume       stacklessCoroResume
	parent       *stacklessCoroTask
	next         *stacklessCoroTask
	state        stacklessCoroTaskState
	terminal     stacklessCoroTerminalKind
	goexit       bool
	resuming     bool
	readyPending bool
	context      stacklessCoroContext
}

type stacklessCoroTerminalKind uint8

const (
	stacklessCoroTerminalNone stacklessCoroTerminalKind = iota
	stacklessCoroTerminalPanic
)

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
	terminalValues map[*stacklessCoroTask]any
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

	if nativeScheduler := coroRunOnNativeStack(s); nativeScheduler != nil {
		nativeScheduler.stopReplacementExecutors()
		nativeScheduler.finish()
		return
	}
	s.run(false)
	s.stopReplacementExecutors()
	s.finish()
}

func (s *stacklessCoroScheduler) finish() {
	if raceenabled {
		raceacquire(unsafe.Pointer(s.root))
	}
	kind := s.root.terminal
	goexit := s.root.goexit
	value, ok := s.terminalValues[s.root]
	s.root.terminal = stacklessCoroTerminalNone
	s.root.goexit = false
	delete(s.terminalValues, s.root)
	switch kind {
	case stacklessCoroTerminalNone:
	case stacklessCoroTerminalPanic:
		if !ok {
			throw("runtime: missing stackless coroutine panic value")
		}
		if goexit {
			stacklessCoroGoexitPanic(value)
			throw("runtime: stackless coroutine Goexit returned")
		}
		panic(value)
	default:
		throw("runtime: invalid stackless coroutine terminal outcome")
	}
	if goexit {
		Goexit()
		throw("runtime: stackless coroutine Goexit returned")
	}
}

// stacklessCoroGoexitPanic recreates a panic layered over a pending Goexit on
// the ordinary root stack. Recovering the panic then resumes Goexit.
func stacklessCoroGoexitPanic(value any) {
	defer func() {
		panic(value)
	}()
	Goexit()
}

func (s *stacklessCoroScheduler) run(native bool) {
	for !s.rootComplete() {
		s.runTasks(native)
	}
}

// runTasks drives the ready queue until the root completes or one resume
// panics. A recovered panic returns to run, which starts another queue-driving
// episode after this native activation has unwound.
func (s *stacklessCoroScheduler) runTasks(native bool) {
	var task *stacklessCoroTask
	defer func() {
		if task == nil || task.context.scheduler != s ||
			task.context.task != task {
			return
		}
		p := getg()._panic
		if p == nil || p.goexit {
			return
		}
		value := recover()
		// A native panic can begin normal cleanup or replace a panic already
		// being unwound. Replacement also preserves a pending Goexit.
		recordStacklessCoroPanic(s, task, value, true)
		task.context.scheduler = nil
		task.context.task = nil
		s.readyAfterPanic(task)
	}()

	for !s.rootComplete() {
		task = s.take()
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
		case stacklessCoroActionPanic:
			s.terminate(task)
		case stacklessCoroActionGoexit:
			s.goexit(task)
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
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine await outside resume")
	}
	parent.state = stacklessCoroTaskWaiting
	s.readyLocked(&stacklessCoroTask{resume: child, parent: parent})
	unlock(&s.lock)
}

// coroPanic records a panic value before the compiler-generated resume enters
// its frame-owned cleanup state.
func coroPanic(ctx unsafe.Pointer, value any) {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil {
		throw("runtime: invalid stackless coroutine panic")
	}
	recordStacklessCoroPanic(context.scheduler, context.task, value, false)
}

func recordStacklessCoroPanic(s *stacklessCoroScheduler,
	task *stacklessCoroTask, value any, replace bool) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		(!replace && task.terminal != stacklessCoroTerminalNone) ||
		(!replace && task.goexit) ||
		(replace && task.terminal != stacklessCoroTerminalNone &&
			task.terminal != stacklessCoroTerminalPanic) {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine panic transition")
	}
	task.terminal = stacklessCoroTerminalPanic
	if s.terminalValues == nil {
		s.terminalValues = make(map[*stacklessCoroTask]any)
	}
	s.terminalValues[task] = value
	unlock(&s.lock)
}

// coroPanicPending reports whether an awaited child transferred a panic to the
// current frame.
func coroPanicPending(ctx unsafe.Pointer) bool {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil {
		throw("runtime: invalid stackless coroutine panic query")
	}
	task := context.task
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending {
		throw("runtime: stackless coroutine panic query outside resume")
	}
	return task.terminal == stacklessCoroTerminalPanic
}

// coroGoexit starts Goexit cleanup for the running logical goroutine.
func coroGoexit(ctx unsafe.Pointer) {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil {
		throw("runtime: invalid stackless coroutine Goexit")
	}
	s := context.scheduler
	task := context.task
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine Goexit transition")
	}
	task.goexit = true
	unlock(&s.lock)
}

// coroTerminalAction reports the pending terminal action for a running task.
func coroTerminalAction(ctx unsafe.Pointer) uint8 {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil {
		throw("runtime: invalid stackless coroutine terminal query")
	}
	task := context.task
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending {
		throw("runtime: stackless coroutine terminal query outside resume")
	}
	switch task.terminal {
	case stacklessCoroTerminalNone:
		if task.goexit {
			return stacklessCoroActionGoexit
		}
		return stacklessCoroActionInvalid
	case stacklessCoroTerminalPanic:
		return stacklessCoroActionPanic
	default:
		throw("runtime: invalid stackless coroutine terminal state")
		return stacklessCoroActionInvalid
	}
}

// coroDeferToken returns a stable opaque task token for compiler-generated
// defer closures. The token may outlive one resume call, but can only be used
// while that task is actively running cleanup.
func coroDeferToken(ctx unsafe.Pointer) unsafe.Pointer {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task == nil ||
		context.task.state != stacklessCoroTaskRunning ||
		!context.task.resuming || context.task.readyPending {
		throw("runtime: invalid stackless coroutine defer token")
	}
	return unsafe.Pointer(context.task)
}

// coroDeferCall invokes a statically proven named defer target with access to
// its task-owned panic. Ordinary native panics remain visible to gorecover
// before this logical panic.
func coroDeferCall(token unsafe.Pointer, deferred func()) {
	task := (*stacklessCoroTask)(token)
	if task == nil || task.context.scheduler == nil ||
		task.context.task != task ||
		task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending {
		throw("runtime: invalid stackless coroutine defer call")
	}
	gp := getg()
	state := gp.stacklessCoro
	temporary := state == nil
	if temporary {
		state = new(stacklessCoroGState)
		gp.stacklessCoro = state
	}
	previous := state.deferTask
	state.deferTask = task
	defer func() {
		state.deferTask = previous
		if temporary {
			gp.stacklessCoro = nil
		}
	}()
	deferred()
}

// coroDeferPanic starts or replaces the panic owned by a running task.
func coroDeferPanic(token unsafe.Pointer, value any) {
	task := (*stacklessCoroTask)(token)
	if task == nil || task.context.scheduler == nil ||
		task.context.task != task {
		throw("runtime: invalid stackless coroutine defer panic")
	}
	recordStacklessCoroPanic(task.context.scheduler, task, value, true)
}

// coroDeferRecover takes the panic owned by a running task. The compiler only
// emits this call for a direct recover expression in the active defer body.
func coroDeferRecover(token unsafe.Pointer) any {
	task := (*stacklessCoroTask)(token)
	if task == nil || task.context.scheduler == nil ||
		task.context.task != task {
		throw("runtime: invalid stackless coroutine defer recover")
	}
	s := task.context.scheduler
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending {
		unlock(&s.lock)
		throw("runtime: stackless coroutine defer recover outside resume")
	}
	if task.terminal == stacklessCoroTerminalNone {
		unlock(&s.lock)
		return nil
	}
	value, ok := s.terminalValues[task]
	if task.terminal != stacklessCoroTerminalPanic || !ok {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine defer recover state")
	}
	task.terminal = stacklessCoroTerminalNone
	delete(s.terminalValues, task)
	unlock(&s.lock)

	if value == nil {
		if debug.panicnil.Load() != 1 {
			value = new(PanicNilError)
		} else {
			panicnil.IncNonDefault()
		}
	}
	return value
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
	if context.task.state != stacklessCoroTaskRunning ||
		!context.task.resuming ||
		context.task.readyPending ||
		context.task.terminal != stacklessCoroTerminalNone ||
		context.task.goexit {
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
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
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
	if task.resuming {
		if task.state != stacklessCoroTaskWaiting || task.readyPending {
			throw("runtime: invalid stackless coroutine early ready transition")
		}
		// An operation may finish before the active resume call returns Wait.
		// Defer publication until the scheduler consumes that action, so two
		// executors cannot enter the same resume function concurrently.
		task.readyPending = true
		if raceenabled {
			racereleasemerge(unsafe.Pointer(task))
		}
		return
	}
	switch task.state {
	case stacklessCoroTaskNew, stacklessCoroTaskRunning, stacklessCoroTaskWaiting:
	default:
		throw("runtime: invalid stackless coroutine ready transition")
	}
	if task.readyPending {
		throw("runtime: stackless coroutine has pending ready transition")
	}
	task.state = stacklessCoroTaskRunnable
	task.next = nil
	if s.tail == nil {
		s.head = task
	} else {
		s.tail.next = task
	}
	s.tail = task
	if raceenabled {
		// Runtime mutex operations are invisible to the race detector.
		// Merge both the previous resume episode and the producer that made
		// this task runnable into the next executor's acquire.
		racereleasemerge(unsafe.Pointer(task))
	}
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
	if task.state != stacklessCoroTaskRunnable || task.resuming ||
		task.readyPending {
		unlock(&s.lock)
		throw("runtime: dequeued non-runnable stackless coroutine")
	}
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	unlock(&s.lock)
	if raceenabled {
		raceacquire(unsafe.Pointer(task))
	}
	return task
}

func (s *stacklessCoroScheduler) yield(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine yield")
	}
	task.resuming = false
	s.readyLocked(task)
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) waiting(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskWaiting || !task.resuming {
		unlock(&s.lock)
		throw("runtime: stackless coroutine wait without operation")
	}
	task.resuming = false
	ready := task.readyPending
	task.readyPending = false
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	if ready {
		s.readyLocked(task)
	}
	unlock(&s.lock)
	if ready {
		s.signal()
	}
}

func (s *stacklessCoroScheduler) readyAfterPanic(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalPanic {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine panic recovery")
	}
	task.resuming = false
	s.readyLocked(task)
	unlock(&s.lock)
	s.signal()
}

func (s *stacklessCoroScheduler) complete(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine completion")
	}
	task.resuming = false
	task.state = stacklessCoroTaskComplete
	task.resume = nil
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	parent := task.parent
	task.parent = nil
	if parent == nil {
		unlock(&s.lock)
		s.signalAll()
		return
	}
	if parent.state != stacklessCoroTaskWaiting ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine completed for non-waiting parent")
	}
	s.readyLocked(parent)
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) terminate(task *stacklessCoroTask) {
	lock(&s.lock)
	value, ok := s.terminalValues[task]
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalPanic || !ok {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine termination")
	}
	task.resuming = false
	task.state = stacklessCoroTaskComplete
	task.resume = nil
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	parent := task.parent
	task.parent = nil
	if parent == nil {
		if task != s.root {
			delete(s.terminalValues, task)
			task.terminal = stacklessCoroTerminalNone
			task.goexit = false
			unlock(&s.lock)
			panic(value)
		}
		unlock(&s.lock)
		s.signalAll()
		return
	}
	if parent.state != stacklessCoroTaskWaiting ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine terminated for invalid parent")
	}
	delete(s.terminalValues, task)
	parent.terminal = task.terminal
	parent.goexit = task.goexit
	task.terminal = stacklessCoroTerminalNone
	task.goexit = false
	s.terminalValues[parent] = value
	s.readyLocked(parent)
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) goexit(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || !task.goexit {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine Goexit termination")
	}
	task.resuming = false
	task.state = stacklessCoroTaskComplete
	task.resume = nil
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	parent := task.parent
	task.parent = nil
	if parent == nil {
		if task == s.root {
			unlock(&s.lock)
			s.signalAll()
			return
		}
		task.goexit = false
		unlock(&s.lock)
		return
	}
	if parent.state != stacklessCoroTaskWaiting ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine Goexit for invalid parent")
	}
	parent.goexit = true
	task.goexit = false
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
		if coroRunOnNativeStack(s) == nil {
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
