// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import (
	"internal/abi"
	"unsafe"
)

type stacklessCoroFrameChunkExportFrame struct {
	depth   int
	state   int
	tracker unsafe.Pointer
}

const (
	StacklessCoroWarmExecutorCount     = stacklessCoroWarmExecutorCount
	StacklessCoroTaskCacheSize         = stacklessCoroTaskCacheSize
	StacklessCoroTaskChunkSize         = stacklessCoroTaskChunkSize
	StacklessCoroFrameChunkSize        = stacklessCoroFrameChunkSize
	StacklessCoroFrameChunkDirectCount = stacklessCoroFrameChunkDirectCount
	StacklessCoroFrameCacheSize        = stacklessCoroFrameCacheSize
	StacklessCoroOperationCacheSize    = stacklessCoroOperationCacheSize

	StacklessCoroActionInvalid  = stacklessCoroActionInvalid
	StacklessCoroActionYield    = stacklessCoroActionYield
	StacklessCoroActionWait     = stacklessCoroActionWait
	StacklessCoroActionComplete = stacklessCoroActionComplete
	StacklessCoroActionPanic    = stacklessCoroActionPanic
	StacklessCoroActionGoexit   = stacklessCoroActionGoexit
	StacklessCoroPollErrClosing = pollErrClosing
	StacklessCoroPollErrTimeout = pollErrTimeout
)

func RunStacklessCoroForTest(resume func(unsafe.Pointer) uint8) {
	coroRun(resume)
}

func RunStacklessCoroFrameForTest(frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	coroRunFrame(frame, resume)
}

func RunStacklessCoroInlineForTest(resume func(unsafe.Pointer) uint8) {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	s.root.resume = resume
	s.ready(&s.root, false)
	s.run(false)
	s.finish()
}

func RunDetachedStacklessCoroForTest(root, detached func(unsafe.Pointer) uint8) {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	s.root.resume = root
	s.ready(&s.root, false)
	s.ready(&stacklessCoroTask{resume: detached}, false)
	s.run(false)
	throw("runtime: detached stackless coroutine test returned")
}

func AwaitStacklessCoroForTest(ctx unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroAwait(ctx, resume)
}

func AwaitStacklessCoroFrameForTest(ctx, frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	coroAwaitFrame(ctx, frame, resume)
}

func TakeStacklessCoroFrameForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8, size uintptr) unsafe.Pointer {
	return coroTakeFrame(ctx, resume, size)
}

func TakeStacklessCoroFrameChunkForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8, size uintptr) (unsafe.Pointer, bool) {
	chunk := false
	if ctx != nil {
		marker := (*stacklessCoroContext)(ctx).task().frameSize
		chunk = marker == stacklessCoroFrameChunkDirectLast ||
			marker == stacklessCoroFrameChunkLast
	}
	frameType := abi.TypeFor[[stacklessCoroFrameChunkSize]stacklessCoroFrameChunkExportFrame]()
	frame := coroTakeFrameChunk(ctx, resume, size, frameType)
	return frame, chunk && frame != nil
}

func ValidStacklessCoroFrameChunkTypesForTest() (valid, missing,
	wrongKind, wrongLength, wrongElementSize bool) {
	integer := abi.TypeFor[uintptr]()
	validChunk := abi.TypeFor[[stacklessCoroFrameChunkSize]stacklessCoroFrameChunkExportFrame]()
	shortChunk := abi.TypeFor[[stacklessCoroFrameChunkSize - 1]stacklessCoroFrameChunkExportFrame]()
	byteChunk := abi.TypeFor[[stacklessCoroFrameChunkSize]byte]()
	size := unsafe.Sizeof(stacklessCoroFrameChunkExportFrame{})
	return validStacklessCoroFrameChunkType(validChunk, size),
		validStacklessCoroFrameChunkType(nil, size),
		validStacklessCoroFrameChunkType(integer, size),
		validStacklessCoroFrameChunkType(shortChunk, size),
		validStacklessCoroFrameChunkType(byteChunk, size)
}

func PanicStacklessCoroForTest(ctx unsafe.Pointer, value any) {
	coroPanic(ctx, value)
}

func PanicPendingStacklessCoroForTest(ctx unsafe.Pointer) bool {
	return coroPanicPending(ctx)
}

func GoexitStacklessCoroForTest(ctx unsafe.Pointer) {
	coroGoexit(ctx)
}

func TerminalActionStacklessCoroForTest(ctx unsafe.Pointer) uint8 {
	return coroTerminalAction(ctx)
}

func DeferTokenStacklessCoroForTest(ctx unsafe.Pointer) unsafe.Pointer {
	return coroDeferToken(ctx)
}

func DeferCallStacklessCoroForTest(token unsafe.Pointer, deferred func()) {
	coroDeferCall(token, deferred)
}

func DeferRunStacklessCoroForTest(token unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroDeferRun(token, resume)
}

func DeferRunStacklessCoroFrameForTest(token, frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	coroDeferRunFrame(token, frame, resume)
}

func DeferGoexitStacklessCoroForTest(token unsafe.Pointer) {
	coroDeferGoexit(token)
}

func StacklessCoroDeferOutcomeErrorsForTest() []string {
	check := func(state stacklessCoroTaskState,
		terminal stacklessCoroTerminalKind, hasValue bool) string {
		s := &stacklessCoroScheduler{
			root: stacklessCoroTask{
				state:    state,
				terminal: terminal,
			},
		}
		root := &s.root
		if hasValue {
			s.terminalValues = map[*stacklessCoroTask]any{root: "panic"}
		}
		_, _, _, reason := takeStacklessCoroDeferOutcome(s)
		return reason
	}
	return []string{
		check(stacklessCoroTaskRunning, stacklessCoroTerminalNone, false),
		check(stacklessCoroTaskComplete, stacklessCoroTerminalNone, true),
		check(stacklessCoroTaskComplete, stacklessCoroTerminalPanic, false),
		check(stacklessCoroTaskComplete, stacklessCoroTerminalKind(255), false),
	}
}

func DeferPanicStacklessCoroForTest(token unsafe.Pointer, value any) {
	coroDeferPanic(token, value)
}

func DeferRecoverStacklessCoroForTest(token unsafe.Pointer) any {
	return coroDeferRecover(token)
}

func StacklessCoroTaskSizeForTest() uintptr {
	return unsafe.Sizeof(stacklessCoroTask{})
}

func StacklessCoroSchedulerSizeForTest() uintptr {
	return unsafe.Sizeof(stacklessCoroScheduler{})
}

func StacklessCoroOverflowTaskCountsForTest(ctx unsafe.Pointer) (int, int) {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task() == nil {
		throw("runtime: invalid stackless coroutine overflow task query")
	}
	s := context.scheduler
	lock(&s.lock)
	free := int(s.freeOverflowTaskCount)
	direct := int(s.directOverflowTaskCount)
	unlock(&s.lock)
	return free, direct
}

func StacklessCoroFreeTaskCountForTest(ctx unsafe.Pointer) int {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task() == nil {
		throw("runtime: invalid stackless coroutine task cache query")
	}
	s := context.scheduler
	lock(&s.lock)
	count := 0
	for task := s.freeTasks; task != nil; task = task.next {
		count++
	}
	unlock(&s.lock)
	return count
}

func StacklessCoroFreeFrameBytesForTest(ctx unsafe.Pointer) int {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task() == nil {
		throw("runtime: invalid stackless coroutine frame cache query")
	}
	s := context.scheduler
	lock(&s.lock)
	bytes := int(s.freeFrameBytes)
	unlock(&s.lock)
	return bytes
}

func StacklessCoroReservedFrameCountForTest(ctx unsafe.Pointer) int {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task() == nil {
		throw("runtime: invalid stackless coroutine frame cache query")
	}
	s := context.scheduler
	lock(&s.lock)
	count := 0
	for task := s.reservedTasks; task != nil; task = task.next {
		count++
	}
	unlock(&s.lock)
	return count
}

func StacklessCoroFrameChunkMarkerIsolationForTest() bool {
	first := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	second := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	parent := &stacklessCoroTask{
		resume: first, frameSize: stacklessCoroFrameChunkDirectFirst,
	}
	task := &stacklessCoroTask{resume: second}
	s := new(stacklessCoroScheduler)
	s.markUncachedFrameLineageLocked(task, parent,
		unsafe.Pointer(new(byte)))
	return task.frameSize == stacklessCoroUncachedFrameLineage
}

func StacklessCoroCancelReservedFramesForTest() bool {
	resume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	firstParent := new(stacklessCoroTask)
	secondParent := new(stacklessCoroTask)
	first := &stacklessCoroTask{
		resume: resume, parent: firstParent, cacheFrame: true, frameSize: 1,
		context: stacklessCoroContext{frame: unsafe.Pointer(new(byte))},
	}
	second := &stacklessCoroTask{
		resume: resume, parent: secondParent, cacheFrame: true, frameSize: 1,
		context: stacklessCoroContext{frame: unsafe.Pointer(new(byte))},
	}
	first.next = second
	s := &stacklessCoroScheduler{
		reservedTasks: first, cachedFrameTasks: 2, cachedFrameBytes: 2,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	s.cancelReservedFrameTasksLocked(firstParent)
	recycled := s.freeTasks == first && s.freePlainTaskCount == 1
	if raceenabled {
		recycled = s.freeTasks == nil && s.freePlainTaskCount == 0
	}
	valid := s.reservedTasks == second && second.next == nil &&
		second.parent == secondParent && second.cacheFrame &&
		s.cachedFrameTasks == 1 && s.cachedFrameBytes == 1 &&
		recycled &&
		first.resume == nil && first.context.frame == nil && !first.cacheFrame
	unlock(&s.lock)
	return valid
}

func StacklessCoroPreferPlainTaskForTest() bool {
	resume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	cached := &stacklessCoroTask{
		resume: resume, cacheFrame: true, frameSize: 1,
		context: stacklessCoroContext{frame: unsafe.Pointer(new(byte))},
	}
	plain := new(stacklessCoroTask)
	cached.next = plain
	s := &stacklessCoroScheduler{
		freeTasks: cached, freePlainTaskCount: 1, freeFrameBytes: 1,
		cachedFrameTasks: 1, cachedFrameBytes: 1,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	got := s.newTaskLocked(nil, resume, nil)
	valid := got == plain && s.freeTasks == cached && cached.next == nil &&
		s.freePlainTaskCount == 0 && s.freeFrameBytes == 1 &&
		s.cachedFrameTasks == 1 && s.cachedFrameBytes == 1
	unlock(&s.lock)
	return valid
}

func StacklessCoroUncachedTaskNotRecycledForTest() bool {
	task := &stacklessCoroTask{
		state:     stacklessCoroTaskComplete,
		frameSize: stacklessCoroUncachedFrameLineage,
	}
	s := &stacklessCoroScheduler{
		cachedFrameTasks: stacklessCoroTaskCacheSize - 1,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	s.recycleTaskLocked(task)
	valid := s.freeTasks == nil && s.freePlainTaskCount == 0 &&
		task.frameSize == 0
	unlock(&s.lock)
	return valid
}

func StacklessCoroOverflowTaskIsolationForTest() bool {
	resume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	overflow := &stacklessCoroTask{
		frameSize: stacklessCoroFreeOverflowTask,
	}
	s := &stacklessCoroScheduler{
		freeTasks: overflow, freeOverflowTaskCount: 1,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	task := s.newTaskLocked(nil, resume, nil)
	valid := task != overflow && task.frameSize == 0 &&
		s.freeTasks == overflow && overflow.next == nil &&
		s.freeOverflowTaskCount == 1
	unlock(&s.lock)
	return valid
}

func StacklessCoroOverflowTaskSelectionForTest() bool {
	resume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	frameSize := uint16(unsafe.Sizeof(uintptr(0)))
	newCachedTask := func() *stacklessCoroTask {
		return &stacklessCoroTask{
			resume:     resume,
			context:    stacklessCoroContext{frame: unsafe.Pointer(new(uintptr))},
			cacheFrame: true,
			frameSize:  frameSize,
		}
	}
	newOverflowTask := func() *stacklessCoroTask {
		return &stacklessCoroTask{frameSize: stacklessCoroFreeOverflowTask}
	}

	cached := newCachedTask()
	overflow := newOverflowTask()
	plain := new(stacklessCoroTask)
	cached.next = overflow
	overflow.next = plain
	s := &stacklessCoroScheduler{
		freeTasks:             cached,
		freePlainTaskCount:    1,
		freeFrameBytes:        frameSize,
		cachedFrameTasks:      1,
		freeOverflowTaskCount: 1,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	task := s.newTaskLocked(nil, resume, nil)
	valid := task == plain && s.freeTasks == cached && cached.next == overflow &&
		overflow.next == nil && s.freePlainTaskCount == 0 &&
		s.freeOverflowTaskCount == 1
	unlock(&s.lock)
	if !valid {
		return false
	}

	cached = newCachedTask()
	overflow = newOverflowTask()
	cached.next = overflow
	s = &stacklessCoroScheduler{
		freeTasks:             cached,
		freeFrameBytes:        frameSize,
		cachedFrameTasks:      1,
		freeOverflowTaskCount: 1,
	}
	lockInit(&s.lock, lockRankLeafRank)
	parent := &stacklessCoroTask{frameSize: stacklessCoroUncachedFrameLineage}
	frame := unsafe.Pointer(new(uintptr))
	lock(&s.lock)
	task = s.newTaskLocked(frame, resume, parent)
	valid = task == overflow && s.freeTasks == cached && cached.next == nil &&
		s.freeOverflowTaskCount == 0
	unlock(&s.lock)
	if !valid {
		return false
	}

	overflow = newOverflowTask()
	s = &stacklessCoroScheduler{
		freeTasks: overflow, freeOverflowTaskCount: 1,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	task = s.newTaskLocked(frame, resume, nil)
	valid = task != overflow && task.context.frame == frame &&
		s.freeTasks == overflow && overflow.next == nil &&
		s.freeOverflowTaskCount == 1
	unlock(&s.lock)
	return valid
}

func StacklessCoroFreeOperationCountForTest(ctx unsafe.Pointer) int {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task() == nil {
		throw("runtime: invalid stackless coroutine operation cache query")
	}
	s := context.scheduler
	lock(&s.lock)
	count := int(s.freeOperationCount)
	unlock(&s.lock)
	return count
}

func SpawnStacklessCoroForTest(ctx unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroSpawn(ctx, resume)
}

func SpawnStacklessCoroFrameForTest(ctx, frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	coroSpawnFrame(ctx, frame, resume)
}

func FrameStacklessCoroForTest(ctx unsafe.Pointer) unsafe.Pointer {
	return coroFrame(ctx)
}

func FrameNeedsClearStacklessCoroForTest(ctx unsafe.Pointer) bool {
	return coroFrameNeedsClear(ctx)
}

func SleepStacklessCoroForTest(ctx unsafe.Pointer, ns int64) bool {
	return coroSleep(ctx, ns)
}

func SendIntStacklessCoroForTest(ctx unsafe.Pointer, channel chan<- int, value *int) {
	coroChanSend(ctx, *(**hchan)(unsafe.Pointer(&channel)),
		unsafe.Pointer(value))
}

func SendStacklessCoroForTest(ctx unsafe.Pointer, channel any,
	value unsafe.Pointer) {
	coroChanSend(ctx, (*hchan)(efaceOf(&channel).data), value)
}

func RecvIntStacklessCoroForTest(ctx unsafe.Pointer, channel <-chan int,
	value *int, received *bool) {
	coroChanRecv(ctx, *(**hchan)(unsafe.Pointer(&channel)),
		unsafe.Pointer(value), received)
}

func RecvStacklessCoroForTest(ctx unsafe.Pointer, channel any,
	value unsafe.Pointer, received *bool) {
	coroChanRecv(ctx, (*hchan)(efaceOf(&channel).data), value, received)
}

type StacklessCoroChannelWaiterCacheForTest struct {
	operation stacklessCoroOperation
}

func NewStacklessCoroChannelWaiterCacheForTest() *StacklessCoroChannelWaiterCacheForTest {
	return new(StacklessCoroChannelWaiterCacheForTest)
}

func (cache *StacklessCoroChannelWaiterCacheForTest) Cycle() {
	var slot *sudog
	sg := newStacklessCoroSudog(&cache.operation, &slot)
	if sg != slot {
		throw("runtime: lost stackless coroutine test waiter root")
	}
	releaseStacklessCoroSudog(unsafe.Pointer(&cache.operation), sg)
}

func (cache *StacklessCoroChannelWaiterCacheForTest) CycleAcrossGC() {
	newStacklessCoroSudog(&cache.operation, nil)
	GC()
	sg := cache.operation.waiter
	if sg == nil {
		throw("runtime: lost active stackless coroutine test waiter")
	}
	releaseStacklessCoroSudog(unsafe.Pointer(&cache.operation), sg)
}

func (cache *StacklessCoroChannelWaiterCacheForTest) Valid() bool {
	return validReleasedStacklessCoroSudog(cache.operation.waiter)
}

type StacklessCoroSelectCasesForTest struct {
	cases  []scase
	nsends int
}

func NewStacklessCoroSelectCasesForTest(channels []any,
	elements []unsafe.Pointer, nsends int) *StacklessCoroSelectCasesForTest {
	if len(channels) != len(elements) || nsends < 0 || nsends > len(channels) {
		throw("runtime: invalid stackless coroutine test select")
	}
	cases := make([]scase, len(channels))
	for i := range channels {
		if channels[i] != nil {
			cases[i].c = (*hchan)(efaceOf(&channels[i]).data)
		}
		cases[i].elem = elements[i]
	}
	return &StacklessCoroSelectCasesForTest{cases: cases, nsends: nsends}
}

func SelectStacklessCoroForTest(ctx unsafe.Pointer,
	cases *StacklessCoroSelectCasesForTest, block bool, chosen *int,
	received *bool) {
	var first *scase
	if len(cases.cases) != 0 {
		first = &cases.cases[0]
	}
	coroSelect(ctx, first, cases.nsends, len(cases.cases)-cases.nsends,
		block, chosen, received)
}

func StacklessCoroChannelWaitersForTest(channel any) (send, recv, logical int) {
	c := (*hchan)(efaceOf(&channel).data)
	lock(&c.lock)
	for sg := c.sendq.first; sg != nil; sg = sg.next {
		send++
		if sg.coro.get() != nil {
			logical++
		}
	}
	for sg := c.recvq.first; sg != nil; sg = sg.next {
		recv++
		if sg.coro.get() != nil {
			logical++
		}
	}
	unlock(&c.lock)
	return
}

func StacklessCoroOperationCountForTest() int {
	lock(&stacklessCoroOperations.lock)
	count := 0
	for op := stacklessCoroOperations.head; op != nil; op = op.next {
		count++
	}
	unlock(&stacklessCoroOperations.lock)
	return count
}

func StacklessCoroPollWaitCountForTest() int {
	lock(&stacklessCoroOperations.lock)
	count := 0
	for op := stacklessCoroOperations.head; op != nil; op = op.next {
		if op.async || op.packet[stacklessCoroPollDescWord] == 0 {
			continue
		}
		pd := (*pollDesc)(unsafe.Pointer(uintptr(op.packet[stacklessCoroPollDescWord])))
		token := pd.rg.Load()
		if token&netpollCoroTagMask == netpollCoroTag &&
			token&^netpollCoroTagMask == uintptr(unsafe.Pointer(op)) {
			count++
		}
	}
	unlock(&stacklessCoroOperations.lock)
	return count
}

func StacklessCoroNetpollWaiterCountForTest() uint32 {
	return netpollWaiters.Load()
}

func StacklessCoroOrdinaryNetpollReadyForTest(fd, iterations int) {
	netpollGenericInit()
	pd, errno := poll_runtime_pollOpen(uintptr(fd))
	if errno != 0 {
		throw("runtime: failed to open ordinary netpoll benchmark descriptor")
	}
	for range iterations {
		pd.rg.Store(pdNil)
		var toRun gList
		if delta := netpollready(&toRun, pd, 'r'); delta != 0 || !toRun.empty() {
			throw("runtime: unexpected ordinary netpoll readiness result")
		}
	}
	pd.rg.Store(pdNil)
	poll_runtime_pollUnblock(pd)
	poll_runtime_pollClose(pd)
}

func StacklessCoroOrdinaryNetpollStatesForTest(fd int) {
	netpollGenericInit()
	pd, errno := poll_runtime_pollOpen(uintptr(fd))
	if errno != 0 {
		throw("runtime: failed to open ordinary netpoll test descriptor")
	}
	check := func(wantDelta int32, want *g) {
		var toRun gList
		if delta := netpollready(&toRun, pd, 'r'); delta != wantDelta {
			throw("runtime: unexpected ordinary netpoll readiness delta")
		}
		if got := toRun.pop(); got != want || !toRun.empty() {
			throw("runtime: unexpected ordinary netpoll readiness list")
		}
	}
	pd.rg.Store(pdNil)
	check(0, nil)
	check(0, nil)
	pd.rg.Store(pdWait)
	check(0, nil)
	gp := getg()
	pd.rg.Store(uintptr(unsafe.Pointer(gp)))
	check(-1, gp)
	pd.rg.Store(pdNil)
	poll_runtime_pollUnblock(pd)
	poll_runtime_pollClose(pd)
}

func StacklessCoroPollArmForTest(fd int, ready, timeout bool) (waiting bool, errno int, tokenMatches bool, delta int32) {
	netpollGenericInit()
	pd, openErr := poll_runtime_pollOpen(uintptr(fd))
	if openErr != 0 {
		return false, openErr, false, 0
	}
	op := new(stacklessCoroOperation)
	if ready {
		var toRun gList
		delta = netpollready(&toRun, pd, 'r')
		if !toRun.empty() {
			throw("runtime: unexpected poll goroutine in coroutine arm test")
		}
		netpollAdjustWaiters(delta)
		delta = 0
	}
	if timeout {
		lock(&pd.lock)
		pd.rd = -1
		pd.publishInfo()
		unlock(&pd.lock)
	}
	waiting, errno = netpollCoroReadArm(pd, op)
	if waiting {
		tokenMatches = netpollCoroReadClaim(pd, op)
		if tokenMatches {
			delta--
			if netpollCoroReadClaim(pd, op) {
				throw("runtime: claimed stackless coroutine poll token twice")
			}
		}
	}
	if timeout {
		lock(&pd.lock)
		pd.rd = 0
		pd.publishInfo()
		unlock(&pd.lock)
	}
	poll_runtime_pollUnblock(pd)
	poll_runtime_pollClose(pd)
	return
}

func StacklessCoroPollIdleRetryForTest(fd int) (skipped, claimed, rearmed bool) {
	netpollGenericInit()
	pd, openErr := poll_runtime_pollOpen(uintptr(fd))
	if openErr != 0 {
		throw("runtime: failed to open coroutine idle-retry descriptor")
	}

	s := new(stacklessCoroScheduler)
	task := new(stacklessCoroTask)
	buffer := make([]byte, 1)
	op := &stacklessCoroOperation{
		stacklessCoroOperationState: stacklessCoroOperationState{
			scheduler: s,
			task:      task,
			fd:        int32(fd),
			buffer:    buffer,
		},
	}
	op.packet[stacklessCoroPollDescWord] = uint64(uintptr(unsafe.Pointer(pd)))
	id := registerStacklessCoroOperation(op)
	waiting, waitErr := netpollCoroReadArm(pd, op)
	if !waiting || waitErr != pollNoError {
		throw("runtime: failed to arm coroutine idle-retry descriptor")
	}

	skipped = stacklessCoroPollReadAtIdle(s, task, nil) == nil
	claimed = stacklessCoroPollReadAtIdle(s, nil, nil) == task
	token := pd.rg.Load()
	rearmed = token&netpollCoroTagMask == netpollCoroTag &&
		token&^netpollCoroTagMask == uintptr(unsafe.Pointer(op))
	if !skipped || !claimed || !rearmed {
		throw("runtime: failed coroutine idle poll retry")
	}
	if !netpollCoroReadClaim(pd, op) {
		throw("runtime: lost rearmed coroutine idle-retry descriptor")
	}
	if takeStacklessCoroOperation(id) != op {
		throw("runtime: lost coroutine idle-retry operation")
	}
	op.packet[stacklessCoroPollDescWord] = 0
	poll_runtime_pollUnblock(pd)
	poll_runtime_pollClose(pd)
	return
}

func StacklessCoroOperationTokenForTest(ctx unsafe.Pointer) unsafe.Pointer {
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil || context.task() == nil {
		throw("runtime: invalid stackless coroutine operation token query")
	}
	lock(&stacklessCoroOperations.lock)
	var found *stacklessCoroOperation
	for op := stacklessCoroOperations.head; op != nil; op = op.next {
		if op.scheduler == context.scheduler && op.task == context.task() {
			if found != nil {
				unlock(&stacklessCoroOperations.lock)
				throw("runtime: multiple stackless coroutine operations for one task")
			}
			found = op
		}
	}
	unlock(&stacklessCoroOperations.lock)
	return unsafe.Pointer(found)
}

type StacklessCoroTimerTokenForTest = stacklessCoroTimerToken

func StartSleepStacklessCoroForTest(ctx unsafe.Pointer, ns int64) StacklessCoroTimerTokenForTest {
	return startStacklessCoroTimer(ctx, ns)
}

func CancelSleepStacklessCoroForTest(token StacklessCoroTimerTokenForTest) bool {
	return cancelStacklessCoroTimer(token)
}

func ReadySleepStacklessCoroForTest(token StacklessCoroTimerTokenForTest) {
	stacklessCoroTimerReady(token.timer, token.sequence, 0)
}

func StacklessCoroTimerOperationForTest(token StacklessCoroTimerTokenForTest) unsafe.Pointer {
	if token.timer == nil {
		return nil
	}
	return unsafe.Pointer(token.timer.operation)
}

func StacklessCoroTimerOwnerForTest(token StacklessCoroTimerTokenForTest) unsafe.Pointer {
	return unsafe.Pointer(token.timer)
}

func CheckStacklessCoroTimerWrapForTest() bool {
	op := new(stacklessCoroOperation)
	first, sequence := op.nextTimer()
	if sequence != 1 {
		return false
	}
	first.sequence = ^uintptr(0)
	second, sequence := op.nextTimer()
	return second != first && sequence == 1 && second.operation == op &&
		op.timer == second
}

func FileReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	coroFileRead(ctx, fd, buffer, n, errno)
}

func PublicFileReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	poll_runtime_coroFileRead(ctx, fd, buffer, n, errno)
}

func SocketReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	coroSocketRead(ctx, fd, buffer, n, errno)
}

func OpenStacklessCoroPollDescForTest(fd int) (unsafe.Pointer, int) {
	netpollGenericInit()
	pd, errno := poll_runtime_pollOpen(uintptr(fd))
	return unsafe.Pointer(pd), errno
}

func UnblockStacklessCoroPollDescForTest(descriptor unsafe.Pointer) {
	poll_runtime_pollUnblock((*pollDesc)(descriptor))
}

func CloseStacklessCoroPollDescForTest(descriptor unsafe.Pointer) {
	poll_runtime_pollClose((*pollDesc)(descriptor))
}

func ExpireReadStacklessCoroPollDescForTest(descriptor unsafe.Pointer) {
	poll_runtime_pollSetDeadline((*pollDesc)(descriptor), -1, 'r')
}

func SocketReadWithPollDescStacklessCoroForTest(ctx, descriptor unsafe.Pointer,
	fd int, buffer []byte, n *int, errno *uintptr) {
	poll_runtime_coroSocketRead(ctx, uintptr(descriptor), fd, buffer, n, errno)
}

func StacklessCoroPollErrorStatusForTest(pollErr int) uintptr {
	return stacklessCoroPollErrorTag | uintptr(pollErr)
}

func StacklessCoroReadLengthForTest(length int) int32 {
	return stacklessCoroReadLength(length)
}

func SocketReadExpiredStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	op := stacklessCoroStartOperation(ctx, "expired read")
	op.fd = int32(fd)
	op.buffer = buffer
	op.n = n
	op.errno = errno
	op.ownsPollDesc = true
	netpollGenericInit()
	pd, openErr := poll_runtime_pollOpen(uintptr(fd))
	if openErr != 0 {
		registerStacklessCoroOperation(op)
		stacklessCoroSocketReadFinish(op, -1, uintptr(openErr))
		return
	}
	op.packet[stacklessCoroPollDescWord] = uint64(uintptr(unsafe.Pointer(pd)))
	registerStacklessCoroOperation(op)
	lock(&pd.lock)
	pd.rd = -1
	pd.publishInfo()
	unlock(&pd.lock)
	stacklessCoroSocketReadAttempt(op)
}

func CallReadStacklessCoroForTest(ctx unsafe.Pointer, call func()) {
	poll_runtime_coroCallRead(ctx, call)
}

func EnterForeignStacklessCoroForTest() {
	coroEnterForeign()
}

func ExitForeignStacklessCoroForTest() {
	coroExitForeign()
}

func ForeignStateStacklessCoroForTest() (incgo, noCallback bool, ncgo int32) {
	gp := getg()
	return gp.m.incgo, gp.nocgocallback, gp.m.ncgo
}

func BlockingReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte) int {
	if len(buffer) == 0 {
		return 0
	}
	coroEnterBlocking(ctx)
	n := read(int32(fd), unsafe.Pointer(&buffer[0]), int32(len(buffer)))
	coroExitBlocking()
	KeepAlive(buffer)
	return int(n)
}

func BlockingBoundaryStacklessCoroForTest(ctx unsafe.Pointer) {
	coroEnterBlocking(ctx)
	coroExitBlocking()
}

func ForeignReturnStateStacklessCoroForTest(ctx unsafe.Pointer) (uint32, bool) {
	context := (*stacklessCoroContext)(ctx)
	scheduler := context.scheduler
	return scheduler.foreignReturners.Load(),
		scheduler.runnableState.Load()&stacklessCoroForeignReturnerBit != 0
}

func ExecutorStateStacklessCoroForTest(ctx unsafe.Pointer) (uint32, uint32) {
	context := (*stacklessCoroContext)(ctx)
	scheduler := context.scheduler
	return scheduler.executorCount.Load(), scheduler.blockingExecutors.Load()
}

func ExecutorChannelsStacklessCoroForTest(ctx unsafe.Pointer) (bool, bool, bool) {
	context := (*stacklessCoroContext)(ctx)
	scheduler := context.scheduler
	return scheduler.wake != nil, scheduler.executorWake != nil,
		scheduler.executorDone != nil
}

func WakeStacklessCoroForTest(ctx unsafe.Pointer) chan struct{} {
	context := (*stacklessCoroContext)(ctx)
	return context.scheduler.wake
}

func SignalStacklessCoroForTest(ctx unsafe.Pointer) {
	context := (*stacklessCoroContext)(ctx)
	context.scheduler.signal()
}

func StacklessCoroWakePoolSizeForTest() int {
	return len(stacklessCoroWakePool.available)
}

func RootEmbeddedStacklessCoroForTest(ctx unsafe.Pointer) bool {
	context := (*stacklessCoroContext)(ctx)
	return context.task() == &context.scheduler.root
}

func CheckEarlyReadyStacklessCoroForTest() bool {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	s.root = stacklessCoroTask{
		state:    stacklessCoroTaskRunning,
		resuming: true,
	}
	task := &s.root
	task.context.scheduler = s

	stacklessCoroStartOperation(unsafe.Pointer(&task.context), "early ready test")
	s.ready(task, true)

	lock(&s.lock)
	deferred := s.head == nil &&
		task.state == stacklessCoroTaskWaiting &&
		task.resuming && task.readyPending
	unlock(&s.lock)
	if !deferred {
		return false
	}

	s.waiting(task)
	return s.take() == task &&
		task.state == stacklessCoroTaskRunning &&
		task.resuming && !task.readyPending
}

func CheckStacklessCoroOperationRegistryForTest() bool {
	first := new(stacklessCoroOperation)
	second := new(stacklessCoroOperation)
	firstID := registerStacklessCoroOperation(first)
	secondID := registerStacklessCoroOperation(second)
	if firstID == 0 || secondID == 0 || firstID == secondID {
		return false
	}
	if findStacklessCoroOperation(firstID) != first {
		return false
	}
	if takeStacklessCoroOperation(firstID) != first {
		return false
	}
	if findStacklessCoroOperation(firstID) != nil {
		return false
	}
	if takeStacklessCoroOperation(firstID) != nil {
		return false
	}
	return takeStacklessCoroOperation(secondID) == second
}

func AsyncReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, result *uint64, errno *uintptr) uint64 {
	op := startStacklessCoroAsync(ctx, fd, result, errno)
	id := op.id
	stacklessCoroReadEnqueue(op)
	return id
}

func FailAsyncStacklessCoroForTest(ctx unsafe.Pointer, result *uint64, errno *uintptr, submitErr uintptr) {
	op := startStacklessCoroAsync(ctx, -1, result, errno)
	failStacklessCoroAsync(op.id, submitErr)
}
