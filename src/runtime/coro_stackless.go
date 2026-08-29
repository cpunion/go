// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
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
	stacklessCoroActionSwitch
)

const stacklessCoroWarmExecutorCount = 4
const stacklessCoroTaskCacheSize = 256

// Four tasks occupy the exact 192-byte size class on 64-bit targets and the
// exact 112-byte size class on 32-bit targets. Both remain below the per-object
// malloc-header threshold.
const stacklessCoroTaskChunkSize = 4
const stacklessCoroFrameChunkSize = 4

const (
	stacklessCoroDirectOverflowTaskCountMask uint8 = 1<<4 - 1
	stacklessCoroFreeOverflowTaskCountShift        = 4
)

// Start typed-frame chunks two tasks after the task-chunk boundary. The
// stagger keeps their first-element allocation charges from occurring at the
// same recursive depth.
const stacklessCoroFrameChunkDirectCount = 6
const stacklessCoroFrameCacheSize = 32 << 10
const stacklessCoroOperationCacheSize = 64

// Share operation storage above a scheduler's local bound. Together the two
// caches cover the bounded task cache without retaining the larger operation
// resources separately in every scheduler.
const stacklessCoroSharedOperationCacheSize = stacklessCoroTaskCacheSize -
	stacklessCoroOperationCacheSize

// A fused structured-await chain uses the same direct prefix as ordinary
// recursive frame chunks. The low bits describe allocation; the high bits
// retain the suspended frame state needed when resumes are heterogeneous.
const (
	stacklessCoroFusedFrameDirectFirst uint8 = 1
	stacklessCoroFusedFrameDirectLast        = stacklessCoroFusedFrameDirectFirst + stacklessCoroFrameChunkDirectCount - 1
	stacklessCoroFusedFrameChunkFirst        = stacklessCoroFusedFrameDirectLast + 1
	stacklessCoroFusedFrameChunkLast         = stacklessCoroFusedFrameChunkFirst +
		stacklessCoroFrameChunkSize - 1
	stacklessCoroFusedFrameAllocationMask uint8 = 1<<4 - 1
	stacklessCoroFusedFrameParent         uint8 = 1 << 6
	stacklessCoroFusedFrameResume         uint8 = 1 << 7
)

// A short bounded retry window covers local pipe and loopback delivery without
// turning a genuinely external read into polling.
const stacklessCoroIdlePollAttempts = 4

// Limit a cold-path registry scan to one scheduler's bounded operation cache.
// Missing a read only preserves the ordinary netpoll wake path.
const stacklessCoroIdlePollScanLimit = stacklessCoroOperationCacheSize

// stacklessCoroUncachedFrameLineage marks an explicit-frame task that was
// created after the task cache saturated. It cannot be a cached frame size.
const stacklessCoroUncachedFrameLineage = ^uint16(0)

// stacklessCoroFreeOverflowTask marks a never-used task slot from a saturated
// allocation chunk while that slot is linked on the scheduler free list.
const stacklessCoroFreeOverflowTask = stacklessCoroUncachedFrameLineage - 1

// A recursive typed-frame chunk uses task-local markers. Direct markers count
// the single-frame prefix. Element markers identify a frame whose following
// recursive child can use the adjacent element of the same typed allocation.
const (
	stacklessCoroFrameChunkDirectFirst = stacklessCoroFreeOverflowTask - 1
	stacklessCoroFrameChunkDirectLast  = stacklessCoroFrameChunkDirectFirst -
		stacklessCoroFrameChunkDirectCount
	stacklessCoroFrameChunkFirst = stacklessCoroFrameChunkDirectLast - 1
	stacklessCoroFrameChunkLast  = stacklessCoroFrameChunkFirst -
		(stacklessCoroFrameChunkSize - 1)
)
const stacklessCoroForeignReturnerBit = uint32(1 << 31)
const stacklessCoroDeferredSleepOperationID = ^uint64(0)

const (
	stacklessCoroExecutorStateOff uint32 = iota
	stacklessCoroExecutorStatePreparing
	stacklessCoroExecutorStateRunning
	stacklessCoroExecutorStateStopping
)

const (
	stacklessCoroLifetimeOperationStarted uint8 = 1 << iota
	stacklessCoroLifetimeRootComplete
)

type stacklessCoroTaskState uint8

const (
	stacklessCoroTaskNew stacklessCoroTaskState = iota
	stacklessCoroTaskRunnable
	stacklessCoroTaskRunning
	stacklessCoroTaskWaiting
	stacklessCoroTaskComplete
)

type stacklessCoroTaskFlags uint8

const (
	stacklessCoroTaskCacheFrame stacklessCoroTaskFlags = 1 << iota
	stacklessCoroTaskFusedFrames
	stacklessCoroTaskFusedRequested
	stacklessCoroTaskFusedPending
	stacklessCoroTaskSwitchPending
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
	flags        stacklessCoroTaskFlags
	frameSize    uint16
	context      stacklessCoroContext
}

func (task *stacklessCoroTask) hasFlag(flag stacklessCoroTaskFlags) bool {
	return task.flags&flag != 0
}

func (task *stacklessCoroTask) setFlag(flag stacklessCoroTaskFlags,
	set bool) {
	if set {
		task.flags |= flag
	} else {
		task.flags &^= flag
	}
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

// stacklessCoroFusedFrameHeader is the prefix of a compiler-generated frame
// that can share one logical task across a structured await chain. parent
// keeps the suspended frame live. owner retains a cache-owned frame holder
// when one exists. marker records allocation and suspended-parent state;
// logical scheduling state stays in the shared task.
type stacklessCoroFusedFrameHeader struct {
	parent unsafe.Pointer
	owner  *stacklessCoroTask
	marker uint8
}

// stacklessCoroFusedResumeFrameHeader extends the common prefix only when a
// local structured await changes resume entry. Self-await frames keep the
// smaller common prefix. ABI 3 resumes are static capture-free funcvals, so a
// single scanned pointer is sufficient to restore the suspended entry.
type stacklessCoroFusedResumeFrameHeader struct {
	stacklessCoroFusedFrameHeader
	resume *funcval
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
	freeOperationCount uint8
	// overflowTaskCounts stores the direct allocation prefix in its low
	// nibble and free slots in its high nibble. Both counts are bounded by the
	// task chunk size.
	overflowTaskCounts uint8
	// initialRootPrivate is cleared once by the initial driver, under the
	// scheduler lock, before an operation or non-root runnable is published.
	initialRootPrivate bool
	// lifetime publishes monotonic facts for one initialized scheduler
	// lifetime. Root completion is published only after its outcome is ready.
	lifetime       atomic.Uint8
	root           stacklessCoroTask
	terminalValues map[*stacklessCoroTask]any
	wake           chan struct{}
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
	// drivers that entered a blocking native operation but have not yet
	// returned with a P.
	executorCount     atomic.Uint32
	blockingExecutors atomic.Uint32
	// runnableState stores the runnable count in its low 31 bits. The high
	// bit asks a native executor to yield its P to a returning foreign call.
	runnableState    atomic.Uint32
	foreignReturners atomic.Uint32
}

// A stacklessCoroOperation retains reusable resources after clearing the state
// owned by a completed source. Keeping those resources outside state avoids a
// transient unrooted pointer while a native executor clears the operation.
type stacklessCoroOperation struct {
	stacklessCoroOperationState
	timer     *stacklessCoroTimer
	waiter    *sudog
	selection *stacklessCoroSelect
}

// A stacklessCoroOperationState is owned by its source until that source
// publishes a terminal fact. Registered sources carry only id. Timers use a
// stable owner and generation so a late callback cannot complete a reused
// operation.
type stacklessCoroOperationState struct {
	id        uint64
	scheduler *stacklessCoroScheduler
	task      *stacklessCoroTask
	channel   *hchan
	element   unsafe.Pointer
	received  *bool
	send      bool
	timerWait bool
	// waiterActive keeps the retained waiter rooted by waiter while it is
	// also linked from a channel queue.
	waiterActive bool
	fd           int32
	buffer       []byte
	call         func()
	n            *int
	errno        *uintptr
	valueOut     *uint64
	// packet holds a deferred-sleep deadline, an asynchronous C reply, or a
	// socket read's pointer-free poll descriptor and completion link. The
	// operation registry remains the GC root for a linked socket operation.
	packet [2]uint64
	async  bool
	// ownsPollDesc distinguishes the explicit raw-descriptor adapter from a
	// descriptor borrowed under its library owner's lifetime locks.
	ownsPollDesc bool
	next         *stacklessCoroOperation
	workNext     *stacklessCoroOperation
}

// A stacklessCoroTimer keeps a runtime timer stable across operation reuse.
// active is the generation that may publish completion. A callback from an
// earlier generation observes a different value and does nothing.
type stacklessCoroTimer struct {
	timer     timer
	operation *stacklessCoroOperation
	sequence  uintptr
	active    atomic.Uintptr
}

type stacklessCoroTimerToken struct {
	timer    *stacklessCoroTimer
	sequence uintptr
}

var stacklessCoroOperations struct {
	lock mutex
	next uint64
	head *stacklessCoroOperation
}

// stacklessCoroSharedOperations retains completed operations that exceeded a
// scheduler's local cache. The process-wide bound keeps this overflow shared
// by concurrent schedulers.
var stacklessCoroSharedOperations struct {
	lock  mutex
	head  *stacklessCoroOperation
	count uint8
}

// The bounded wake pool retains channels for nested schedulers only after
// every executor for their previous scheduler has stopped. Race builds keep a
// distinct synchronization identity for each scheduler instead.
var stacklessCoroWakePool struct {
	available chan chan struct{}
}

// Operation-free public roots may reuse a scheduler and its wake channel only
// after every replacement executor has stopped and the root outcome has been
// transferred to its caller. Fixed atomic slots keep cache lookup independent
// of channel scheduling. Race builds retain a distinct root synchronization
// identity instead.
var stacklessCoroRootSchedulerCache struct {
	slots [stacklessCoroWarmExecutorCount]atomic.Pointer[stacklessCoroScheduler]
}

// stacklessCoroRootFrame retains a compiler-generated typed allocation only
// after its public root has become quiescent. The frame pointer keeps the
// allocation's exact GC type alive while it is cached.
type stacklessCoroRootFrame struct {
	frame  unsafe.Pointer
	resume *funcval
	size   uint16
}

const (
	stacklessCoroRootFrameSlotEmpty uint32 = iota
	stacklessCoroRootFrameSlotBusy
	stacklessCoroRootFrameSlotReady
)

// stacklessCoroRootFrameSlot keeps cached in GC-visible pointer fields. State
// is the mutator publication barrier; a producer or consumer owns cached only
// while state is busy.
type stacklessCoroRootFrameSlot struct {
	state  atomic.Uint32
	cached stacklessCoroRootFrame
}

var stacklessCoroRootFrameCache struct {
	slots [stacklessCoroWarmExecutorCount]stacklessCoroRootFrameSlot
}

func init() {
	lockInit(&stacklessCoroOperations.lock, lockRankLeafRank)
	lockInit(&stacklessCoroSharedOperations.lock, lockRankLeafRank)
	stacklessCoroWakePool.available = make(chan chan struct{}, stacklessCoroWarmExecutorCount)
}

// coroRun drives one stackless logical goroutine and its children. The
// compiler emits calls to coroRun only for coroutine root adapters.
func coroRun(resume stacklessCoroResume) {
	coroRunFrame(nil, resume, 0)
}

func coroRunFrame(frame unsafe.Pointer, resume stacklessCoroResume,
	frameSize uintptr) bool {
	s := acquireStacklessCoroRootScheduler(frame, resume, frameSize)
	s = runStacklessCoroRoot(s)
	s.stopReplacementExecutors()
	return finishStacklessCoroRootScheduler(s)
}

// runStacklessCoroRoot drives native fixed-stack episodes from an ordinary G.
// Keeping the ordinary G parked between episodes lets its M run unrelated
// work while the root waits for a timer or asynchronous operation.
func runStacklessCoroRoot(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	var native stacklessCoroNativeDriver
	initial := true
	for {
		nativeScheduler := native.run(s, initial)
		if nativeScheduler == nil {
			native.close(s, false)
			if initial {
				s.runInitial()
			} else {
				s.run(false)
			}
			return s
		}
		s = nativeScheduler
		initial = false
		if s.rootComplete() || !native.waitForWork(s) {
			native.close(s, true)
			return s
		}
	}
}

func newStacklessCoroScheduler(frame unsafe.Pointer,
	resume stacklessCoroResume) *stacklessCoroScheduler {
	return initializeStacklessCoroScheduler(
		new(stacklessCoroScheduler), frame, resume, 0)
}

func acquireStacklessCoroRootScheduler(frame unsafe.Pointer,
	resume stacklessCoroResume, frameSize uintptr) *stacklessCoroScheduler {
	if !raceenabled {
		for i := range stacklessCoroRootSchedulerCache.slots {
			slot := &stacklessCoroRootSchedulerCache.slots[i]
			s := slot.Load()
			if s != nil && slot.CompareAndSwap(s, nil) {
				return initializeStacklessCoroScheduler(s, frame, resume,
					frameSize)
			}
		}
	}
	s := new(stacklessCoroScheduler)
	// A root scheduler retains its wake channel when both become quiescent.
	// Allocate a distinct channel for a cold root instead of borrowing from
	// the pool used by short-lived nested schedulers.
	s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
	return initializeStacklessCoroScheduler(
		s, frame, resume, frameSize)
}

func initializeStacklessCoroScheduler(s *stacklessCoroScheduler,
	frame unsafe.Pointer, resume stacklessCoroResume,
	frameSize uintptr) *stacklessCoroScheduler {
	if resume == nil {
		throw("runtime: nil stackless coroutine resume function")
	}
	if s.wake == nil {
		s.wake = acquireStacklessCoroWake()
	}
	s.executorCount.Store(1)
	lockInit(&s.lock, lockRankLeafRank)
	root := &s.root
	root.resume = resume
	root.context.frame = frame
	if frame != nil && canCacheStacklessCoroRootFrame(resume, frameSize) {
		root.setFlag(stacklessCoroTaskCacheFrame, true)
		root.frameSize = uint16(frameSize)
	}
	// The scheduler remains private until initialization returns, so install
	// its initial runnable root without taking the queue lock.
	s.initialRootPrivate = true
	root.state = stacklessCoroTaskRunnable
	s.head = root
	s.tail = root
	s.runnableState.Store(1)
	if raceenabled {
		racereleasemerge(unsafe.Pointer(root))
	}
	return s
}

func finishStacklessCoroRootScheduler(s *stacklessCoroScheduler) (
	quiescent bool) {
	defer func() {
		if raceenabled {
			s.discardWake()
		} else {
			quiescent = releaseStacklessCoroRootScheduler(s)
		}
	}()
	s.finish()
	return
}

func releaseStacklessCoroRootScheduler(s *stacklessCoroScheduler) bool {
	// An operation producer can still hold a completed root scheduler after
	// removing its operation from the global registry. Detach its wake channel,
	// but do not inspect or reuse the remaining scheduler state in that case.
	if s.lifetime.Load()&stacklessCoroLifetimeOperationStarted != 0 {
		s.discardWake()
		return false
	}
	state := s.executorState.Load()
	runnable := s.runnableState.Load()
	quiescent := s.wake != nil && s.head == nil && s.tail == nil &&
		s.reservedTasks == nil && runnable == 0 &&
		s.root.state == stacklessCoroTaskComplete &&
		!s.root.resuming && !s.root.readyPending &&
		s.root.terminal == stacklessCoroTerminalNone && !s.root.goexit &&
		len(s.terminalValues) == 0 && s.blockingExecutors.Load() == 0 &&
		s.foreignReturners.Load() == 0 &&
		(state == stacklessCoroExecutorStateOff ||
			state == stacklessCoroExecutorStateStopping)
	// A remaining runnable task or frame reservation belongs to the old root.
	// Leave those schedulers on their existing allocation lifetime rather than
	// making their addresses available to another public root. Pooling is
	// optional, so any state not proven quiescent also keeps that lifetime.
	if !quiescent {
		s.discardWake()
		return false
	}
	wake := s.wake
	drainStacklessCoroWake(wake)
	*s = stacklessCoroScheduler{}
	s.wake = wake
	for i := range stacklessCoroRootSchedulerCache.slots {
		if stacklessCoroRootSchedulerCache.slots[i].CompareAndSwap(nil, s) {
			return true
		}
	}
	// Root wake channels belong to the scheduler cache as one unit. Do not
	// increase the separate nested-scheduler wake pool after a root cache miss
	// or a concurrency spike.
	s.discardWake()
	return true
}

func canCacheStacklessCoroRootFrame(resume stacklessCoroResume,
	size uintptr) bool {
	return !raceenabled && resume != nil && size != 0 &&
		size <= stacklessCoroFrameCacheSize
}

// coroTakeRootFrame returns a quiescent typed frame with the same resume
// identity and size. A miss leaves allocation to the generated factory.
func coroTakeRootFrame(resume stacklessCoroResume,
	size uintptr) unsafe.Pointer {
	if !canCacheStacklessCoroRootFrame(resume, size) {
		return nil
	}
	identity := stacklessCoroResumeIdentity(resume)
	for i := range stacklessCoroRootFrameCache.slots {
		slot := &stacklessCoroRootFrameCache.slots[i]
		if !slot.state.CompareAndSwap(stacklessCoroRootFrameSlotReady,
			stacklessCoroRootFrameSlotBusy) {
			continue
		}
		cached := slot.cached
		if cached.resume == identity && cached.size == uint16(size) {
			frame := cached.frame
			slot.cached = stacklessCoroRootFrame{}
			slot.state.Store(stacklessCoroRootFrameSlotEmpty)
			return frame
		}
		slot.state.Store(stacklessCoroRootFrameSlotReady)
	}
	return nil
}

// coroReleaseRootFrame publishes a frame only after coroRunFrame reported a
// quiescent root, so no scheduler or operation can retain its previous
// identity. The generated wrapper copies result values out before calling
// this function.
func coroReleaseRootFrame(frame unsafe.Pointer, resume stacklessCoroResume,
	size uintptr) {
	if frame == nil || !canCacheStacklessCoroRootFrame(resume, size) {
		return
	}
	cached := stacklessCoroRootFrame{
		frame:  frame,
		resume: stacklessCoroResumeIdentity(resume),
		size:   uint16(size),
	}
	for i := range stacklessCoroRootFrameCache.slots {
		slot := &stacklessCoroRootFrameCache.slots[i]
		if slot.state.CompareAndSwap(stacklessCoroRootFrameSlotEmpty,
			stacklessCoroRootFrameSlotBusy) {
			slot.cached = cached
			slot.state.Store(stacklessCoroRootFrameSlotReady)
			return
		}
	}
	// A full cache can otherwise retain an old set of resume identities
	// forever. Every ready entry is quiescent, so replace one with the newly
	// active identity. Cache loss when all slots are busy is harmless.
	for i := range stacklessCoroRootFrameCache.slots {
		slot := &stacklessCoroRootFrameCache.slots[i]
		if slot.state.CompareAndSwap(stacklessCoroRootFrameSlotReady,
			stacklessCoroRootFrameSlotBusy) {
			slot.cached = cached
			slot.state.Store(stacklessCoroRootFrameSlotReady)
			return
		}
	}
}

func acquireStacklessCoroWake() chan struct{} {
	if !raceenabled {
		select {
		case wake := <-stacklessCoroWakePool.available:
			return wake
		default:
		}
	}
	return make(chan struct{}, stacklessCoroWarmExecutorCount)
}

func drainStacklessCoroWake(wake chan struct{}) {
	// Callers detach wake or establish scheduler quiescence before draining,
	// so the length snapshot can skip the common empty-channel receive.
	if len(wake) == 0 {
		return
	}
	for {
		select {
		case <-wake:
			continue
		default:
			return
		}
	}
}

func (s *stacklessCoroScheduler) discardWake() {
	wake := s.wake
	s.wake = nil
	drainStacklessCoroWake(wake)
}

func (s *stacklessCoroScheduler) releaseWake() {
	wake := s.wake
	s.wake = nil
	if !raceenabled {
		drainStacklessCoroWake(wake)
		select {
		case stacklessCoroWakePool.available <- wake:
		default:
		}
	}
}

// newTaskLocked returns a new or recycled non-root task. The scheduler lock
// must be held.
func (s *stacklessCoroScheduler) newTaskLocked(frame unsafe.Pointer,
	resume stacklessCoroResume, parent *stacklessCoroTask) *stacklessCoroTask {
	if s.overflowTaskCounts >= 1<<stacklessCoroFreeOverflowTaskCountShift {
		return s.newTaskWithOverflowLocked(frame, resume, parent)
	}
	task := s.takeFreeTaskLocked()
	if task == nil {
		if frame == nil {
			return &stacklessCoroTask{
				resume: resume,
				parent: parent,
			}
		}
		return s.newTaskAfterCacheMissLocked(frame, resume, parent)
	}
	return initializeStacklessCoroTask(task, frame, resume, parent)
}

func (s *stacklessCoroScheduler) newTaskWithOverflowLocked(frame unsafe.Pointer,
	resume stacklessCoroResume, parent *stacklessCoroTask) *stacklessCoroTask {
	allowOverflow := !raceenabled && frame != nil && parent != nil &&
		isStacklessCoroUncachedFrameLineage(parent.frameSize)
	task := s.takeFreeOverflowTaskLocked(allowOverflow)
	if task == nil {
		if frame == nil {
			return &stacklessCoroTask{
				resume: resume,
				parent: parent,
			}
		}
		return s.newTaskAfterCacheMissLocked(frame, resume, parent)
	}
	return initializeStacklessCoroTask(task, frame, resume, parent)
}

func (s *stacklessCoroScheduler) newTaskAfterCacheMissLocked(
	frame unsafe.Pointer, resume stacklessCoroResume,
	parent *stacklessCoroTask) *stacklessCoroTask {
	uncachedLineage := !raceenabled && parent != nil &&
		isStacklessCoroUncachedFrameLineage(parent.frameSize)
	task := s.allocateTaskLocked(uncachedLineage)
	return initializeStacklessCoroTask(task, frame, resume, parent)
}

// newFrameReservationTaskLocked preserves completed typed frames while the
// bounded frame cache still has room for another resume identity.
func (s *stacklessCoroScheduler) newFrameReservationTaskLocked(
	resume stacklessCoroResume, parent *stacklessCoroTask,
	size uintptr) *stacklessCoroTask {
	cacheHasRoom := s.cachedFrameTasks < stacklessCoroTaskCacheSize &&
		uintptr(s.cachedFrameBytes)+size <= stacklessCoroFrameCacheSize
	if cacheHasRoom && s.freePlainTaskCount == 0 {
		return initializeStacklessCoroTask(new(stacklessCoroTask), nil,
			resume, parent)
	}
	return s.newTaskLocked(nil, resume, parent)
}

func initializeStacklessCoroTask(task *stacklessCoroTask, frame unsafe.Pointer,
	resume stacklessCoroResume, parent *stacklessCoroTask) *stacklessCoroTask {
	task.resume = resume
	task.parent = parent
	task.context.frame = frame
	task.flags = 0
	task.frameSize = 0
	return task
}

// allocateTaskLocked batches task storage only after an explicit-frame
// lineage has saturated the bounded task cache. A short direct-allocation
// prefix avoids a chunk-sized memory cliff immediately above the cache bound.
func (s *stacklessCoroScheduler) allocateTaskLocked(
	uncachedLineage bool) *stacklessCoroTask {
	if !uncachedLineage ||
		s.overflowTaskCounts&stacklessCoroDirectOverflowTaskCountMask <
			uint8(stacklessCoroTaskChunkSize) {
		if uncachedLineage {
			s.overflowTaskCounts++
		}
		return new(stacklessCoroTask)
	}

	tasks := new([stacklessCoroTaskChunkSize]stacklessCoroTask)
	for i := 1; i < len(tasks); i++ {
		task := &tasks[i]
		task.frameSize = stacklessCoroFreeOverflowTask
		task.next = s.freeTasks
		s.freeTasks = task
	}
	s.overflowTaskCounts |= uint8(stacklessCoroTaskChunkSize-1) <<
		stacklessCoroFreeOverflowTaskCountShift
	return &tasks[0]
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
		for task != nil && task.hasFlag(stacklessCoroTaskCacheFrame) {
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
	if task.hasFlag(stacklessCoroTaskCacheFrame) {
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

// takeFreeOverflowTaskLocked selects an ordinary task before an unused chunk
// slot. Unmarked lineages cannot acquire chunk storage. The scheduler lock
// must be held, and the free overflow task count must be nonzero.
func (s *stacklessCoroScheduler) takeFreeOverflowTaskLocked(
	allowOverflow bool) *stacklessCoroTask {
	task := s.freeTasks
	if task == nil ||
		s.overflowTaskCounts < 1<<stacklessCoroFreeOverflowTaskCountShift {
		throw("runtime: invalid stackless coroutine overflow task cache")
	}
	if s.freePlainTaskCount > stacklessCoroTaskCacheSize {
		throw("runtime: invalid stackless coroutine task cache count")
	}
	var previous *stacklessCoroTask
	switch {
	case s.freePlainTaskCount > 0:
		for task != nil &&
			(task.hasFlag(stacklessCoroTaskCacheFrame) ||
				task.frameSize == stacklessCoroFreeOverflowTask) {
			previous = task
			task = task.next
		}
	case allowOverflow:
		for task != nil && task.frameSize != stacklessCoroFreeOverflowTask {
			previous = task
			task = task.next
		}
	default:
		for task != nil && !task.hasFlag(stacklessCoroTaskCacheFrame) {
			previous = task
			task = task.next
		}
	}
	if task == nil {
		if s.freePlainTaskCount > 0 || allowOverflow {
			throw("runtime: invalid stackless coroutine overflow task list")
		}
		return nil
	}
	if previous == nil {
		s.freeTasks = task.next
	} else {
		previous.next = task.next
	}
	task.next = nil
	if task.hasFlag(stacklessCoroTaskCacheFrame) {
		if s.freeFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine frame cache size")
		}
		s.freeFrameBytes -= task.frameSize
		s.discardCachedFrameTaskLocked(task)
	} else if task.frameSize == stacklessCoroFreeOverflowTask {
		s.overflowTaskCounts -= 1 << stacklessCoroFreeOverflowTaskCountShift
	} else {
		if task.frameSize != 0 || s.freePlainTaskCount == 0 {
			throw("runtime: invalid stackless coroutine task cache count")
		}
		s.freePlainTaskCount--
	}
	return task
}

// discardFreeOverflowTasks drops never-used slots from the final partial
// chunk before a native context retains the bounded ordinary task cache.
// The caller must have exclusive ownership of s.
func (s *stacklessCoroScheduler) discardFreeOverflowTasks() {
	if s.overflowTaskCounts < 1<<stacklessCoroFreeOverflowTaskCountShift {
		return
	}
	link := &s.freeTasks
	for *link != nil {
		task := *link
		if !task.hasFlag(stacklessCoroTaskCacheFrame) &&
			task.frameSize == stacklessCoroFreeOverflowTask {
			*link = task.next
			task.next = nil
			s.overflowTaskCounts -= 1 << stacklessCoroFreeOverflowTaskCountShift
			if s.overflowTaskCounts < 1<<stacklessCoroFreeOverflowTaskCountShift {
				return
			}
			continue
		}
		link = &task.next
	}
	throw("runtime: invalid stackless coroutine overflow task cache")
}

func clearStacklessCoroTaskFrame(task *stacklessCoroTask) {
	uncachedLineage := isStacklessCoroUncachedFrameLineage(task.frameSize)
	task.resume = nil
	task.context.frame = nil
	task.flags = 0
	if !uncachedLineage {
		task.frameSize = 0
	}
}

func (s *stacklessCoroScheduler) discardCachedFrameTaskLocked(
	task *stacklessCoroTask) {
	if task.hasFlag(stacklessCoroTaskCacheFrame) {
		if s.cachedFrameTasks == 0 ||
			s.cachedFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine cached-frame ownership")
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
	uncachedLineage := !task.hasFlag(stacklessCoroTaskCacheFrame) &&
		isStacklessCoroUncachedFrameLineage(task.frameSize)
	validFrame := !task.hasFlag(stacklessCoroTaskCacheFrame) &&
		task.resume == nil &&
		task.context.frame == nil &&
		(task.frameSize == 0 || uncachedLineage)
	if task.hasFlag(stacklessCoroTaskCacheFrame) {
		validFrame = task.resume != nil && task.context.frame != nil &&
			int(task.frameSize) <= stacklessCoroFrameCacheSize &&
			s.cachedFrameTasks > 0 &&
			s.cachedFrameBytes >= task.frameSize
	}
	if task == &s.root || !validFrame || task.parent != nil ||
		task.next != nil || task.state != stacklessCoroTaskComplete ||
		task.terminal != stacklessCoroTerminalNone || task.goexit ||
		task.hasFlag(stacklessCoroTaskFusedFrames) ||
		task.hasFlag(stacklessCoroTaskFusedRequested) ||
		task.hasFlag(stacklessCoroTaskFusedPending) ||
		task.hasFlag(stacklessCoroTaskSwitchPending) ||
		task.resuming || task.readyPending ||
		task.context.scheduler != nil ||
		hasTerminalValue {
		throw("runtime: invalid stackless coroutine task recycle")
	}
	if uncachedLineage {
		task.frameSize = 0
		return
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
	if !task.hasFlag(stacklessCoroTaskCacheFrame) &&
		s.freePlainTaskCount ==
			uint16(stacklessCoroTaskCacheSize)-s.cachedFrameTasks {
		return
	}
	task.state = stacklessCoroTaskNew
	task.next = s.freeTasks
	s.freeTasks = task
	if task.hasFlag(stacklessCoroTaskCacheFrame) {
		if s.freeFrameBytes > s.cachedFrameBytes ||
			s.cachedFrameBytes-s.freeFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine free-frame size")
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

// runInitial drives a scheduler that has not yet been published to a producer.
func (s *stacklessCoroScheduler) runInitial() {
	scope := enterStacklessCoroRunScope()
	defer scope.leave()
	s.runTasks(false, true, true)
	s.runLoop(false)
}

func (s *stacklessCoroScheduler) runLoop(native bool) {
	for !s.rootComplete() {
		s.runTasks(native, true, false)
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

// runTasks drives the ready queue until the root completes, one resume panics,
// or a non-parking episode exhausts ready work. A recovered panic returns to
// run, which starts another queue-driving episode after this native activation
// has unwound. initial is valid only while the scheduler remains private.
func (s *stacklessCoroScheduler) runTasks(native, park, initial bool) {
	var task *stacklessCoroTask
	var idlePollSkip *stacklessCoroTask
	var directInitialRoot bool
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

	// Select the private root before entering the ordinary queue loop. Keeping
	// this branch out of that loop avoids adding work to every later yield.
	// takeInitialRoot validates the root before its first resume. A native
	// driver can retain that ownership while the root remains private; every
	// non-yield action is validated by its handler below.
	if initial {
		task = s.takeInitialRoot()
		directInitialRoot = native && !raceenabled &&
			!stacklessCoroIsPullComparison(s)
		goto resume
	}

next:
	if s.rootComplete() {
		return
	}
	task = s.take()
	if task == nil {
		if native {
			task = stacklessCoroPollReadBeforePark(s, idlePollSkip)
		}
		if task == nil {
			if !park {
				return
			}
			if !s.waitForWork() {
				return
			}
			goto next
		}
	}
resume:
	task.context.scheduler = s
	// Retain the active context across private yields. Detaching it on every
	// iteration would repeat two pointer write barriers while no other executor
	// can observe or resume this task.
resumeActive:
	action := task.resume(unsafe.Pointer(&task.context))
	if directInitialRoot && action == stacklessCoroActionYield &&
		s.initialRootPrivate {
		goto resumeActive
	}
	task.context.scheduler = nil

	switch action {
	case stacklessCoroActionYield:
		idlePollSkip = nil
		directInitialRoot = false
		resumeDirectly, foreignReturner := s.yield(task, native)
		if resumeDirectly {
			goto resume
		}
		if !native || foreignReturner {
			// Cooperate with the host scheduler when this target has no
			// native executor implementation or a foreign-call executor
			// needs a P to return from syscall state.
			Gosched()
		}
	case stacklessCoroActionWait:
		if s.waiting(task, native) {
			idlePollSkip = nil
			initial = false
			directInitialRoot = false
			goto resume
		}
		idlePollSkip = task
	case stacklessCoroActionComplete:
		idlePollSkip = nil
		if initial && s.completeInitialRoot(task) {
			return
		}
		task = s.complete(task)
		if task != nil && !s.rootComplete() {
			goto resume
		}
	case stacklessCoroActionPanic:
		idlePollSkip = nil
		s.terminate(task)
	case stacklessCoroActionGoexit:
		idlePollSkip = nil
		s.goexit(task)
	case stacklessCoroActionSwitch:
		idlePollSkip = nil
		if !task.hasFlag(stacklessCoroTaskSwitchPending) ||
			task.state != stacklessCoroTaskRunning || !task.resuming ||
			task.readyPending {
			throw("runtime: invalid stackless coroutine frame switch")
		}
		task.setFlag(stacklessCoroTaskSwitchPending, false)
		goto resume
	default:
		throw("runtime: invalid stackless coroutine action")
	}
	initial = false
	directInitialRoot = false
	goto next
}

// waitForWork parks the current ordinary G until work is ready. It reports
// false when root completion stopped the replacement executors instead.
func (s *stacklessCoroScheduler) waitForWork() bool {
	if s.executorStop == nil {
		<-s.wake
		return true
	}
	select {
	case <-s.wake:
		return true
	case <-s.executorStop:
		return false
	}
}

func (s *stacklessCoroScheduler) rootComplete() bool {
	return s.lifetime.Load()&stacklessCoroLifetimeRootComplete != 0
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

func isStacklessCoroUncachedFrameLineage(marker uint16) bool {
	return marker == stacklessCoroUncachedFrameLineage ||
		marker <= stacklessCoroFrameChunkDirectFirst &&
			marker >= stacklessCoroFrameChunkLast
}

func isStacklessCoroFrameChunkElement(marker uint16) bool {
	return marker <= stacklessCoroFrameChunkFirst &&
		marker >= stacklessCoroFrameChunkLast
}

// coroFrameNeedsClear reports whether another runtime-owned reference can
// retain the current typed allocation after this frame completes. Generated
// completion paths clear pointer fields before returning such a frame.
func coroFrameNeedsClear(ctx unsafe.Pointer) bool {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil ||
		task.state != stacklessCoroTaskRunning || !task.resuming {
		throw("runtime: invalid stackless coroutine frame-clear query")
	}
	return task.hasFlag(stacklessCoroTaskFusedFrames) ||
		task.hasFlag(stacklessCoroTaskCacheFrame) ||
		isStacklessCoroFrameChunkElement(task.frameSize)
}

// coroTakeFrame reserves a task for a compiler-generated frame factory and
// returns a cached frame when one has the same resume entry and size. The
// reservation keeps the opaque typed allocation reachable until the matching
// await or spawn operation consumes it.
func coroTakeFrame(ctx unsafe.Pointer, child stacklessCoroResume,
	size uintptr) unsafe.Pointer {
	if ctx == nil || raceenabled || size > stacklessCoroFrameCacheSize {
		return nil
	}
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context.scheduler == nil || parent == nil || child == nil {
		throw("runtime: invalid stackless coroutine frame reservation")
	}
	s := context.scheduler
	if isStacklessCoroUncachedFrameLineage(parent.frameSize) {
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
		task = s.newFrameReservationTaskLocked(child, parent, size)
		if s.cachedFrameTasks < stacklessCoroTaskCacheSize &&
			uintptr(s.cachedFrameBytes)+size <= stacklessCoroFrameCacheSize {
			task.setFlag(stacklessCoroTaskCacheFrame, true)
			task.frameSize = uint16(size)
			s.cachedFrameTasks++
			s.cachedFrameBytes += uint16(size)
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

// coroTakeFrameChunk extends coroTakeFrame for compiler-proven structured
// self recursion. chunkType describes the generated [4]frame allocation, so
// newobject preserves its exact GC pointer map.
func coroTakeFrameChunk(ctx unsafe.Pointer, child stacklessCoroResume,
	size uintptr, chunkType *_type) unsafe.Pointer {
	if ctx == nil || raceenabled || size > stacklessCoroFrameCacheSize {
		return nil
	}
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context.scheduler == nil || parent == nil || child == nil {
		throw("runtime: invalid stackless coroutine frame reservation")
	}
	s := context.scheduler
	// A marked explicit-frame task carries a saturated lineage without a
	// shared-state query.
	if isStacklessCoroUncachedFrameLineage(parent.frameSize) {
		chunkEligible := size != 0 &&
			size <= stacklessCoroFrameCacheSize/stacklessCoroFrameChunkSize &&
			validStacklessCoroFrameChunkType(chunkType, size) &&
			parent.resume != nil &&
			stacklessCoroResumeIdentity(parent.resume) ==
				stacklessCoroResumeIdentity(child)
		if !chunkEligible {
			return nil
		}
		switch marker := parent.frameSize; {
		case marker == stacklessCoroFrameChunkDirectLast ||
			marker == stacklessCoroFrameChunkLast:
			return newobject(chunkType)
		case marker <= stacklessCoroFrameChunkFirst &&
			marker > stacklessCoroFrameChunkLast:
			if parent.context.frame == nil {
				throw("runtime: missing stackless coroutine frame chunk")
			}
			return add(parent.context.frame, size)
		default:
			return nil
		}
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
		task = s.newFrameReservationTaskLocked(child, parent, size)
		if s.cachedFrameTasks < stacklessCoroTaskCacheSize &&
			uintptr(s.cachedFrameBytes)+size <= stacklessCoroFrameCacheSize {
			task.setFlag(stacklessCoroTaskCacheFrame, true)
			task.frameSize = uint16(size)
			s.cachedFrameTasks++
			s.cachedFrameBytes += uint16(size)
		} else if s.cachedFrameTasks == stacklessCoroTaskCacheSize &&
			s.freeFrameBytes == 0 {
			task.frameSize = stacklessCoroUncachedFrameLineage
			if size != 0 &&
				size <= stacklessCoroFrameCacheSize/stacklessCoroFrameChunkSize &&
				validStacklessCoroFrameChunkType(chunkType, size) {
				task.frameSize = stacklessCoroFrameChunkDirectFirst
			}
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

func validStacklessCoroFrameChunkType(chunkType *_type, size uintptr) bool {
	if chunkType == nil || chunkType.Kind() != abi.Array {
		return false
	}
	array := (*arraytype)(unsafe.Pointer(chunkType))
	return array.Len == stacklessCoroFrameChunkSize &&
		array.Elem != nil && array.Elem.Size_ == size &&
		chunkType.Size_ == size*stacklessCoroFrameChunkSize
}

func validStacklessCoroFusedFrameMarker(marker uint8) bool {
	allowed := stacklessCoroFusedFrameAllocationMask |
		stacklessCoroFusedFrameParent | stacklessCoroFusedFrameResume
	return marker&^allowed == 0 &&
		marker&stacklessCoroFusedFrameAllocationMask <=
			stacklessCoroFusedFrameChunkLast
}

func validStacklessCoroSelfFrameMarker(marker uint8) bool {
	return marker <= stacklessCoroFusedFrameChunkLast
}

// coroTakeSelfFrame keeps the homogeneous recursive fast path separate from
// heterogeneous fusion. Matching resumes need neither a request transition
// nor an extended frame header.
func coroTakeSelfFrame(ctx unsafe.Pointer, child stacklessCoroResume,
	size uintptr, chunkType *_type) unsafe.Pointer {
	if ctx == nil || raceenabled || size < unsafe.Sizeof(stacklessCoroFusedFrameHeader{}) ||
		size > stacklessCoroFrameCacheSize/stacklessCoroFrameChunkSize ||
		!validStacklessCoroFrameChunkType(chunkType, size) {
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context.scheduler == nil || parent == nil || child == nil {
		throw("runtime: invalid stackless coroutine self-frame reservation")
	}
	if parent.resume == nil ||
		stacklessCoroResumeIdentity(parent.resume) !=
			stacklessCoroResumeIdentity(child) {
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	if context.frame == nil {
		throw("runtime: missing stackless coroutine self frame")
	}

	s := context.scheduler
	lock(&s.lock)
	if stacklessCoroIsPullComparison(s) {
		unlock(&s.lock)
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.hasFlag(stacklessCoroTaskFusedPending) ||
		parent.hasFlag(stacklessCoroTaskSwitchPending) ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine self-frame reservation outside resume")
	}
	header := (*stacklessCoroFusedFrameHeader)(context.frame)
	if !validStacklessCoroSelfFrameMarker(header.marker) {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine self-frame marker")
	}

	task := s.takeCachedFrameTaskLocked(child, uint16(size))
	if task == nil && s.cachedFrameTasks < stacklessCoroTaskCacheSize &&
		uintptr(s.cachedFrameBytes)+size <= stacklessCoroFrameCacheSize {
		task = s.newFrameReservationTaskLocked(child, parent, size)
		task.setFlag(stacklessCoroTaskCacheFrame, true)
		task.frameSize = uint16(size)
		s.cachedFrameTasks++
		s.cachedFrameBytes += uint16(size)
	} else if task != nil {
		task.parent = parent
	}
	parent.setFlag(stacklessCoroTaskFusedPending, true)
	if task != nil {
		task.next = s.reservedTasks
		s.reservedTasks = task
		frame := task.context.frame
		unlock(&s.lock)
		return frame
	}
	marker := header.marker
	frame := context.frame
	unlock(&s.lock)

	switch {
	case marker == 0 || marker < stacklessCoroFusedFrameDirectLast:
		return nil
	case marker == stacklessCoroFusedFrameDirectLast ||
		marker == stacklessCoroFusedFrameChunkLast:
		return newobject(chunkType)
	case marker >= stacklessCoroFusedFrameChunkFirst &&
		marker < stacklessCoroFusedFrameChunkLast:
		return add(frame, size)
	default:
		throw("runtime: invalid stackless coroutine self-frame allocation")
		return nil
	}
}

// coroAwaitSelfFrame switches a homogeneous structured self-await to its next
// frame while preserving the original fast-path scheduling behavior.
func coroAwaitSelfFrame(ctx, frame unsafe.Pointer,
	child stacklessCoroResume) uint8 {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context == nil || context.scheduler == nil || context.frame == nil ||
		parent == nil || frame == nil || child == nil {
		throw("runtime: invalid stackless coroutine self-frame await")
	}
	s := context.scheduler
	lock(&s.lock)
	if raceenabled || stacklessCoroIsPullComparison(s) ||
		!parent.hasFlag(stacklessCoroTaskFusedPending) || parent.resume == nil ||
		stacklessCoroResumeIdentity(parent.resume) !=
			stacklessCoroResumeIdentity(child) {
		unlock(&s.lock)
		coroAwaitFrame(ctx, frame, child)
		return stacklessCoroActionWait
	}
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.hasFlag(stacklessCoroTaskSwitchPending) ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine self-frame await outside resume")
	}
	parent.setFlag(stacklessCoroTaskFusedPending, false)
	childHeader := (*stacklessCoroFusedFrameHeader)(frame)
	parentHeader := (*stacklessCoroFusedFrameHeader)(context.frame)
	if childHeader.parent != nil || childHeader.owner != nil ||
		childHeader.marker != 0 {
		unlock(&s.lock)
		throw("runtime: invalid initialized stackless coroutine self frame")
	}
	owner := s.takeReservedFrameTaskLocked(parent, frame, child)
	if owner != nil {
		owner.parent = nil
		childHeader.owner = owner
	} else {
		switch marker := parentHeader.marker; {
		case marker == 0:
			childHeader.marker = stacklessCoroFusedFrameDirectFirst
		case marker >= stacklessCoroFusedFrameDirectFirst &&
			marker < stacklessCoroFusedFrameDirectLast:
			childHeader.marker = marker + 1
		case marker == stacklessCoroFusedFrameDirectLast ||
			marker == stacklessCoroFusedFrameChunkLast:
			childHeader.marker = stacklessCoroFusedFrameChunkFirst
		case marker >= stacklessCoroFusedFrameChunkFirst &&
			marker < stacklessCoroFusedFrameChunkLast:
			childHeader.marker = marker + 1
		default:
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine self-frame transition")
		}
	}
	childHeader.parent = context.frame
	context.frame = frame
	parent.setFlag(stacklessCoroTaskFusedFrames, true)
	if s.head == nil {
		parent.setFlag(stacklessCoroTaskSwitchPending, true)
		unlock(&s.lock)
		return stacklessCoroActionSwitch
	}
	unlock(&s.lock)
	return stacklessCoroActionYield
}

// coroCompleteSelfFrame restores a suspended homogeneous parent frame.
func coroCompleteSelfFrame(ctx unsafe.Pointer) uint8 {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || context.frame == nil ||
		task == nil {
		throw("runtime: invalid stackless coroutine self-frame completion")
	}
	s := context.scheduler
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		!task.hasFlag(stacklessCoroTaskFusedFrames) ||
		task.hasFlag(stacklessCoroTaskFusedPending) ||
		task.hasFlag(stacklessCoroTaskSwitchPending) ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine self-frame completion outside resume")
	}
	frame := context.frame
	header := (*stacklessCoroFusedFrameHeader)(frame)
	if header.parent == nil || !validStacklessCoroSelfFrameMarker(header.marker) {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine completed self frame")
	}
	owner := header.owner
	if header.marker == 0 {
		if !validStacklessCoroFusedFrameOwner(owner, frame) {
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine self-frame owner")
		}
	} else if owner != nil {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine self-frame ownership")
	}
	parentFrame := header.parent
	header.parent = nil
	header.owner = nil
	header.marker = 0
	context.frame = parentFrame
	parentHeader := (*stacklessCoroFusedFrameHeader)(parentFrame)
	task.setFlag(stacklessCoroTaskFusedFrames, parentHeader.parent != nil)
	if owner != nil {
		owner.state = stacklessCoroTaskComplete
		s.recycleTaskLocked(owner)
	}
	if s.head == nil {
		task.setFlag(stacklessCoroTaskSwitchPending, true)
		unlock(&s.lock)
		return stacklessCoroActionSwitch
	}
	unlock(&s.lock)
	return stacklessCoroActionYield
}

// coroRequestFusedFrame marks the next local ABI 3 factory call as a
// compiler-proven structured await. The request is consumed by
// coroTakeFusedFrame. Race and pull-comparison builds retain distinct tasks.
func coroRequestFusedFrame(ctx unsafe.Pointer) {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context == nil || context.scheduler == nil || parent == nil {
		throw("runtime: invalid stackless coroutine fused-frame request")
	}
	if raceenabled {
		return
	}
	s := context.scheduler
	lock(&s.lock)
	if stacklessCoroIsPullComparison(s) {
		unlock(&s.lock)
		return
	}
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.hasFlag(stacklessCoroTaskFusedRequested) ||
		parent.hasFlag(stacklessCoroTaskFusedPending) ||
		parent.hasFlag(stacklessCoroTaskSwitchPending) ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine fused-frame request outside resume")
	}
	parent.setFlag(stacklessCoroTaskFusedRequested, true)
	unlock(&s.lock)
}

// coroTakeFusedFrame extends typed-frame reuse for a compiler-proven
// structured await. A fused child shares the active logical task, so only a
// cache-owned frame holder is reserved. Homogeneous recursion additionally
// uses the direct prefix and typed four-frame chunks after the cache fills.
func coroTakeFusedFrame(ctx unsafe.Pointer, child stacklessCoroResume,
	size uintptr, chunkType *_type, self bool) unsafe.Pointer {
	if ctx == nil || raceenabled {
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context.scheduler == nil || parent == nil || child == nil {
		throw("runtime: invalid stackless coroutine fused-frame reservation")
	}

	s := context.scheduler
	sameResume := parent.resume != nil &&
		stacklessCoroResumeIdentity(parent.resume) ==
			stacklessCoroResumeIdentity(child)
	lock(&s.lock)
	if stacklessCoroIsPullComparison(s) {
		unlock(&s.lock)
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.hasFlag(stacklessCoroTaskFusedPending) ||
		parent.hasFlag(stacklessCoroTaskSwitchPending) ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine fused-frame reservation outside resume")
	}
	requested := parent.hasFlag(stacklessCoroTaskFusedRequested)
	self = self && sameResume
	if !self && !requested {
		unlock(&s.lock)
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	if requested {
		parent.setFlag(stacklessCoroTaskFusedRequested, false)
	}
	if context.frame == nil {
		unlock(&s.lock)
		throw("runtime: missing stackless coroutine fused frame")
	}
	if size < unsafe.Sizeof(stacklessCoroFusedFrameHeader{}) ||
		size > stacklessCoroFrameCacheSize {
		unlock(&s.lock)
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	if sameResume &&
		(size > stacklessCoroFrameCacheSize/stacklessCoroFrameChunkSize ||
			!validStacklessCoroFrameChunkType(chunkType, size)) {
		unlock(&s.lock)
		return coroTakeFrameChunk(ctx, child, size, chunkType)
	}
	var marker uint8
	if sameResume {
		header := (*stacklessCoroFusedFrameHeader)(context.frame)
		if !validStacklessCoroFusedFrameMarker(header.marker) {
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine fused-frame marker")
		}
		marker = header.marker & stacklessCoroFusedFrameAllocationMask
	}

	task := s.takeCachedFrameTaskLocked(child, uint16(size))
	if task == nil && !sameResume {
		for (s.cachedFrameTasks >= stacklessCoroTaskCacheSize ||
			uintptr(s.cachedFrameBytes)+size > stacklessCoroFrameCacheSize) &&
			s.discardFreeCachedFrameTaskLocked() {
		}
	}
	if task == nil && s.cachedFrameTasks < stacklessCoroTaskCacheSize &&
		uintptr(s.cachedFrameBytes)+size <= stacklessCoroFrameCacheSize {
		task = s.newFrameReservationTaskLocked(child, parent, size)
		task.setFlag(stacklessCoroTaskCacheFrame, true)
		task.frameSize = uint16(size)
		s.cachedFrameTasks++
		s.cachedFrameBytes += uint16(size)
	} else if task != nil {
		task.parent = parent
	}
	parent.setFlag(stacklessCoroTaskFusedPending, true)
	if task != nil {
		task.next = s.reservedTasks
		s.reservedTasks = task
		frame := task.context.frame
		unlock(&s.lock)
		return frame
	}
	frame := context.frame
	unlock(&s.lock)
	if !sameResume {
		return nil
	}

	switch {
	case marker == 0 || marker < stacklessCoroFusedFrameDirectLast:
		return nil
	case marker == stacklessCoroFusedFrameDirectLast ||
		marker == stacklessCoroFusedFrameChunkLast:
		return newobject(chunkType)
	case marker >= stacklessCoroFusedFrameChunkFirst &&
		marker < stacklessCoroFusedFrameChunkLast:
		return add(frame, size)
	default:
		throw("runtime: invalid stackless coroutine fused-frame allocation")
		return nil
	}
}

// coroAwaitFusedFrame switches a compiler-proven structured await to child
// without creating a logical task. An already-runnable sibling keeps queue
// priority; otherwise the current executor can enter child immediately.
func coroAwaitFusedFrame(ctx, frame unsafe.Pointer,
	child stacklessCoroResume) uint8 {
	context := (*stacklessCoroContext)(ctx)
	parent := context.task()
	if context == nil || context.scheduler == nil || context.frame == nil ||
		parent == nil || frame == nil || child == nil {
		throw("runtime: invalid stackless coroutine fused-frame await")
	}
	s := context.scheduler
	lock(&s.lock)
	if parent.hasFlag(stacklessCoroTaskFusedRequested) {
		unlock(&s.lock)
		throw("runtime: unconsumed stackless coroutine fused-frame request")
	}
	if raceenabled || stacklessCoroIsPullComparison(s) ||
		!parent.hasFlag(stacklessCoroTaskFusedPending) {
		unlock(&s.lock)
		coroAwaitFrame(ctx, frame, child)
		return stacklessCoroActionWait
	}
	if parent.state != stacklessCoroTaskRunning || !parent.resuming ||
		parent.readyPending ||
		parent.hasFlag(stacklessCoroTaskSwitchPending) ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit ||
		parent.resume == nil {
		unlock(&s.lock)
		throw("runtime: stackless coroutine fused-frame await outside resume")
	}
	parent.setFlag(stacklessCoroTaskFusedPending, false)
	childHeader := (*stacklessCoroFusedFrameHeader)(frame)
	parentHeader := (*stacklessCoroFusedFrameHeader)(context.frame)
	if childHeader.parent != nil || childHeader.owner != nil ||
		childHeader.marker != 0 {
		unlock(&s.lock)
		throw("runtime: invalid initialized stackless coroutine fused frame")
	}
	owner := s.takeReservedFrameTaskLocked(parent, frame, child)
	sameResume := stacklessCoroResumeIdentity(parent.resume) ==
		stacklessCoroResumeIdentity(child)
	marker := uint8(0)
	if owner != nil {
		owner.parent = nil
		childHeader.owner = owner
	} else if sameResume {
		parentMarker := parentHeader.marker &
			stacklessCoroFusedFrameAllocationMask
		switch {
		case parentMarker == 0:
			marker = stacklessCoroFusedFrameDirectFirst
		case parentMarker >= stacklessCoroFusedFrameDirectFirst &&
			parentMarker < stacklessCoroFusedFrameDirectLast:
			marker = parentMarker + 1
		case parentMarker == stacklessCoroFusedFrameDirectLast ||
			parentMarker == stacklessCoroFusedFrameChunkLast:
			marker = stacklessCoroFusedFrameChunkFirst
		case parentMarker >= stacklessCoroFusedFrameChunkFirst &&
			parentMarker < stacklessCoroFusedFrameChunkLast:
			marker = parentMarker + 1
		default:
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine fused-frame transition")
		}
	} else {
		marker = stacklessCoroFusedFrameDirectFirst
	}
	if parent.hasFlag(stacklessCoroTaskFusedFrames) {
		marker |= stacklessCoroFusedFrameParent
	}
	if !sameResume {
		header := (*stacklessCoroFusedResumeFrameHeader)(frame)
		if header.resume != nil {
			unlock(&s.lock)
			throw("runtime: initialized stackless coroutine fused resume")
		}
		header.resume = stacklessCoroResumeIdentity(parent.resume)
		marker |= stacklessCoroFusedFrameResume
	}
	childHeader.marker = marker
	childHeader.parent = context.frame
	context.frame = frame
	parent.resume = child
	parent.setFlag(stacklessCoroTaskFusedFrames, true)
	if s.head == nil {
		parent.setFlag(stacklessCoroTaskSwitchPending, true)
		unlock(&s.lock)
		return stacklessCoroActionSwitch
	}
	unlock(&s.lock)
	return stacklessCoroActionYield
}

func validStacklessCoroFusedFrameOwner(owner *stacklessCoroTask,
	frame unsafe.Pointer) bool {
	return owner != nil && owner.flags == stacklessCoroTaskCacheFrame &&
		owner.context.frame == frame
}

// coroCompleteFusedFrame restores the suspended frame and resume in a fused
// structured-await chain. The generated resume has already copied results and
// cleared pointer fields that another live frame allocation can retain.
func coroCompleteFusedFrame(ctx unsafe.Pointer) uint8 {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || context.frame == nil ||
		task == nil {
		throw("runtime: invalid stackless coroutine fused-frame completion")
	}
	s := context.scheduler
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		!task.hasFlag(stacklessCoroTaskFusedFrames) ||
		task.hasFlag(stacklessCoroTaskFusedRequested) ||
		task.hasFlag(stacklessCoroTaskFusedPending) ||
		task.hasFlag(stacklessCoroTaskSwitchPending) ||
		task.terminal != stacklessCoroTerminalNone ||
		task.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine fused-frame completion outside resume")
	}
	frame := context.frame
	header := (*stacklessCoroFusedFrameHeader)(frame)
	if header.parent == nil || !validStacklessCoroFusedFrameMarker(header.marker) {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine completed fused frame")
	}
	allocation := header.marker & stacklessCoroFusedFrameAllocationMask
	owner := header.owner
	if allocation == 0 {
		if !validStacklessCoroFusedFrameOwner(owner, frame) {
			unlock(&s.lock)
			throw("runtime: invalid stackless coroutine fused-frame owner")
		}
	} else if owner != nil {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine fused-frame ownership")
	}
	parentFrame := header.parent
	parentFused := header.marker&stacklessCoroFusedFrameParent != 0
	if header.marker&stacklessCoroFusedFrameResume != 0 {
		resumeHeader := (*stacklessCoroFusedResumeFrameHeader)(frame)
		if resumeHeader.resume == nil {
			unlock(&s.lock)
			throw("runtime: missing stackless coroutine fused resume")
		}
		task.resume = *(*stacklessCoroResume)(unsafe.Pointer(
			&resumeHeader.resume))
		resumeHeader.resume = nil
	}
	header.parent = nil
	header.owner = nil
	header.marker = 0
	context.frame = parentFrame
	task.setFlag(stacklessCoroTaskFusedFrames, parentFused)
	if owner != nil {
		owner.state = stacklessCoroTaskComplete
		s.recycleTaskLocked(owner)
	}
	if s.head == nil {
		task.setFlag(stacklessCoroTaskSwitchPending, true)
		unlock(&s.lock)
		return stacklessCoroActionSwitch
	}
	unlock(&s.lock)
	return stacklessCoroActionYield
}

// discardFusedFramesLocked drops every fused child after a terminal outcome.
// Compiler eligibility excludes cleanup in these frames, so restoring the
// chain root preserves the terminal behavior while cached holders must be
// discarded rather than retaining uncleared pointers.
func (s *stacklessCoroScheduler) discardFusedFramesLocked(
	task *stacklessCoroTask) {
	for task.hasFlag(stacklessCoroTaskFusedFrames) {
		frame := task.context.frame
		if frame == nil {
			throw("runtime: missing stackless coroutine fused frame")
		}
		header := (*stacklessCoroFusedFrameHeader)(frame)
		if header.parent == nil ||
			!validStacklessCoroFusedFrameMarker(header.marker) {
			throw("runtime: invalid stackless coroutine fused-frame unwind")
		}
		allocation := header.marker & stacklessCoroFusedFrameAllocationMask
		owner := header.owner
		if allocation == 0 {
			if !validStacklessCoroFusedFrameOwner(owner, frame) {
				throw("runtime: invalid stackless coroutine fused-frame unwind owner")
			}
		} else if owner != nil {
			throw("runtime: invalid stackless coroutine fused-frame unwind owner")
		}
		parentFrame := header.parent
		parentFused := header.marker&stacklessCoroFusedFrameParent != 0
		if header.marker&stacklessCoroFusedFrameResume != 0 {
			resumeHeader := (*stacklessCoroFusedResumeFrameHeader)(frame)
			if resumeHeader.resume == nil {
				throw("runtime: missing stackless coroutine fused-frame unwind resume")
			}
			task.resume = *(*stacklessCoroResume)(unsafe.Pointer(
				&resumeHeader.resume))
			resumeHeader.resume = nil
		}
		header.parent = nil
		header.owner = nil
		header.marker = 0
		task.context.frame = parentFrame
		if owner != nil {
			owner.state = stacklessCoroTaskComplete
			s.discardCachedFrameTaskLocked(owner)
			s.recycleTaskLocked(owner)
		}
		task.setFlag(stacklessCoroTaskFusedFrames, parentFused)
	}
}

// markUncachedFrameLineageLocked records a saturated explicit-frame lineage
// after a factory bypasses its reservation. The scheduler lock must be held.
func (s *stacklessCoroScheduler) markUncachedFrameLineageLocked(task,
	parent *stacklessCoroTask, frame unsafe.Pointer) {
	if frame == nil || task.hasFlag(stacklessCoroTaskCacheFrame) ||
		isStacklessCoroUncachedFrameLineage(task.frameSize) {
		return
	}
	marker := parent.frameSize
	if !isStacklessCoroUncachedFrameLineage(marker) {
		return
	}
	task.frameSize = stacklessCoroUncachedFrameLineage
	if parent.resume == nil || task.resume == nil ||
		stacklessCoroResumeIdentity(parent.resume) !=
			stacklessCoroResumeIdentity(task.resume) {
		return
	}
	switch {
	case marker <= stacklessCoroFrameChunkDirectFirst &&
		marker >= stacklessCoroFrameChunkDirectLast:
		if marker == stacklessCoroFrameChunkDirectLast {
			task.frameSize = stacklessCoroFrameChunkFirst
		} else {
			task.frameSize = marker - 1
		}
	case marker <= stacklessCoroFrameChunkFirst &&
		marker >= stacklessCoroFrameChunkLast:
		if marker == stacklessCoroFrameChunkLast {
			task.frameSize = stacklessCoroFrameChunkFirst
		} else {
			task.frameSize = marker - 1
		}
	}
}

func (s *stacklessCoroScheduler) takeCachedFrameTaskLocked(
	resume stacklessCoroResume, size uint16) *stacklessCoroTask {
	identity := stacklessCoroResumeIdentity(resume)
	var previous *stacklessCoroTask
	for task := s.freeTasks; task != nil; task = task.next {
		if !task.hasFlag(stacklessCoroTaskCacheFrame) ||
			task.frameSize != size ||
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
		task.next = nil
		return task
	}
	return nil
}

// discardFreeCachedFrameTaskLocked makes room for a new typed frame identity.
// Native contexts retain a bounded cache across roots; without replacement, a
// deep call of one type could permanently force later heterogeneous frames to
// allocate. Active frames stay untouched.
func (s *stacklessCoroScheduler) discardFreeCachedFrameTaskLocked() bool {
	var previous *stacklessCoroTask
	for task := s.freeTasks; task != nil; task = task.next {
		if !task.hasFlag(stacklessCoroTaskCacheFrame) {
			previous = task
			continue
		}
		if task.state != stacklessCoroTaskNew || task.parent != nil ||
			task.context.scheduler != nil || task.context.frame == nil ||
			task.frameSize == 0 || s.freeFrameBytes < task.frameSize {
			throw("runtime: invalid stackless coroutine free cached frame")
		}
		if previous == nil {
			s.freeTasks = task.next
		} else {
			previous.next = task.next
		}
		task.next = nil
		s.freeFrameBytes -= task.frameSize
		s.discardCachedFrameTaskLocked(task)
		task.next = s.freeTasks
		s.freeTasks = task
		s.freePlainTaskCount++
		return true
	}
	return false
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

// coroAwait transfers execution to child. Completion schedules the current
// task again; it does not call the parent continuation from the child resume.
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
	s.runInitial()
	s.releaseWake()
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
	startStacklessCoroSleep(ctx, ns)
	return true
}

func startStacklessCoroSleep(ctx unsafe.Pointer, ns int64) {
	op := stacklessCoroStartOperation(ctx, "sleep")
	s := op.scheduler
	when := stacklessCoroTimerWhen(ns)
	op.id = stacklessCoroDeferredSleepOperationID
	op.packet[0] = uint64(when)
	if stacklessCoroIsPullComparison(s) || !stacklessCoroDeferSleep(s, op) {
		armStacklessCoroTimerAt(op, when)
	}
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
	if op != nil && op.selection.active() {
		finishStacklessCoroSelect(op, waiter, success)
		return
	}
	if op == nil || takeStacklessCoroOperation(op.id) != op {
		throw("runtime: invalid stackless coroutine channel completion")
	}
	if waiter != nil {
		waiter.coro.clear()
		waiter.c.set(nil)
		releaseStacklessCoroSudog(unsafe.Pointer(op), waiter)
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

func startStacklessCoroTimer(ctx unsafe.Pointer, ns int64) stacklessCoroTimerToken {
	op := stacklessCoroStartOperation(ctx, "sleep")
	return armStacklessCoroTimer(op, ns)
}

func armStacklessCoroTimer(op *stacklessCoroOperation, ns int64) stacklessCoroTimerToken {
	t, sequence := op.nextTimer()
	return scheduleStacklessCoroTimer(op, t, sequence, stacklessCoroTimerWhen(ns))
}

func stacklessCoroTimerWhen(ns int64) int64 {
	when := nanotime()
	if ns > 0 {
		when += ns
		if when < 0 {
			when = maxWhen
		}
	}
	return when
}

func armStacklessCoroTimerAt(op *stacklessCoroOperation, when int64) stacklessCoroTimerToken {
	t, sequence := op.nextTimer()
	return scheduleStacklessCoroTimer(op, t, sequence, when)
}

func scheduleStacklessCoroTimer(op *stacklessCoroOperation,
	t *stacklessCoroTimer, sequence uintptr, when int64) stacklessCoroTimerToken {
	op.id = uint64(sequence)
	t.active.Store(sequence)
	t.timer.modify(when, 0, stacklessCoroTimerReady, t, sequence)
	return stacklessCoroTimerToken{timer: t, sequence: sequence}
}

func (op *stacklessCoroOperation) nextTimer() (*stacklessCoroTimer, uintptr) {
	t := op.timer
	if t != nil && (t.operation != op || t.active.Load() != 0) {
		throw("runtime: invalid stackless coroutine timer reuse")
	}
	if t == nil || t.sequence == ^uintptr(0) {
		t = &stacklessCoroTimer{operation: op}
		t.timer.init(stacklessCoroTimerReady, t)
		op.timer = t
	}
	t.sequence++
	return t, t.sequence
}

func cancelStacklessCoroTimer(token stacklessCoroTimerToken) bool {
	t := token.timer
	if t == nil || token.sequence == 0 ||
		!t.active.CompareAndSwap(token.sequence, 0) {
		return false
	}
	op := t.operation
	if op == nil || op.timer != t || op.id != uint64(token.sequence) {
		throw("runtime: canceled stackless coroutine operation is not a timer")
	}
	t.timer.stop()
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

// coroEnterSyscall reserves replacement capacity, saves its caller as the
// syscall continuation, and enters a blocking syscall without marking it as a
// foreign call. Its caller remains active until coroExitSyscall.
//
//go:nosplit
func coroEnterSyscall(ctx unsafe.Pointer) {
	context := (*stacklessCoroContext)(ctx)
	task := context.task()
	if context == nil || context.scheduler == nil || task == nil {
		throw("runtime: invalid stackless coroutine syscall")
	}
	scheduler := context.scheduler
	tracked := scheduler.executorState.Load() == stacklessCoroExecutorStateRunning
	handoff := tracked && scheduler.enterBlockingExecutor()

	gp := getg()
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

	pc := sys.GetCallerPC()
	sp := sys.GetCallerSP()
	bp := getcallerfp()
	if handoff {
		reentersyscallblock(pc, sp, bp)
	} else {
		reentersyscall(pc, sp, bp)
	}
}

//go:nosplit
func coroExitSyscall() {
	exitsyscall()
	gp := getg()
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
	if s.initialRootPrivate {
		s.initialRootPrivate = false
	}
	s.lifetime.Or(stacklessCoroLifetimeOperationStarted)
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
		op = takeStacklessCoroSharedOperation()
		if op == nil {
			op = new(stacklessCoroOperation)
		}
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
		(op.timer != nil && op.timer.active.Load() != 0) ||
		op.timerWait || op.waiterActive || op.next != nil || op.workNext != nil {
		throw("runtime: invalid completed stackless coroutine operation")
	}
	s := op.scheduler
	task := op.task
	op.stacklessCoroOperationState = stacklessCoroOperationState{}
	return s, task
}

// recycleOperationLocked retains a completed operation for this scheduler and
// reports whether it fit. The operation address is a race-detector
// synchronization identity for channel operations, so race builds must not
// reuse it for another operation.
func (s *stacklessCoroScheduler) recycleOperationLocked(op *stacklessCoroOperation) bool {
	if raceenabled {
		return false
	}
	if s.freeOperationCount > stacklessCoroOperationCacheSize {
		throw("runtime: invalid stackless coroutine operation cache size")
	}
	if s.freeOperationCount == stacklessCoroOperationCacheSize {
		return false
	}
	op.next = s.freeOperations
	s.freeOperations = op
	s.freeOperationCount++
	return true
}

// takeStacklessCoroSharedOperation removes one operation from the overflow
// shared by scheduler-local caches.
func takeStacklessCoroSharedOperation() *stacklessCoroOperation {
	if raceenabled {
		return nil
	}
	lock(&stacklessCoroSharedOperations.lock)
	if !validStacklessCoroSharedOperationCache(
		stacklessCoroSharedOperations.head,
		stacklessCoroSharedOperations.count) {
		throw("runtime: invalid shared stackless coroutine operation cache")
	}
	op := stacklessCoroSharedOperations.head
	if op != nil {
		stacklessCoroSharedOperations.head = op.next
		stacklessCoroSharedOperations.count--
		op.next = nil
	}
	unlock(&stacklessCoroSharedOperations.lock)
	return op
}

// validStacklessCoroSharedOperationCache checks the facts represented by the
// shared cache's head and bounded byte count.
func validStacklessCoroSharedOperationCache(head *stacklessCoroOperation,
	count uint8) bool {
	return (head == nil) == (count == 0) &&
		int(count) <= stacklessCoroSharedOperationCacheSize
}

// cacheStacklessCoroSharedOperation retains an operation that did not fit in
// its scheduler's local cache.
func cacheStacklessCoroSharedOperation(op *stacklessCoroOperation) {
	if raceenabled {
		return
	}
	lock(&stacklessCoroSharedOperations.lock)
	if !validStacklessCoroSharedOperationCache(
		stacklessCoroSharedOperations.head,
		stacklessCoroSharedOperations.count) {
		throw("runtime: invalid shared stackless coroutine operation cache")
	}
	if int(stacklessCoroSharedOperations.count) == stacklessCoroSharedOperationCacheSize {
		unlock(&stacklessCoroSharedOperations.lock)
		return
	}
	op.next = stacklessCoroSharedOperations.head
	stacklessCoroSharedOperations.head = op
	stacklessCoroSharedOperations.count++
	unlock(&stacklessCoroSharedOperations.lock)
}

func completeStacklessCoroOperation(op *stacklessCoroOperation) {
	s, task := clearStacklessCoroOperation(op)
	lock(&s.lock)
	completedDuringResume := task.resuming
	s.readyLocked(task)
	cachedLocally := s.recycleOperationLocked(op)
	unlock(&s.lock)
	if !cachedLocally {
		cacheStacklessCoroSharedOperation(op)
	}
	if !completedDuringResume {
		s.signal()
	}
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
	completedDuringResume := task.resuming
	s.readyLocked(task)
	cachedLocally := s.recycleOperationLocked(op)
	unlock(&s.lock)
	if !cachedLocally {
		cacheStacklessCoroSharedOperation(op)
	}
	if !completedDuringResume {
		s.signal()
	}
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
	if task != &s.root && s.initialRootPrivate {
		s.initialRootPrivate = false
	}
	task.state = stacklessCoroTaskRunnable
	task.next = nil
	var runnable uint32
	if stacklessCoroIsPullComparison(s) {
		// A pull comparison root has one active structured leaf. An event
		// publishes readiness for that leaf and rings the root doorbell; it
		// does not enqueue the exact continuation. Child completion replaces
		// the leaf with its parent before the next polling episode.
		s.head = task
	} else {
		if s.tail == nil {
			s.head = task
		} else {
			s.tail.next = task
		}
		s.tail = task
		runnable = s.runnableState.Add(1)
	}
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
	pull := stacklessCoroIsPullComparison(s)
	task := s.head
	if task == nil || pull && task.state != stacklessCoroTaskRunnable {
		unlock(&s.lock)
		return nil
	}
	if !pull {
		s.head = task.next
		if s.head == nil {
			s.tail = nil
		}
		s.runnableState.Add(-1)
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

// takeInitialRoot consumes the queue entry installed while the scheduler was
// private. The caller must retain exclusive ownership until this transition
// completes; later episodes and replacement executors use take.
func (s *stacklessCoroScheduler) takeInitialRoot() *stacklessCoroTask {
	pull := stacklessCoroIsPullComparison(s)
	task := &s.root
	runnable := s.runnableState.Load()
	if s.head != task || (!pull && s.tail != task) || task.next != nil ||
		task.state != stacklessCoroTaskRunnable || task.resuming ||
		task.readyPending || (pull && runnable != 0) || (!pull && runnable != 1) {
		throw("runtime: invalid initial stackless coroutine root")
	}
	if !pull {
		s.head = nil
		s.tail = nil
		s.runnableState.Store(0)
	}
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	if raceenabled {
		raceacquire(unsafe.Pointer(task))
	}
	return task
}

func (s *stacklessCoroScheduler) yield(task *stacklessCoroTask, native bool) (resumeDirectly, foreignReturner bool) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine yield")
	}
	if native && !raceenabled && !stacklessCoroIsPullComparison(s) && !s.rootComplete() &&
		s.head == nil && s.tail == nil && s.runnableState.Load() == 0 {
		// With no competing work, a queue round trip would select this task
		// again. Keep its running ownership on the current native executor.
		unlock(&s.lock)
		return true, false
	}
	task.resuming = false
	runnable := s.readyLocked(task)
	unlock(&s.lock)
	return false, runnable&stacklessCoroForeignReturnerBit != 0
}

func (s *stacklessCoroScheduler) waiting(task *stacklessCoroTask,
	native bool) (resumeDirectly bool) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskWaiting || !task.resuming {
		unlock(&s.lock)
		throw("runtime: stackless coroutine wait without operation")
	}
	ready := task.readyPending
	task.readyPending = false
	if ready && native && !raceenabled &&
		!stacklessCoroIsPullComparison(s) && !s.rootComplete() &&
		s.head == nil && s.tail == nil && s.runnableState.Load() == 0 {
		// The producer completed while the initiating resume call was still
		// active. With no competing logical work, retain task ownership on
		// this native executor instead of publishing and taking it again.
		// Race and pull-comparison builds retain their separate handoff.
		task.state = stacklessCoroTaskRunning
		unlock(&s.lock)
		return true
	}
	task.resuming = false
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
	return false
}

func (s *stacklessCoroScheduler) readyAfterPanic(task *stacklessCoroTask) {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.terminal != stacklessCoroTerminalPanic {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine panic recovery")
	}
	s.discardFusedFramesLocked(task)
	task.setFlag(stacklessCoroTaskFusedRequested, false)
	task.setFlag(stacklessCoroTaskFusedPending, false)
	s.cancelReservedFrameTasksLocked(task)
	task.resuming = false
	s.readyLocked(task)
	unlock(&s.lock)
	s.signal()
}

// completeInitialRoot completes a root that has remained private since its
// initial selection. Once a producer or replacement executor can observe the
// scheduler, completion retains the ordinary locked path.
func (s *stacklessCoroScheduler) completeInitialRoot(task *stacklessCoroTask) bool {
	// These facts are monotonic for one initialized scheduler lifetime and are
	// published before an operation producer or replacement executor can
	// observe the scheduler.
	if raceenabled || stacklessCoroIsPullComparison(s) || task != &s.root ||
		s.lifetime.Load() != 0 ||
		s.executorState.Load() != stacklessCoroExecutorStateOff ||
		task.parent != nil || task.next != nil ||
		task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.hasFlag(stacklessCoroTaskFusedFrames) ||
		task.hasFlag(stacklessCoroTaskFusedRequested) ||
		task.hasFlag(stacklessCoroTaskFusedPending) ||
		task.hasFlag(stacklessCoroTaskSwitchPending) ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		return false
	}
	task.resuming = false
	task.state = stacklessCoroTaskComplete
	if !task.hasFlag(stacklessCoroTaskCacheFrame) {
		clearStacklessCoroTaskFrame(task)
	}
	// Publish completion only after the root outcome and frame state are final.
	s.lifetime.Store(stacklessCoroLifetimeRootComplete)
	return true
}

func (s *stacklessCoroScheduler) complete(task *stacklessCoroTask) *stacklessCoroTask {
	lock(&s.lock)
	if task.state != stacklessCoroTaskRunning || !task.resuming ||
		task.readyPending ||
		task.hasFlag(stacklessCoroTaskFusedFrames) ||
		task.hasFlag(stacklessCoroTaskFusedRequested) ||
		task.hasFlag(stacklessCoroTaskFusedPending) ||
		task.hasFlag(stacklessCoroTaskSwitchPending) ||
		task.terminal != stacklessCoroTerminalNone || task.goexit {
		unlock(&s.lock)
		throw("runtime: invalid stackless coroutine completion")
	}
	task.resuming = false
	task.state = stacklessCoroTaskComplete
	if !task.hasFlag(stacklessCoroTaskCacheFrame) {
		clearStacklessCoroTaskFrame(task)
	}
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	parent := task.parent
	task.parent = nil
	if parent == nil {
		if task == &s.root {
			s.lifetime.Or(stacklessCoroLifetimeRootComplete)
		} else {
			s.recycleTaskLocked(task)
		}
		unlock(&s.lock)
		if task == &s.root {
			s.stopExecutorRuns()
		} else {
			s.signalAll()
		}
		return nil
	}
	if parent.state != stacklessCoroTaskWaiting ||
		parent.terminal != stacklessCoroTerminalNone || parent.goexit {
		unlock(&s.lock)
		throw("runtime: stackless coroutine completed for non-waiting parent")
	}
	// A completed structured child can return its already-suspended parent to
	// this executor without publishing and immediately consuming a queue
	// entry. Preserve the queue path when another task is ready, while the
	// parent is still returning Wait, or when a build needs a distinct
	// synchronization or comparison policy.
	if !raceenabled && !stacklessCoroIsPullComparison(s) &&
		!parent.resuming && !parent.readyPending && parent.next == nil &&
		s.head == nil && s.tail == nil {
		parent.state = stacklessCoroTaskRunning
		parent.resuming = true
		s.recycleTaskLocked(task)
		unlock(&s.lock)
		return parent
	}
	s.readyLocked(parent)
	s.recycleTaskLocked(task)
	unlock(&s.lock)
	return nil
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
	s.discardFusedFramesLocked(task)
	task.setFlag(stacklessCoroTaskFusedRequested, false)
	task.setFlag(stacklessCoroTaskFusedPending, false)
	s.cancelReservedFrameTasksLocked(task)
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
		s.lifetime.Or(stacklessCoroLifetimeRootComplete)
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
	s.discardFusedFramesLocked(task)
	task.setFlag(stacklessCoroTaskFusedRequested, false)
	task.setFlag(stacklessCoroTaskFusedPending, false)
	s.cancelReservedFrameTasksLocked(task)
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
			s.lifetime.Or(stacklessCoroLifetimeRootComplete)
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
	for !s.rootComplete() {
		// Run ready work on the fixed native stack, but park this ordinary G
		// between episodes. Keeping an idle synthetic executor locked to its M
		// would make every blocking-call handoff pay stoplockedm/startlockedm.
		if coroRunOnNativeStack(s) == nil {
			s.run(false)
			break
		}
		if s.rootComplete() {
			break
		}
		select {
		case <-s.wake:
		case <-s.executorStop:
		}
	}
	s.executorDone <- struct{}{}
}

// enterBlockingExecutor notifies replacement capacity and records an executor
// entering a blocking native operation. It reports whether the executor
// should promptly release its P.
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

func stacklessCoroTimerReady(arg any, sequence uintptr, _ int64) {
	t := arg.(*stacklessCoroTimer)
	if !t.active.CompareAndSwap(sequence, 0) {
		return
	}
	op := t.operation
	if op == nil || op.timer != t || op.id != uint64(sequence) {
		throw("runtime: invalid stackless coroutine timer completion")
	}
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
