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

const stacklessCoroWarmExecutorCount = 4
const stacklessCoroTaskCacheSize = 256
const stacklessCoroFrameCacheSize = 32 << 10
const stacklessCoroOperationCacheSize = 64

// stacklessCoroUncachedFrameLineage marks an explicit-frame task that was
// created after the task cache saturated. It cannot be a cached frame size.
const stacklessCoroUncachedFrameLineage = ^uint16(0)
const stacklessCoroForeignReturnerBit = uint32(1 << 31)

const (
	stacklessCoroExecutorStateOff uint32 = iota
	stacklessCoroExecutorStatePreparing
	stacklessCoroExecutorStateRunning
	stacklessCoroExecutorStateStopping
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
	resume       stacklessCoroResume
	parent       *stacklessCoroTask
	next         *stacklessCoroTask
	state        stacklessCoroTaskState
	terminal     stacklessCoroTerminalKind
	goexit       bool
	resuming     bool
	readyPending bool
	cacheFrame   bool
	frameSize    uint16
	context      stacklessCoroContext
}

type stacklessCoroTerminalKind uint8

const (
	stacklessCoroTerminalNone stacklessCoroTerminalKind = iota
	stacklessCoroTerminalPanic
)

type stacklessCoroContext struct {
	// frame stays first so compiler-generated resume functions can use the
	// context as the stable frame-bearing resume packet.
	frame     unsafe.Pointer
	scheduler *stacklessCoroScheduler
}

// task returns the task that embeds context. Keeping the resume packet in the
// task lets an indirect resume call use a stable address without allocating a
// packet on every transition.
func (context *stacklessCoroContext) task() *stacklessCoroTask {
	if context == nil {
		return nil
	}
	return (*stacklessCoroTask)(unsafe.Pointer(
		uintptr(unsafe.Pointer(context)) -
			unsafe.Offsetof(stacklessCoroTask{}.context)))
}

// stacklessCoroScheduler owns every continuation it runs. Producers enqueue
// work through scheduler operations; they never invoke a continuation.
type stacklessCoroScheduler struct {
	lock          mutex
	head          *stacklessCoroTask
	tail          *stacklessCoroTask
	freeTasks     *stacklessCoroTask
	reservedTasks *stacklessCoroTask
	// The bounded task and frame caches fit in uint16. Keep their counters
	// packed so enabling frame reuse does not grow the scheduler size class.
	freePlainTaskCount uint16
	freeFrameBytes     uint16
	cachedFrameTasks   uint16
	cachedFrameBytes   uint16
	freeOperations     *stacklessCoroOperation
	// The operation cache is bounded at 64 entries. Keeping its count in
	// uint32 leaves room for the lock-free frame-cache saturation hint
	// without growing the scheduler.
	frameCacheSaturatedState atomic.Uint32
	freeOperationCount       uint32
	root                     stacklessCoroTask
	terminalValues           map[*stacklessCoroTask]any
	wake                     chan struct{}
	// executorWake admits the initial warm replacements. executorGrow asks
	// the manager for capacity beyond that warm set, and executorStop
	// broadcasts root completion to every admitted or waiting replacement.
	// These channels remain nil until a root first needs replacement
	// executors.
	executorWake        chan struct{}
	executorDone        chan struct{}
	executorGrow        chan struct{}
	executorStop        chan struct{}
	executorManagerDone chan struct{}
	executorState       atomic.Uint32
	// executorCount includes the original driver. blockingExecutors counts
	// drivers that entered a blocking foreign call but have not yet returned
	// with a P.
	executorCount     atomic.Uint32
	blockingExecutors atomic.Uint32
	// runnableState stores the runnable count in its low 31 bits. The high
	// bit asks a native executor to yield its P to a returning foreign call.
	runnableState    atomic.Uint32
	foreignReturners atomic.Uint32
}

func (s *stacklessCoroScheduler) frameCacheSaturated() bool {
	return s.frameCacheSaturatedState.Load() != 0
}

// setFrameCacheSaturatedLocked publishes a transition into or out of
// saturation. The scheduler lock must be held.
func (s *stacklessCoroScheduler) setFrameCacheSaturatedLocked(saturated bool) {
	if saturated {
		s.frameCacheSaturatedState.Store(1)
	} else {
		s.frameCacheSaturatedState.Store(0)
	}
}

// A stacklessCoroOperation is owned by the runtime registry until its source
// publishes a terminal fact. Source callbacks carry only id.
type stacklessCoroOperation struct {
	id        uint64
	scheduler *stacklessCoroScheduler
	task      *stacklessCoroTask
	timer     *timer
	channel   *hchan
	element   unsafe.Pointer
	received  *bool
	send      bool
	timerWait bool
	selection *stacklessCoroSelect
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
	coroRunFrame(nil, resume)
}

func coroRunFrame(frame unsafe.Pointer, resume stacklessCoroResume) {
	s := newStacklessCoroScheduler(frame, resume)

	if nativeScheduler := coroRunOnNativeStack(s); nativeScheduler != nil {
		nativeScheduler.stopReplacementExecutors()
		nativeScheduler.finish()
		return
	}
	s.run(false)
	s.stopReplacementExecutors()
	s.finish()
}

func newStacklessCoroScheduler(frame unsafe.Pointer,
	resume stacklessCoroResume) *stacklessCoroScheduler {
	if resume == nil {
		throw("runtime: nil stackless coroutine resume function")
	}
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	s.executorCount.Store(1)
	lockInit(&s.lock, lockRankLeafRank)
	s.root.resume = resume
	s.root.context.frame = frame
	s.ready(&s.root, false)
	return s
}

// newTaskLocked returns a new or recycled non-root task. The scheduler lock
// must be held.
func (s *stacklessCoroScheduler) newTaskLocked(frame unsafe.Pointer,
	resume stacklessCoroResume, parent *stacklessCoroTask) *stacklessCoroTask {
	task := s.takeFreeTaskLocked()
	if task == nil {
		return &stacklessCoroTask{
			resume: resume,
			parent: parent,
			context: stacklessCoroContext{
				frame: frame,
			},
		}
	}
	task.resume = resume
	task.parent = parent
	task.context.frame = frame
	task.cacheFrame = false
	task.frameSize = 0
	return task
}

func (s *stacklessCoroScheduler) takeFreeTaskLocked() *stacklessCoroTask {
	task := s.freeTasks
	if task == nil {
		if s.freePlainTaskCount != 0 || s.freeFrameBytes != 0 {
			throw("runtime: invalid stackless coroutine task cache")
		}
		return nil
	}
	if s.freePlainTaskCount > stacklessCoroTaskCacheSize {
		throw("runtime: invalid stackless coroutine task cache count")
	}
	var previous *stacklessCoroTask
	if s.freePlainTaskCount > 0 {
		for task != nil && task.cacheFrame {
			previous = task
			task = task.next
		}
		if task == nil {
			throw("runtime: invalid stackless coroutine plain task cache")
		}
	}
	if previous == nil {
		s.freeTasks = task.next
	} else {
		previous.next = task.next
	}
	task.next = nil
	if task.cacheFrame {
		if s.freeFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine frame cache size")
		}
		s.freeFrameBytes -= task.frameSize
		s.discardCachedFrameTaskLocked(task)
	} else {
		if s.freePlainTaskCount == 0 {
			throw("runtime: invalid stackless coroutine task cache count")
		}
		s.freePlainTaskCount--
	}
	return task
}

func clearStacklessCoroTaskFrame(task *stacklessCoroTask) {
	task.resume = nil
	task.context.frame = nil
	task.cacheFrame = false
	task.frameSize = 0
}

func (s *stacklessCoroScheduler) discardCachedFrameTaskLocked(
	task *stacklessCoroTask) {
	if task.cacheFrame {
		if s.cachedFrameTasks == 0 ||
			s.cachedFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine cached-frame ownership")
		}
		if s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
			s.freeFrameBytes == 0 {
			s.setFrameCacheSaturatedLocked(false)
		}
		s.cachedFrameTasks--
		s.cachedFrameBytes -= task.frameSize
	}
	clearStacklessCoroTaskFrame(task)
}

// recycleTaskLocked retains a completed non-root task for this scheduler. A
// task address is also a race-detector synchronization identity, so race
// builds must not reuse it for an unrelated logical goroutine.
func (s *stacklessCoroScheduler) recycleTaskLocked(task *stacklessCoroTask) {
	_, hasTerminalValue := s.terminalValues[task]
	validFrame := !task.cacheFrame && task.resume == nil &&
		task.context.frame == nil && task.frameSize == 0
	if task.cacheFrame {
		validFrame = task.resume != nil && task.context.frame != nil &&
			int(task.frameSize) <= stacklessCoroFrameCacheSize &&
			s.cachedFrameTasks > 0 &&
			s.cachedFrameBytes >= task.frameSize
	}
	if task == &s.root || !validFrame || task.parent != nil ||
		task.next != nil || task.state != stacklessCoroTaskComplete ||
		task.terminal != stacklessCoroTerminalNone || task.goexit ||
		task.resuming || task.readyPending ||
		task.context.scheduler != nil ||
		hasTerminalValue {
		throw("runtime: invalid stackless coroutine task recycle")
	}
	if raceenabled {
		s.discardCachedFrameTaskLocked(task)
		return
	}
	if s.cachedFrameTasks > stacklessCoroTaskCacheSize ||
		s.freePlainTaskCount >
			uint16(stacklessCoroTaskCacheSize)-s.cachedFrameTasks {
		throw("runtime: invalid stackless coroutine task cache size")
	}
	if !task.cacheFrame &&
		s.freePlainTaskCount ==
			uint16(stacklessCoroTaskCacheSize)-s.cachedFrameTasks {
		return
	}
	task.state = stacklessCoroTaskNew
	task.next = s.freeTasks
	s.freeTasks = task
	if task.cacheFrame {
		if s.freeFrameBytes > s.cachedFrameBytes ||
			s.cachedFrameBytes-s.freeFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine free-frame size")
		}
		if s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
			s.freeFrameBytes == 0 {
			s.setFrameCacheSaturatedLocked(false)
		}
		s.freeFrameBytes += task.frameSize
	} else {
		s.freePlainTaskCount++
	}
}

func (s *stacklessCoroScheduler) finish() {
	if raceenabled {
		raceacquire(unsafe.Pointer(&s.root))
	}
	kind := s.root.terminal
	goexit := s.root.goexit
	value, ok := s.terminalValues[&s.root]
	s.root.terminal = stacklessCoroTerminalNone
	s.root.goexit = false
	delete(s.terminalValues, &s.root)
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
	if native {
		s.runLoop(true)
		return
	}
	scope := enterStacklessCoroRunScope()
	defer scope.leave()
	s.runLoop(false)
}

func (s *stacklessCoroScheduler) runLoop(native bool) {
	for !s.rootComplete() {
		s.runTasks(native)
	}
}

type stacklessCoroRunScope struct {
	gp        *g
	state     *stacklessCoroGState
	temporary bool
}

func enterStacklessCoroRunScope() stacklessCoroRunScope {
	gp := getg()
	state := gp.stacklessCoro
	temporary := state == nil
	if temporary {
		state = new(stacklessCoroGState)
		gp.stacklessCoro = state
	}
	return stacklessCoroRunScope{gp: gp, state: state, temporary: temporary}
}

func (scope *stacklessCoroRunScope) leave() {
	if scope.state.blockingScheduler != nil {
		throw("runtime: active stackless coroutine blocking call after run")
	}
	if scope.temporary {
		if scope.gp.stacklessCoro != scope.state || scope.state.deferTask != nil {
			throw("runtime: invalid stackless coroutine run state")
		}
		scope.gp.stacklessCoro = nil
	}
}

// runTasks drives the ready queue until the root completes or one resume
// panics. A recovered panic returns to run, which starts another queue-driving
// episode after this native activation has unwound.
func (s *stacklessCoroScheduler) runTasks(native bool) {
	var task *stacklessCoroTask
	defer func() {
		if task == nil || task.context.scheduler != s {
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
		s.readyAfterPanic(task)
	}()

	for !s.rootComplete() {
		task = s.take()
		if task == nil {
			if s.executorStop == nil {
				<-s.wake
			} else {
				select {
				case <-s.wake:
				case <-s.executorStop:
					return
				}
			}
			continue
		}
		task.context.scheduler = s
		action := task.resume(unsafe.Pointer(&task.context))
		task.context.scheduler = nil

		switch action {
		case stacklessCoroActionYield:
			foreignReturner := s.yield(task)
			if !native || foreignReturner {
				// Cooperate with the host scheduler when this target has no
				// native executor implementation or a foreign-call executor
				// needs a P to return from syscall state.
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

// coroFrame returns the typed-frame pointer carried by a resume packet.
// Compiler-generated explicit resume functions use the first packet word
// directly; this helper keeps tests and runtime validation independent of
// that layout.
func coroFrame(ctx unsafe.Pointer) unsafe.Pointer {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine frame query")
	}
	return context.frame
}

// coroFrameCached reports whether the current typed frame owns bounded cache
// capacity. Compiler-generated completion paths clear its pointer fields
// before the runtime retains it.
func coroFrameCached(ctx unsafe.Pointer) bool {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil ||
		task.state != stacklessCoroTaskRunning || !task.resuming {
		throw("runtime: invalid stackless coroutine frame-cache query")
	}
	return task.cacheFrame
}

// coroTakeFrame reserves a task for a compiler-generated frame factory and
// returns a cached frame when one has the same resume entry and size. The
// reservation keeps the opaque typed allocation reachable until the matching
// await or spawn operation consumes it.
func coroTakeFrame(ctx unsafe.Pointer, child stacklessCoroResume,
	size uintptr) unsafe.Pointer {
	if ctx == nil || raceenabled || size == 0 ||
		size > stacklessCoroFrameCacheSize {
		return nil
	}
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context.scheduler == nil || parent == nil || child == nil {
		throw("runtime: invalid stackless coroutine frame reservation")
	}
	s := context.scheduler
	// A marked explicit-frame task carries a saturated lineage without a
	// shared-state query. A root may fill the cache with sibling spawns, so it
	// consults the saturation hint before entering the scheduler.
	if parent.frameSize == stacklessCoroUncachedFrameLineage {
		return nil
	}
	if parent == &s.root && s.frameCacheSaturated() {
		return nil
	}
	lock(&s.lock)
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine frame reservation outside resume")
	}
	task := s.takeCachedFrameTaskLocked(child, uint16(size))
	if task == nil {
		task = s.newTaskLocked(nil, child, parent)
		if s.cachedFrameTasks < stacklessCoroTaskCacheSize &&
			uintptr(s.cachedFrameBytes)+size <= stacklessCoroFrameCacheSize {
			task.cacheFrame = true
			task.frameSize = uint16(size)
			s.cachedFrameTasks++
			s.cachedFrameBytes += uint16(size)
			if s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
				s.freeFrameBytes == 0 {
				s.setFrameCacheSaturatedLocked(true)
			}
		} else if s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
			s.freeFrameBytes == 0 {
			task.frameSize = stacklessCoroUncachedFrameLineage
		}
	} else {
		task.parent = parent
	}
	task.next = s.reservedTasks
	s.reservedTasks = task
	frame := task.context.frame
	unlock(&s.lock)
	return frame
}

// markUncachedFrameLineageLocked records a saturated explicit-frame lineage
// after a factory bypasses its reservation. The scheduler lock must be held.
func (s *stacklessCoroScheduler) markUncachedFrameLineageLocked(task,
	parent *stacklessCoroTask, frame unsafe.Pointer) {
	if frame == nil || task.cacheFrame ||
		task.frameSize == stacklessCoroUncachedFrameLineage {
		return
	}
	if parent.frameSize == stacklessCoroUncachedFrameLineage ||
		(parent == &s.root &&
			s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
			s.freeFrameBytes == 0) {
		task.frameSize = stacklessCoroUncachedFrameLineage
	}
}

func (s *stacklessCoroScheduler) takeCachedFrameTaskLocked(
	resume stacklessCoroResume, size uint16) *stacklessCoroTask {
	identity := stacklessCoroResumeIdentity(resume)
	var previous *stacklessCoroTask
	for task := s.freeTasks; task != nil; task = task.next {
		if !task.cacheFrame || task.frameSize != size ||
			stacklessCoroResumeIdentity(task.resume) != identity {
			previous = task
			continue
		}
		if previous == nil {
			s.freeTasks = task.next
		} else {
			previous.next = task.next
		}
		if s.freeFrameBytes < task.frameSize ||
			s.cachedFrameTasks == 0 ||
			s.cachedFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine frame cache")
		}
		s.freeFrameBytes -= task.frameSize
		if s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
			s.freeFrameBytes == 0 {
			s.setFrameCacheSaturatedLocked(true)
		}
		task.next = nil
		return task
	}
	return nil
}

func (s *stacklessCoroScheduler) takeReservedFrameTaskLocked(
	parent *stacklessCoroTask, frame unsafe.Pointer,
	resume stacklessCoroResume) *stacklessCoroTask {
	if s.reservedTasks == nil {
		return nil
	}
	identity := stacklessCoroResumeIdentity(resume)
	var previous *stacklessCoroTask
	for task := s.reservedTasks; task != nil; task = task.next {
		if task.parent != parent ||
			stacklessCoroResumeIdentity(task.resume) != identity {
			previous = task
			continue
		}
		if task.state != stacklessCoroTaskNew ||
			task.context.scheduler != nil ||
			(task.context.frame != nil && task.context.frame != frame) {
			throw("runtime: invalid stackless coroutine frame reservation")
		}
		if previous == nil {
			s.reservedTasks = task.next
		} else {
			previous.next = task.next
		}
		task.next = nil
		task.context.frame = frame
		return task
	}
	return nil
}

// ABI 3 factories return static, capture-free resume values. Their funcval
// pointers are stable identities and avoid resolving a PC at every child
// transition.
func stacklessCoroResumeIdentity(resume stacklessCoroResume) *funcval {
	return *(**funcval)(unsafe.Pointer(&resume))
}

func (s *stacklessCoroScheduler) cancelReservedFrameTasksLocked(
	parent *stacklessCoroTask) {
	reserved := s.reservedTasks
	s.reservedTasks = nil
	for task := reserved; task != nil; {
		next := task.next
		task.next = nil
		if task.parent != parent {
			task.next = s.reservedTasks
			s.reservedTasks = task
			task = next
			continue
		}
		task.parent = nil
		task.state = stacklessCoroTaskComplete
		s.discardCachedFrameTaskLocked(task)
		s.recycleTaskLocked(task)
		task = next
	}
}

// coroAwait transfers execution to child. Completion makes the current task
// runnable; it does not call the parent continuation.
func coroAwait(ctx unsafe.Pointer, child stacklessCoroResume) {
	coroAwaitFrame(ctx, nil, child)
}

func coroAwaitFrame(ctx, frame unsafe.Pointer, child stacklessCoroResume) {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context == nil || context.scheduler == nil || parent == nil ||
		child == nil {
		throw("runtime: invalid stackless coroutine await")
	}
	s := context.scheduler
	lock(&s.lock)
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine await outside resume")
	}
	task := s.takeReservedFrameTaskLocked(parent, frame, child)
	if task == nil {
		task = s.newTaskLocked(frame, child, parent)
	}
	s.markUncachedFrameLineageLocked(task, parent, frame)
	parent.state = stacklessCoroTaskWaiting
	s.readyLocked(task)
	unlock(&s.lock)
}

// coroPanic records a panic value before the compiler-generated resume enters
// its frame-owned cleanup state.
func coroPanic(ctx unsafe.Pointer, value any) {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine panic")
	}
	recordStacklessCoroPanic(context.scheduler, task, value, false)
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
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine panic query")
	}
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending {
		throw("runtime: stackless coroutine panic query outside resume")
	}
	return task.terminal == stacklessCoroTerminalPanic
}

// coroGoexit starts Goexit cleanup for the running logical goroutine.
func coroGoexit(ctx unsafe.Pointer) {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine Goexit")
	}
	s := context.scheduler
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
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine terminal query")
	}
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
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil ||
		task.state != stacklessCoroTaskRunning ||
		!task.resuming || task.readyPending {
		throw("runtime: invalid stackless coroutine defer token")
	}
	return unsafe.Pointer(task)
}

func activeStacklessCoroDeferTask(token unsafe.Pointer,
	message string) *stacklessCoroTask {
	task := (*stacklessCoroTask)(token)
	if task == nil || task.context.scheduler == nil ||
		task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending {
		throw(message)
	}
	return task
}

type stacklessCoroDeferScope struct {
	gp        *g
	state     *stacklessCoroGState
	previous  *stacklessCoroTask
	temporary bool
}

func enterStacklessCoroDeferScope(task *stacklessCoroTask) stacklessCoroDeferScope {
	gp := getg()
	state := gp.stacklessCoro
	temporary := state == nil
	if temporary {
		state = new(stacklessCoroGState)
		gp.stacklessCoro = state
	}
	previous := state.deferTask
	state.deferTask = task
	return stacklessCoroDeferScope{
		gp: gp, state: state, previous: previous, temporary: temporary,
	}
}

func (scope *stacklessCoroDeferScope) leave() {
	scope.state.deferTask = scope.previous
	if scope.temporary {
		scope.gp.stacklessCoro = nil
	}
}

// coroDeferCall invokes a statically proven named defer target with access to
// its task-owned panic. Ordinary native panics remain visible to gorecover
// before this logical panic.
func coroDeferCall(token unsafe.Pointer, deferred func()) {
	task := activeStacklessCoroDeferTask(token,
		"runtime: invalid stackless coroutine defer call")
	scope := enterStacklessCoroDeferScope(task)
	defer scope.leave()
	deferred()
}

// coroDeferRun invokes a statically resolved deferred function through its
// coroutine factory. The nested scheduler is restricted by compiler proof to
// run-to-completion work, so it cannot leave detached tasks or operations.
func coroDeferRun(token unsafe.Pointer, resume stacklessCoroResume) {
	coroDeferRunFrame(token, nil, resume)
}

func coroDeferRunFrame(token, frame unsafe.Pointer,
	resume stacklessCoroResume) {
	task := activeStacklessCoroDeferTask(token,
		"runtime: invalid stackless coroutine defer run")
	scope := enterStacklessCoroDeferScope(task)
	defer scope.leave()

	s := newStacklessCoroScheduler(frame, resume)
	s.run(false)
	if raceenabled {
		raceacquire(unsafe.Pointer(&s.root))
	}
	kind, goexit, value, reason :=
		takeStacklessCoroDeferOutcome(s)
	if reason != "" {
		throw(reason)
	}

	parent := task.context.scheduler
	lock(&parent.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		(task.terminal != stacklessCoroTerminalNone &&
			task.terminal != stacklessCoroTerminalPanic) {
		unlock(&parent.lock)
		throw("runtime: invalid stackless coroutine defer merge")
	}
	switch kind {
	case stacklessCoroTerminalNone:
		if goexit {
			task.terminal = stacklessCoroTerminalNone
			delete(parent.terminalValues, task)
			task.goexit = true
		}
	case stacklessCoroTerminalPanic:
		task.terminal = stacklessCoroTerminalPanic
		task.goexit = task.goexit || goexit
		if parent.terminalValues == nil {
			parent.terminalValues = make(map[*stacklessCoroTask]any)
		}
		parent.terminalValues[task] = value
	}
	unlock(&parent.lock)
}

func takeStacklessCoroDeferOutcome(s *stacklessCoroScheduler) (
	kind stacklessCoroTerminalKind, goexit bool, value any, reason string) {
	root := &s.root
	kind = root.terminal
	goexit = root.goexit
	value, ok := s.terminalValues[root]
	if root.state != stacklessCoroTaskComplete {
		return 0, false, nil,
			"runtime: incomplete stackless coroutine defer run"
	}
	switch kind {
	case stacklessCoroTerminalNone:
		if ok {
			return 0, false, nil,
				"runtime: unexpected stackless coroutine defer value"
		}
	case stacklessCoroTerminalPanic:
		if !ok {
			return 0, false, nil,
				"runtime: missing stackless coroutine defer panic value"
		}
	default:
		return 0, false, nil,
			"runtime: invalid stackless coroutine defer outcome"
	}
	root.terminal = stacklessCoroTerminalNone
	root.goexit = false
	delete(s.terminalValues, root)
	return kind, goexit, value, ""
}

// coroDeferGoexit replaces a running task's panic with a sticky Goexit. The
// compiler emits an immediate return after this call, allowing the deferred
// function's own native defers to run without executing its remaining body.
func coroDeferGoexit(token unsafe.Pointer) {
	task := activeStacklessCoroDeferTask(token,
		"runtime: invalid stackless coroutine defer Goexit")
	s := task.context.scheduler
	lock(&s.lock)
	if task.terminal != stacklessCoroTerminalNone &&
		task.terminal != stacklessCoroTerminalPanic {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine defer Goexit transition")
	}
	task.terminal = stacklessCoroTerminalNone
	delete(s.terminalValues, task)
	task.goexit = true
	unlock(&s.lock)
}

// coroDeferPanic starts or replaces the panic owned by a running task.
func coroDeferPanic(token unsafe.Pointer, value any) {
	task := (*stacklessCoroTask)(token)
	if task == nil || task.context.scheduler == nil {
		throw("runtime: invalid stackless coroutine defer panic")
	}
	recordStacklessCoroPanic(task.context.scheduler, task, value, true)
}

// coroDeferRecover takes the panic owned by a running task. The compiler only
// emits this call for a direct recover expression in the active defer body.
func coroDeferRecover(token unsafe.Pointer) any {
	task := (*stacklessCoroTask)(token)
	if task == nil || task.context.scheduler == nil {
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
	coroSpawnFrame(ctx, nil, child)
}

func coroSpawnFrame(ctx, frame unsafe.Pointer, child stacklessCoroResume) {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context == nil || context.scheduler == nil || parent == nil ||
		child == nil {
		throw("runtime: invalid stackless coroutine spawn")
	}
	s := context.scheduler
	lock(&s.lock)
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending || parent.terminal != stacklessCoroTerminalNone ||
		parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine spawn outside resume")
	}
	task := s.takeReservedFrameTaskLocked(parent, frame, child)
	if task == nil {
		task = s.newTaskLocked(frame, child, nil)
	} else {
		task.parent = nil
	}
	s.markUncachedFrameLineageLocked(task, parent, frame)
	s.readyLocked(task)
	unlock(&s.lock)
	s.prepareReplacementExecutors()
	s.wakeReplacementExecutor()
}

// coroSleep starts a timer operation for the current logical goroutine and
// reports whether the task must wait for it.
func coroSleep(ctx unsafe.Pointer, ns int64) bool {
	if ns <= 0 {
		return false
	}
	startStacklessCoroTimer(ctx, ns)
	return true
}

// coroChanSend starts a channel send for a stackless logical goroutine.
func coroChanSend(ctx unsafe.Pointer, channel *hchan, element unsafe.Pointer) {
	startStacklessCoroChannel(ctx, channel, element, nil, true)
}

// coroChanRecv starts a channel receive for a stackless logical goroutine.
// received is optional and records the comma-ok result.
func coroChanRecv(ctx unsafe.Pointer, channel *hchan, element unsafe.Pointer, received *bool) {
	startStacklessCoroChannel(ctx, channel, element, received, false)
}

// coroSelect starts a channel select for a stackless logical goroutine.
func coroSelect(ctx unsafe.Pointer, cases *scase, nsends, nrecvs int, block bool,
	chosen *int, received *bool) {
	startStacklessCoroSelect(ctx, cases, nsends, nrecvs, block, chosen, received)
}

func startStacklessCoroChannel(ctx unsafe.Pointer, channel *hchan,
	element unsafe.Pointer, received *bool, send bool) {
	op := stacklessCoroStartOperation(ctx, "channel")
	op.channel = channel
	op.element = element
	op.received = received
	op.send = send
	op.id = registerStacklessCoroOperation(op)
	if send {
		chansendStackless(op)
	} else {
		chanrecvStackless(op)
	}
}

func finishStacklessCoroChannel(owner unsafe.Pointer, waiter *sudog, success bool) {
	op := (*stacklessCoroOperation)(owner)
	if op != nil && op.selection != nil {
		finishStacklessCoroSelect(op, waiter, success)
		return
	}
	if op == nil || takeStacklessCoroOperation(op.id) != op {
		throw("runtime: invalid stackless coroutine channel completion")
	}
	if waiter != nil {
		waiter.coro.clear()
		waiter.c.set(nil)
		releaseStacklessCoroSudog(waiter)
	}
	if op.timerWait {
		unblockTimerChan(op.channel)
		op.timerWait = false
	}
	send := op.send
	if !send && op.received != nil {
		*op.received = success
	}
	if send && !success {
		panicStacklessCoroOperation(op, plainError("send on closed channel"))
		return
	}
	completeStacklessCoroOperation(op)
}

func startStacklessCoroTimer(ctx unsafe.Pointer, ns int64) uint64 {
	op := stacklessCoroStartOperation(ctx, "sleep")
	id := registerStacklessCoroOperation(op)
	t := new(timer)
	t.init(stacklessCoroTimerReady, id)
	op.timer = t

	when := nanotime()
	if ns > 0 {
		when += ns
		if when < 0 {
			when = maxWhen
		}
	}
	t.reset(when, 0)
	return id
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
	completeStacklessCoroOperation(op)
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

// coroEnterBlocking reserves replacement capacity, saves its caller as the
// syscall continuation, and enters foreign-call state. If another logical
// task can run, it promptly hands the P to replacement capacity. Its caller
// is the generated resume frame and remains active until coroExitBlocking.
// Saving the helper frame itself would leave exitsyscall with an invalid
// continuation after this function returns.
//
//go:nosplit
func coroEnterBlocking(ctx unsafe.Pointer) {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine blocking call")
	}
	scheduler := context.scheduler
	tracked := scheduler.executorState.Load() == stacklessCoroExecutorStateRunning
	handoff := tracked && scheduler.enterBlockingExecutor()

	// Keep this state transition in sync with coroEnterForeign. It is
	// open-coded so the blocking path crosses one runtime boundary.
	gp := getg()
	mp := gp.m
	if mp.incgo || gp.nocgocallback {
		throw("runtime: nested stackless coroutine foreign call")
	}
	state := gp.stacklessCoro
	if tracked && state == nil {
		throw("runtime: missing stackless coroutine executor state")
	}
	if state != nil && state.blockingScheduler != nil {
		throw("runtime: nested stackless coroutine blocking call")
	}
	if tracked {
		state.blockingScheduler = scheduler
	}
	mp.ncgocall++
	if mp.cgoCallers != nil {
		mp.cgoCallers[0] = 0
	}

	pc := sys.GetCallerPC()
	sp := sys.GetCallerSP()
	bp := getcallerfp()
	if handoff {
		reentersyscallblock(pc, sp, bp)
	} else {
		reentersyscall(pc, sp, bp)
	}

	osPreemptExtEnter(mp)
	gp.nocgocallback = true
	mp.incgo = true
	mp.ncgo++
}

//go:nosplit
func coroExitBlocking() {
	// Keep this state transition in sync with coroExitForeign. It is
	// open-coded so the blocking path crosses one runtime boundary.
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
	exitsyscall()
	gp = getg()
	state := gp.stacklessCoro
	if state != nil && state.blockingScheduler != nil {
		scheduler := state.blockingScheduler
		state.blockingScheduler = nil
		scheduler.leaveBlockingExecutor()
	}
}

func stacklessCoroStartOperation(ctx unsafe.Pointer, name string) *stacklessCoroOperation {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine " + name)
	}
	s := context.scheduler
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine operation outside resume")
	}
	op := s.freeOperations
	if op == nil {
		if s.freeOperationCount != 0 {
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine operation cache")
		}
	} else {
		if s.freeOperationCount == 0 ||
			s.freeOperationCount > stacklessCoroOperationCacheSize {
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine operation cache count")
		}
		s.freeOperations = op.next
		s.freeOperationCount--
		op.next = nil
	}
	task.state = stacklessCoroTaskWaiting
	unlock(&s.lock)
	if op == nil {
		op = new(stacklessCoroOperation)
	}
	op.scheduler = s
	op.task = task
	return op
}

func (s *stacklessCoroScheduler) ready(task *stacklessCoroTask, signal bool) {
	lock(&s.lock)
	s.readyLocked(task)
	unlock(&s.lock)
	if signal {
		s.signal()
	}
}

func clearStacklessCoroOperation(op *stacklessCoroOperation) (*stacklessCoroScheduler, *stacklessCoroTask) {
	if op == nil || op.id == 0 || op.scheduler == nil || op.task == nil ||
		op.timer != nil || op.timerWait || op.next != nil || op.workNext != nil {
		throw("runtime: invalid completed stackless coroutine operation")
	}
	s := op.scheduler
	task := op.task
	*op = stacklessCoroOperation{}
	return s, task
}

// recycleOperationLocked retains a completed operation for this scheduler.
// The operation address is a race-detector synchronization identity for
// channel operations, so race builds must not reuse it for another operation.
func (s *stacklessCoroScheduler) recycleOperationLocked(op *stacklessCoroOperation) {
	if raceenabled {
		return
	}
	if s.freeOperationCount > stacklessCoroOperationCacheSize {
		throw("runtime: invalid stackless coroutine operation cache size")
	}
	if s.freeOperationCount == stacklessCoroOperationCacheSize {
		return
	}
	op.next = s.freeOperations
	s.freeOperations = op
	s.freeOperationCount++
}

func completeStacklessCoroOperation(op *stacklessCoroOperation) {
	s, task := clearStacklessCoroOperation(op)
	lock(&s.lock)
	s.readyLocked(task)
	s.recycleOperationLocked(op)
	unlock(&s.lock)
	s.signal()
}

func panicStacklessCoroOperation(op *stacklessCoroOperation, value any) {
	s, task := clearStacklessCoroOperation(op)
	lock(&s.lock)
	if task.state != stacklessCoroTaskWaiting ||
		task.terminal != stacklessCoroTerminalNone || task.goexit ||
		task.readyPending {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine operation panic")
	}
	task.terminal = stacklessCoroTerminalPanic
	if s.terminalValues == nil {
		s.terminalValues = make(map[*stacklessCoroTask]any)
	}
	s.terminalValues[task] = value
	s.readyLocked(task)
	s.recycleOperationLocked(op)
	unlock(&s.lock)
	s.signal()
}

func (s *stacklessCoroScheduler) readyLocked(task *stacklessCoroTask) uint32 {
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
		return 0
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
	runnable := s.runnableState.Add(1)
	if raceenabled {
		// Runtime mutex operations are invisible to the race detector.
		// Merge both the previous resume episode and the producer that made
		// this task runnable into the next executor's acquire.
		racereleasemerge(unsafe.Pointer(task))
	}
	return runnable
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
	s.runnableState.Add(-1)
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

func (s *stacklessCoroScheduler) yield(task *stacklessCoroTask) bool {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine yield")
	}
	task.resuming = false
	runnable := s.readyLocked(task)
	unlock(&s.lock)
	return runnable&stacklessCoroForeignReturnerBit != 0
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
	s.cancelReservedFrameTasksLocked(task)
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
	if !task.cacheFrame {
		clearStacklessCoroTaskFrame(task)
	}
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	parent := task.parent
	task.parent = nil
	if parent == nil {
		if task != &s.root {
			s.recycleTaskLocked(task)
		}
		unlock(&s.lock)
		if task == &s.root {
			s.stopExecutorRuns()
		} else {
			s.signalAll()
		}
		return
	}
	if parent.state != stacklessCoroTaskWaiting ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine completed for non-waiting parent")
	}
	s.readyLocked(parent)
	s.recycleTaskLocked(task)
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
	s.discardCachedFrameTaskLocked(task)
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	parent := task.parent
	task.parent = nil
	if parent == nil {
		if task != &s.root {
			delete(s.terminalValues, task)
			task.terminal = stacklessCoroTerminalNone
			task.goexit = false
			s.recycleTaskLocked(task)
			unlock(&s.lock)
			panic(value)
		}
		unlock(&s.lock)
		s.stopExecutorRuns()
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
	s.recycleTaskLocked(task)
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
	s.discardCachedFrameTaskLocked(task)
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	parent := task.parent
	task.parent = nil
	if parent == nil {
		if task == &s.root {
			unlock(&s.lock)
			s.stopExecutorRuns()
			return
		}
		task.goexit = false
		s.recycleTaskLocked(task)
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
	s.recycleTaskLocked(task)
	unlock(&s.lock)
}

func (s *stacklessCoroScheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *stacklessCoroScheduler) signalAll() {
	for range stacklessCoroWarmExecutorCount {
		s.signal()
	}
}

// wakeReplacementExecutor admits a warm replacement and wakes an admitted
// executor. A spawn may add runnable work while its current resume function
// continues to run, so the executor driving that function cannot consume the
// new task yet. With one P, admission would only add contention; blocking
// foreign calls admit replacement capacity through their own path.
func (s *stacklessCoroScheduler) wakeReplacementExecutor() {
	if gomaxprocs <= 1 {
		return
	}
	s.signal()
	select {
	case s.executorWake <- struct{}{}:
	default:
	}
}

func (s *stacklessCoroScheduler) prepareReplacementExecutors() {
	if !s.executorState.CompareAndSwap(stacklessCoroExecutorStateOff, stacklessCoroExecutorStatePreparing) {
		return
	}

	s.executorWake = make(chan struct{}, stacklessCoroWarmExecutorCount-1)
	s.executorDone = make(chan struct{})
	s.executorGrow = make(chan struct{}, 1)
	s.executorStop = make(chan struct{})
	s.executorManagerDone = make(chan struct{})
	s.executorCount.Store(stacklessCoroWarmExecutorCount)
	for range stacklessCoroWarmExecutorCount - 1 {
		go s.replacementExecutor(false)
	}
	go s.manageReplacementExecutors()
	s.executorState.Store(stacklessCoroExecutorStateRunning)
}

func (s *stacklessCoroScheduler) manageReplacementExecutors() {
	for {
		select {
		case <-s.executorGrow:
			for !s.rootComplete() {
				count := s.executorCount.Load()
				if s.blockingExecutors.Load() < count {
					break
				}
				if s.executorCount.Add(1) == 0 {
					throw("runtime: stackless coroutine executor count overflow")
				}
				go s.replacementExecutor(true)
			}
		case <-s.executorStop:
			close(s.executorManagerDone)
			return
		}
	}
}

func (s *stacklessCoroScheduler) replacementExecutor(start bool) {
	if !start {
		select {
		case <-s.executorWake:
		case <-s.executorStop:
		}
	}
	if !s.rootComplete() {
		if coroRunOnNativeStack(s) == nil {
			s.run(false)
		}
	}
	s.executorDone <- struct{}{}
}

// enterBlockingExecutor notifies replacement capacity and records an executor
// entering a blocking foreign call. It reports whether the executor should
// promptly release its P.
//
// coroEnterBlocking keeps schedulers without replacement executors on the
// ordinary direct-C path and does not call this slow path.
//
// Keep this path out of coroEnterBlocking so its common no-replacement case
// retains the compact direct-C entry sequence.
//
//go:noinline
//go:nosplit
func (s *stacklessCoroScheduler) enterBlockingExecutor() (handoff bool) {
	blocked := s.blockingExecutors.Add(1)
	shortage := blocked >= s.executorCount.Load()
	if shortage {
		select {
		case s.executorGrow <- struct{}{}:
		default:
		}
	}
	handoff = shortage || s.runnableState.Load() != 0
	// A replacement that has already entered run waits on wake, not
	// executorWake. Notify both admitted and not-yet-admitted warm executors.
	select {
	case s.wake <- struct{}{}:
	default:
	}
	select {
	case s.executorWake <- struct{}{}:
	default:
	}
	return handoff
}

//go:nosplit
func (s *stacklessCoroScheduler) leaveBlockingExecutor() {
	if s == nil || s.blockingExecutors.Add(-1) == ^uint32(0) {
		throw("runtime: unmatched stackless coroutine blocking executor")
	}
}

func (s *stacklessCoroScheduler) stopExecutorRuns() {
	state := s.executorState.Load()
	if state == stacklessCoroExecutorStateOff {
		return
	}
	if state == stacklessCoroExecutorStatePreparing {
		throw("runtime: stackless coroutine stopped while preparing executors")
	}
	if state == stacklessCoroExecutorStateRunning &&
		s.executorState.CompareAndSwap(stacklessCoroExecutorStateRunning, stacklessCoroExecutorStateStopping) {
		close(s.executorStop)
	}
}

func (s *stacklessCoroScheduler) stopReplacementExecutors() {
	if s.executorState.Load() == stacklessCoroExecutorStateOff {
		return
	}
	s.stopExecutorRuns()
	<-s.executorManagerDone
	for range s.executorCount.Load() - 1 {
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
	completeStacklessCoroOperation(op)
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
