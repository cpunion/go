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

type stacklessCoroLargeFrameChunkExportFrame struct {
	data [stacklessCoroFrameCacheSize/stacklessCoroFrameChunkSize + 1]byte
}

type stacklessCoroOversizedFrameChunkExportFrame struct {
	data [stacklessCoroFrameChunkByteLimit/stacklessCoroLargeFrameChunkSize + 1]byte
}

type stacklessCoroPlainParentFrame struct {
	first  uintptr
	second uintptr
	marker uint8
}

var stacklessCoroPlainParentForTest stacklessCoroPlainParentFrame

// StacklessCoroSelfFrameForTest matches the compiler-private fused-frame
// prefix and leaves enough ordinary state for runtime transition tests.
type StacklessCoroSelfFrameForTest struct {
	Parent unsafe.Pointer
	Owner  unsafe.Pointer
	Marker uint8
	State  uint8
	Depth  int
	Value  *int
}

// StacklessCoroLargeSelfFrameForTest exercises the four-element chunk selected
// when eight frames would exceed the runtime's typed-allocation bound.
type StacklessCoroLargeSelfFrameForTest struct {
	Parent unsafe.Pointer
	Owner  unsafe.Pointer
	Marker uint8
	State  uint8
	Depth  int
	Value  *int
	Data   [stacklessCoroFrameCacheSize/stacklessCoroFrameChunkSize + 1]byte
}

// StacklessCoroFusedResumeFrameForTest matches the extended compiler-private
// prefix used when a fused structured await changes resume entry.
type StacklessCoroFusedResumeFrameForTest struct {
	Parent unsafe.Pointer
	Owner  unsafe.Pointer
	Marker uint8
	Resume unsafe.Pointer
	State  uint8
	Depth  int
	Value  *int
}

const (
	StacklessCoroWarmExecutorCount        = stacklessCoroWarmExecutorCount
	StacklessCoroTaskCacheSize            = stacklessCoroTaskCacheSize
	StacklessCoroTaskChunkSize            = stacklessCoroTaskChunkSize
	StacklessCoroFrameChunkSize           = stacklessCoroFrameChunkSize
	StacklessCoroLargeFrameChunkSize      = stacklessCoroLargeFrameChunkSize
	StacklessCoroFrameChunkDirectCount    = stacklessCoroFrameChunkDirectCount
	StacklessCoroFusedFrameDirectFirst    = stacklessCoroFusedFrameDirectFirst
	StacklessCoroFusedFrameDirectLast     = stacklessCoroFusedFrameDirectLast
	StacklessCoroFusedFrameChunkFirst     = stacklessCoroFusedFrameChunkFirst
	StacklessCoroFusedFrameChunkLast      = stacklessCoroFusedFrameChunkLast
	StacklessCoroFusedFrameAllocationMask = stacklessCoroFusedFrameAllocationMask
	StacklessCoroFusedFrameShortChunk     = stacklessCoroFusedFrameShortChunk
	StacklessCoroFrameCacheSize           = stacklessCoroFrameCacheSize
	StacklessCoroOperationCacheSize       = stacklessCoroOperationCacheSize
	StacklessCoroSharedOperationCacheSize = stacklessCoroSharedOperationCacheSize
	StacklessCoroSelectCaseCacheSize      = stacklessCoroSelectCaseCacheSize

	StacklessCoroActionInvalid  = stacklessCoroActionInvalid
	StacklessCoroActionYield    = stacklessCoroActionYield
	StacklessCoroActionWait     = stacklessCoroActionWait
	StacklessCoroActionComplete = stacklessCoroActionComplete
	StacklessCoroActionPanic    = stacklessCoroActionPanic
	StacklessCoroActionGoexit   = stacklessCoroActionGoexit
	StacklessCoroActionSwitch   = stacklessCoroActionSwitch
	StacklessCoroPollErrClosing = pollErrClosing
	StacklessCoroPollErrTimeout = pollErrTimeout
)

func RunStacklessCoroForTest(resume func(unsafe.Pointer) uint8) {
	coroRun(resume)
}

func RunStacklessCoroFrameForTest(frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	coroRunFrame(frame, resume, 0)
}

func RunCachedStacklessCoroFrameForTest(frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8, size uintptr) bool {
	return coroRunFrame(frame, resume, size)
}

func TakeRootStacklessCoroFrameForTest(resume func(unsafe.Pointer) uint8,
	size uintptr) unsafe.Pointer {
	return coroTakeRootFrame(resume, size)
}

func ReleaseRootStacklessCoroFrameForTest(frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8, size uintptr) {
	coroReleaseRootFrame(frame, resume, size)
}

func RunStacklessCoroInlineForTest(resume func(unsafe.Pointer) uint8) {
	RunStacklessCoroFrameInlineForTest(nil, resume)
}

func RunStacklessCoroFrameInlineForTest(frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	s.root.resume = resume
	s.root.context.frame = frame
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

func TakeStacklessCoroSelfFrameForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) unsafe.Pointer {
	frameType := abi.TypeFor[[stacklessCoroFrameChunkSize]StacklessCoroSelfFrameForTest]()
	return coroTakeSelfFrame(ctx, resume,
		unsafe.Sizeof(StacklessCoroSelfFrameForTest{}), frameType)
}

func TakeStacklessCoroLargeSelfFrameForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) unsafe.Pointer {
	frameType := abi.TypeFor[[stacklessCoroLargeFrameChunkSize]StacklessCoroLargeSelfFrameForTest]()
	return coroTakeSelfFrame(ctx, resume,
		unsafe.Sizeof(StacklessCoroLargeSelfFrameForTest{}), frameType)
}

func TakeStacklessCoroLargeFusedFrameForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) unsafe.Pointer {
	frameType := abi.TypeFor[[stacklessCoroLargeFrameChunkSize]StacklessCoroLargeSelfFrameForTest]()
	return coroTakeFusedFrame(ctx, resume,
		unsafe.Sizeof(StacklessCoroLargeSelfFrameForTest{}), frameType, true)
}

func AwaitStacklessCoroSelfFrameForTest(ctx, frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) uint8 {
	return coroAwaitSelfFrame(ctx, frame, resume)
}

func AwaitStacklessCoroLargeFusedFrameForTest(ctx, frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) uint8 {
	return coroAwaitFusedFrame(ctx, frame, resume)
}

func CompleteStacklessCoroSelfFrameForTest(ctx unsafe.Pointer) uint8 {
	return coroCompleteSelfFrame(ctx)
}

func CompleteStacklessCoroLargeFusedFrameForTest(ctx unsafe.Pointer) uint8 {
	return coroCompleteFusedFrame(ctx)
}

func TakeStacklessCoroFusedResumeFrameForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) unsafe.Pointer {
	frameType := abi.TypeFor[[stacklessCoroFrameChunkSize]StacklessCoroFusedResumeFrameForTest]()
	coroRequestFusedFrame(ctx)
	return coroTakeFusedFrame(ctx, resume,
		unsafe.Sizeof(StacklessCoroFusedResumeFrameForTest{}), frameType, false)
}

func TakeStacklessCoroFusedFallbackFrameForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8, size uintptr,
	validChunkType bool) unsafe.Pointer {
	chunkType := abi.TypeFor[uintptr]()
	if validChunkType {
		chunkType = abi.TypeFor[[stacklessCoroFrameChunkSize]StacklessCoroSelfFrameForTest]()
	}
	return coroTakeSelfFrame(ctx, resume, size, chunkType)
}

func TakeStacklessCoroGeneralFusedFallbackFrameForTest(ctx unsafe.Pointer,
	resume func(unsafe.Pointer) uint8, size uintptr,
	validChunkType bool) unsafe.Pointer {
	chunkType := abi.TypeFor[uintptr]()
	if validChunkType {
		chunkType = abi.TypeFor[[stacklessCoroFrameChunkSize]StacklessCoroSelfFrameForTest]()
	}
	return coroTakeFusedFrame(ctx, resume, size, chunkType, true)
}

func AwaitStacklessCoroFusedResumeFrameForTest(ctx, frame unsafe.Pointer,
	resume func(unsafe.Pointer) uint8) uint8 {
	return coroAwaitFusedFrame(ctx, frame, resume)
}

func CompleteStacklessCoroFusedResumeFrameForTest(ctx unsafe.Pointer) uint8 {
	return coroCompleteFusedFrame(ctx)
}

func CheckStacklessCoroPlainParentFusedAwaitForTest() bool {
	parentResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionInvalid
	})
	childResume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankLeafRank)
	parent := &s.root
	parent.resume = parentResume
	parent.state = stacklessCoroTaskRunning
	parent.resuming = true
	parent.setFlag(stacklessCoroTaskFusedPending, true)
	// A root caller need not carry a fused-frame header. These values make an
	// accidental header interpretation fail marker validation deterministically.
	plain := &stacklessCoroPlainParentForTest
	*plain = stacklessCoroPlainParentFrame{
		first: 1, second: 1<<20 - 1, marker: ^uint8(0),
	}
	parent.context.frame = unsafe.Pointer(plain)
	parent.context.scheduler = s

	frame := new(stacklessCoroFusedResumeFrameHeader)
	owner := initializeStacklessCoroTask(new(stacklessCoroTask),
		unsafe.Pointer(frame), childResume, parent)
	owner.setFlag(stacklessCoroTaskCacheFrame, true)
	owner.frameSize = uint16(unsafe.Sizeof(*frame))
	s.reservedTasks = owner

	action := coroAwaitFusedFrame(unsafe.Pointer(&parent.context),
		unsafe.Pointer(frame), childResume)
	return action == stacklessCoroActionSwitch &&
		parent.context.frame == unsafe.Pointer(frame) &&
		stacklessCoroResumeIdentity(parent.resume) ==
			stacklessCoroResumeIdentity(childResume) &&
		parent.hasFlag(stacklessCoroTaskFusedFrames) &&
		!parent.hasFlag(stacklessCoroTaskFusedPending) &&
		parent.hasFlag(stacklessCoroTaskSwitchPending) &&
		s.reservedTasks == nil &&
		frame.parent == unsafe.Pointer(plain) && frame.owner == owner &&
		frame.marker == stacklessCoroFusedFrameResume &&
		frame.resume == stacklessCoroResumeIdentity(parentResume)
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

func ValidStacklessCoroAdaptiveFrameChunksForTest() bool {
	smallSize := unsafe.Sizeof(stacklessCoroFrameChunkExportFrame{})
	largeSize := unsafe.Sizeof(stacklessCoroLargeFrameChunkExportFrame{})
	smallLast := uintptr(stacklessCoroFrameChunkByteLimit /
		stacklessCoroFrameChunkSize)
	largeChunk := abi.TypeFor[[stacklessCoroLargeFrameChunkSize]stacklessCoroLargeFrameChunkExportFrame]()
	oversizedChunk := abi.TypeFor[[stacklessCoroFrameChunkSize]stacklessCoroLargeFrameChunkExportFrame]()
	tooLargeSize := unsafe.Sizeof(stacklessCoroOversizedFrameChunkExportFrame{})
	tooLargeChunk := abi.TypeFor[[stacklessCoroLargeFrameChunkSize]stacklessCoroOversizedFrameChunkExportFrame]()
	shortLast := uint8(stacklessCoroFusedFrameChunkFirst +
		stacklessCoroLargeFrameChunkSize - 1)
	return stacklessCoroFrameChunkLength(smallSize) ==
		stacklessCoroFrameChunkSize &&
		stacklessCoroFrameChunkLength(largeSize) ==
			stacklessCoroLargeFrameChunkSize &&
		stacklessCoroFrameChunkLength(smallLast) ==
			stacklessCoroFrameChunkSize &&
		stacklessCoroFrameChunkLength(smallLast+1) ==
			stacklessCoroLargeFrameChunkSize &&
		validStacklessCoroFrameChunkType(largeChunk, largeSize) &&
		!validStacklessCoroFrameChunkType(tooLargeChunk, tooLargeSize) &&
		!validStacklessCoroFrameChunkType(oversizedChunk, largeSize) &&
		validStacklessCoroFusedFrameMarker(
			stacklessCoroFusedFrameChunkValidated|
				stacklessCoroFusedFrameShortChunk|shortLast) &&
		!validStacklessCoroFusedFrameMarker(
			stacklessCoroFusedFrameShortChunk|shortLast+1) &&
		validStacklessCoroSelfFrameMarker(
			stacklessCoroFusedFrameChunkValidated|
				stacklessCoroFusedFrameShortChunk|shortLast)
}

func StacklessCoroAdaptiveFrameChunkMarkersForTest() bool {
	largeSize := unsafe.Sizeof(stacklessCoroLargeFrameChunkExportFrame{})
	directFirst, directLast, first, last :=
		stacklessCoroFrameChunkBounds(largeSize)
	if directFirst != stacklessCoroShortFrameChunkDirectFirst ||
		directLast != stacklessCoroShortFrameChunkDirectLast ||
		first != stacklessCoroShortFrameChunkFirst ||
		last != stacklessCoroShortFrameChunkLast ||
		isStacklessCoroFrameChunkElement(directLast) ||
		!isStacklessCoroFrameChunkElement(first) ||
		!isStacklessCoroFrameChunkElement(last) {
		return false
	}
	resume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	for _, test := range [...]struct {
		marker uint16
		want   uint16
	}{
		{marker: directFirst, want: directFirst - 1},
		{marker: directLast, want: first},
		{marker: first, want: first - 1},
		{marker: last, want: first},
	} {
		parent := &stacklessCoroTask{resume: resume, frameSize: test.marker}
		task := &stacklessCoroTask{resume: resume}
		s := new(stacklessCoroScheduler)
		s.markUncachedFrameLineageLocked(task, parent,
			unsafe.Pointer(new(byte)))
		if task.frameSize != test.want {
			return false
		}
	}
	return true
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

func StacklessCoroOperationSizeForTest() uintptr {
	return unsafe.Sizeof(stacklessCoroOperation{})
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
	free := int(s.overflowTaskCounts >> stacklessCoroFreeOverflowTaskCountShift)
	direct := int(s.overflowTaskCounts & stacklessCoroDirectOverflowTaskCountMask)
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
		resume: resume, parent: firstParent,
		flags: stacklessCoroTaskCacheFrame, frameSize: 1,
		context: stacklessCoroContext{frame: unsafe.Pointer(new(byte))},
	}
	second := &stacklessCoroTask{
		resume: resume, parent: secondParent,
		flags: stacklessCoroTaskCacheFrame, frameSize: 1,
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
		second.parent == secondParent &&
		second.hasFlag(stacklessCoroTaskCacheFrame) &&
		s.cachedFrameTasks == 1 && s.cachedFrameBytes == 1 &&
		recycled &&
		first.resume == nil && first.context.frame == nil &&
		!first.hasFlag(stacklessCoroTaskCacheFrame)
	unlock(&s.lock)
	return valid
}

func StacklessCoroPreferPlainTaskForTest() bool {
	resume := stacklessCoroResume(func(unsafe.Pointer) uint8 {
		return stacklessCoroActionComplete
	})
	cached := &stacklessCoroTask{
		resume: resume, flags: stacklessCoroTaskCacheFrame, frameSize: 1,
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
		freeTasks:          overflow,
		overflowTaskCounts: 1 << stacklessCoroFreeOverflowTaskCountShift,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	task := s.newTaskLocked(nil, resume, nil)
	valid := task != overflow && task.frameSize == 0 &&
		s.freeTasks == overflow && overflow.next == nil &&
		s.overflowTaskCounts>>stacklessCoroFreeOverflowTaskCountShift == 1
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
			resume:    resume,
			context:   stacklessCoroContext{frame: unsafe.Pointer(new(uintptr))},
			flags:     stacklessCoroTaskCacheFrame,
			frameSize: frameSize,
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
		freeTasks:          cached,
		freePlainTaskCount: 1,
		freeFrameBytes:     frameSize,
		cachedFrameTasks:   1,
		overflowTaskCounts: 1 << stacklessCoroFreeOverflowTaskCountShift,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	task := s.newTaskLocked(nil, resume, nil)
	valid := task == plain && s.freeTasks == cached && cached.next == overflow &&
		overflow.next == nil && s.freePlainTaskCount == 0 &&
		s.overflowTaskCounts>>stacklessCoroFreeOverflowTaskCountShift == 1
	unlock(&s.lock)
	if !valid {
		return false
	}

	cached = newCachedTask()
	overflow = newOverflowTask()
	cached.next = overflow
	s = &stacklessCoroScheduler{
		freeTasks:          cached,
		freeFrameBytes:     frameSize,
		cachedFrameTasks:   1,
		overflowTaskCounts: 1 << stacklessCoroFreeOverflowTaskCountShift,
	}
	lockInit(&s.lock, lockRankLeafRank)
	parent := &stacklessCoroTask{frameSize: stacklessCoroUncachedFrameLineage}
	frame := unsafe.Pointer(new(uintptr))
	lock(&s.lock)
	task = s.newTaskLocked(frame, resume, parent)
	valid = task == overflow && s.freeTasks == cached && cached.next == nil &&
		s.overflowTaskCounts>>stacklessCoroFreeOverflowTaskCountShift == 0
	unlock(&s.lock)
	if !valid {
		return false
	}

	overflow = newOverflowTask()
	s = &stacklessCoroScheduler{
		freeTasks:          overflow,
		overflowTaskCounts: 1 << stacklessCoroFreeOverflowTaskCountShift,
	}
	lockInit(&s.lock, lockRankLeafRank)
	lock(&s.lock)
	task = s.newTaskLocked(frame, resume, nil)
	valid = task != overflow && task.context.frame == frame &&
		s.freeTasks == overflow && overflow.next == nil &&
		s.overflowTaskCounts>>stacklessCoroFreeOverflowTaskCountShift == 1
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

func StacklessCoroSharedOperationCountForTest() int {
	lock(&stacklessCoroSharedOperations.lock)
	count := int(stacklessCoroSharedOperations.count)
	unlock(&stacklessCoroSharedOperations.lock)
	return count
}

func DrainStacklessCoroSharedOperationsForTest() int {
	lock(&stacklessCoroSharedOperations.lock)
	count := 0
	for op := stacklessCoroSharedOperations.head; op != nil; {
		next := op.next
		op.next = nil
		op = next
		count++
	}
	if count != int(stacklessCoroSharedOperations.count) {
		unlock(&stacklessCoroSharedOperations.lock)
		throw("runtime: invalid shared stackless coroutine operation cache drain")
	}
	stacklessCoroSharedOperations.head = nil
	stacklessCoroSharedOperations.count = 0
	unlock(&stacklessCoroSharedOperations.lock)
	return count
}

func CheckStacklessCoroSharedOperationCacheForTest() bool {
	op := new(stacklessCoroOperation)
	return validStacklessCoroSharedOperationCache(nil, 0) &&
		!validStacklessCoroSharedOperationCache(nil, 1) &&
		!validStacklessCoroSharedOperationCache(op, 0) &&
		validStacklessCoroSharedOperationCache(
			op, uint8(stacklessCoroSharedOperationCacheSize)) &&
		!validStacklessCoroSharedOperationCache(
			op, uint8(stacklessCoroSharedOperationCacheSize+1))
}

func CheckStacklessCoroSharedOperationPanicForTest() bool {
	if raceenabled {
		return true
	}
	s := new(stacklessCoroScheduler)
	lockInit(&s.lock, lockRankLeafRank)
	operations := new([stacklessCoroOperationCacheSize]stacklessCoroOperation)
	for i := range operations {
		op := &operations[i]
		op.next = s.freeOperations
		s.freeOperations = op
		s.freeOperationCount++
	}
	task := &stacklessCoroTask{state: stacklessCoroTaskWaiting}
	op := &stacklessCoroOperation{
		stacklessCoroOperationState: stacklessCoroOperationState{
			id:        1,
			scheduler: s,
			task:      task,
		},
	}
	value := plainError("shared operation panic")
	panicStacklessCoroOperation(op, value)

	lock(&s.lock)
	localValid := s.freeOperationCount == stacklessCoroOperationCacheSize &&
		task.state == stacklessCoroTaskRunnable &&
		task.terminal == stacklessCoroTerminalPanic &&
		s.terminalValues[task] == value
	unlock(&s.lock)
	lock(&stacklessCoroSharedOperations.lock)
	sharedValid := stacklessCoroSharedOperations.head == op &&
		stacklessCoroSharedOperations.count == 1
	unlock(&stacklessCoroSharedOperations.lock)
	return localValid && sharedValid
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

func StartStacklessCoroSendIntForTest(ctx unsafe.Pointer, channel chan<- int, value *int) {
	startStacklessCoroChannel(ctx, *(**hchan)(unsafe.Pointer(&channel)),
		unsafe.Pointer(value), nil, true)
}

func StartStacklessCoroSendForTest(ctx unsafe.Pointer, channel any,
	value unsafe.Pointer) {
	startStacklessCoroChannel(ctx, (*hchan)(efaceOf(&channel).data),
		value, nil, true)
}

func StartStacklessCoroRecvIntForTest(ctx unsafe.Pointer, channel <-chan int,
	value *int, received *bool) {
	startStacklessCoroChannel(ctx, *(**hchan)(unsafe.Pointer(&channel)),
		unsafe.Pointer(value), received, false)
}

func StartStacklessCoroRecvForTest(ctx unsafe.Pointer, channel any,
	value unsafe.Pointer, received *bool) {
	startStacklessCoroChannel(ctx, (*hchan)(efaceOf(&channel).data),
		value, received, false)
}

func SendIntStacklessCoroForTest(ctx unsafe.Pointer, channel chan<- int,
	value *int) bool {
	return coroChanSend(ctx, *(**hchan)(unsafe.Pointer(&channel)),
		unsafe.Pointer(value))
}

func SendStacklessCoroForTest(ctx unsafe.Pointer, channel any,
	value unsafe.Pointer) bool {
	return coroChanSend(ctx, (*hchan)(efaceOf(&channel).data), value)
}

func RecvIntStacklessCoroForTest(ctx unsafe.Pointer, channel <-chan int,
	value *int, received *bool) bool {
	return coroChanRecv(ctx, *(**hchan)(unsafe.Pointer(&channel)),
		unsafe.Pointer(value), received)
}

func RecvStacklessCoroForTest(ctx unsafe.Pointer, channel any,
	value unsafe.Pointer, received *bool) bool {
	return coroChanRecv(ctx, (*hchan)(efaceOf(&channel).data), value, received)
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

type StacklessCoroSelectStorageForTest struct {
	selection stacklessCoroSelect
	cases     [stacklessCoroSelectCaseCacheSize + 1]scase
	chosen    int
	received  bool
}

func NewStacklessCoroSelectStorageForTest() *StacklessCoroSelectStorageForTest {
	return new(StacklessCoroSelectStorageForTest)
}

func (cache *StacklessCoroSelectStorageForTest) Cycle(n int) {
	if n < 0 || n > len(cache.cases) {
		throw("runtime: invalid stackless coroutine select cache test size")
	}
	cache.selection.prepare(cache.cases[:n], n, n, 0,
		&cache.chosen, &cache.received)
	cache.selection.clear()
}

func (cache *StacklessCoroSelectStorageForTest) Valid() bool {
	return cache.selection.validCache()
}

func (cache *StacklessCoroSelectStorageForTest) Capacities() (locks, waiters int) {
	return cap(cache.selection.lockOrder), cap(cache.selection.waiters)
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

func StartStacklessCoroSelectForTest(ctx unsafe.Pointer,
	cases *StacklessCoroSelectCasesForTest, block bool, chosen *int,
	received *bool) {
	var first *scase
	if len(cases.cases) != 0 {
		first = &cases.cases[0]
	}
	startStacklessCoroSelect(ctx, first, cases.nsends,
		len(cases.cases)-cases.nsends, block, chosen, received)
}

func SelectStacklessCoroForTest(ctx unsafe.Pointer,
	cases *StacklessCoroSelectCasesForTest, block bool, chosen *int,
	received *bool) bool {
	var first *scase
	if len(cases.cases) != 0 {
		first = &cases.cases[0]
	}
	return coroSelect(ctx, first, cases.nsends,
		len(cases.cases)-cases.nsends,
		block, chosen, received)
}

func StacklessCoroSelectMayBeReadyForTest(
	cases *StacklessCoroSelectCasesForTest) bool {
	return stacklessCoroSelectMayBeReady(cases.cases, cases.nsends)
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
	for index := range stacklessCoroOperations.buckets {
		for op := stacklessCoroOperations.buckets[index]; op != nil; op = op.next {
			count++
		}
	}
	unlock(&stacklessCoroOperations.lock)
	return count
}

func StacklessCoroPollWaitCountForTest() int {
	lock(&stacklessCoroOperations.lock)
	count := 0
	for index := range stacklessCoroOperations.buckets {
		for op := stacklessCoroOperations.buckets[index]; op != nil; op = op.next {
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
	for index := range stacklessCoroOperations.buckets {
		for op := stacklessCoroOperations.buckets[index]; op != nil; op = op.next {
			if op.scheduler == context.scheduler && op.task == context.task() {
				if found != nil {
					unlock(&stacklessCoroOperations.lock)
					throw("runtime: multiple stackless coroutine operations for one task")
				}
				found = op
			}
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

func StacklessCoroRootSchedulerPoolSizeForTest() int {
	count := 0
	for i := range stacklessCoroRootSchedulerCache.slots {
		if stacklessCoroRootSchedulerCache.slots[i].Load() != nil {
			count++
		}
	}
	return count
}

func StacklessCoroRootFramePoolSizeForTest() int {
	count := 0
	for i := range stacklessCoroRootFrameCache.slots {
		if stacklessCoroRootFrameCache.slots[i].state.Load() ==
			stacklessCoroRootFrameSlotReady {
			count++
		}
	}
	return count
}

func ClearStacklessCoroRootFramePoolForTest() int {
	count := 0
	for i := range stacklessCoroRootFrameCache.slots {
		slot := &stacklessCoroRootFrameCache.slots[i]
		if slot.state.CompareAndSwap(stacklessCoroRootFrameSlotReady,
			stacklessCoroRootFrameSlotBusy) {
			slot.cached = stacklessCoroRootFrame{}
			slot.state.Store(stacklessCoroRootFrameSlotEmpty)
			count++
		}
	}
	return count
}

func SchedulerStacklessCoroForTest(ctx unsafe.Pointer) unsafe.Pointer {
	context := (*stacklessCoroContext)(ctx)
	return unsafe.Pointer(context.scheduler)
}

func RootEmbeddedStacklessCoroForTest(ctx unsafe.Pointer) bool {
	context := (*stacklessCoroContext)(ctx)
	return context.task() == &context.scheduler.root
}

func newEarlyReadyStacklessCoroForTest() (*stacklessCoroScheduler,
	*stacklessCoroTask, *stacklessCoroOperation) {
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
	op := stacklessCoroStartOperation(
		unsafe.Pointer(&task.context), "early ready test")
	op.id = 1
	task.context.scheduler = nil
	return s, task, op
}

func CheckEarlyReadyStacklessCoroForTest() bool {
	s, task, op := newEarlyReadyStacklessCoroForTest()
	completeStacklessCoroOperation(op)

	lock(&s.lock)
	deferred := s.head == nil &&
		task.state == stacklessCoroTaskWaiting &&
		task.resuming && task.readyPending
	unlock(&s.lock)
	if !deferred || len(s.wake) != 0 {
		return false
	}

	direct := s.waiting(task, true)
	if raceenabled {
		return !direct && len(s.wake) == 1 && s.take() == task &&
			task.state == stacklessCoroTaskRunning && task.resuming &&
			!task.readyPending && s.freeOperationCount == 0
	}
	return direct && len(s.wake) == 0 && s.head == nil && s.tail == nil &&
		task.state == stacklessCoroTaskRunning && task.resuming &&
		!task.readyPending && s.freeOperations == op &&
		s.freeOperationCount == 1
}

func CheckEarlyReadyManagedFallbackStacklessCoroForTest() bool {
	s, task, op := newEarlyReadyStacklessCoroForTest()
	completeStacklessCoroOperation(op)
	return !s.waiting(task, false) && len(s.wake) == 1 &&
		s.take() == task && task.state == stacklessCoroTaskRunning &&
		task.resuming && !task.readyPending
}

func CheckEarlyReadyOrderingStacklessCoroForTest() bool {
	s, task, op := newEarlyReadyStacklessCoroForTest()
	other := new(stacklessCoroTask)
	lock(&s.lock)
	s.readyLocked(other)
	unlock(&s.lock)
	completeStacklessCoroOperation(op)
	if s.waiting(task, true) || len(s.wake) != 1 {
		return false
	}
	return s.take() == other && s.take() == task && s.head == nil &&
		s.tail == nil && s.runnableState.Load() == 0
}

func CheckDelayedReadySignalStacklessCoroForTest() bool {
	s, task, op := newEarlyReadyStacklessCoroForTest()
	if s.waiting(task, true) || len(s.wake) != 0 {
		return false
	}
	completeStacklessCoroOperation(op)
	return len(s.wake) == 1 && s.take() == task &&
		task.state == stacklessCoroTaskRunning && task.resuming
}

func CheckCompletionHandoffStacklessCoroForTest(parentResuming,
	ready bool) (direct, pending, parentQueued, readyFirst bool) {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	parent := &s.root
	parent.state = stacklessCoroTaskWaiting
	parent.resuming = parentResuming
	child := &stacklessCoroTask{
		parent:   parent,
		state:    stacklessCoroTaskRunning,
		resuming: true,
	}
	var readyTask *stacklessCoroTask
	if ready {
		readyTask = new(stacklessCoroTask)
		lock(&s.lock)
		s.readyLocked(readyTask)
		unlock(&s.lock)
	}
	next := s.complete(child)
	lock(&s.lock)
	direct = next == parent && parent.state == stacklessCoroTaskRunning &&
		parent.resuming
	pending = parent.state == stacklessCoroTaskWaiting && parent.resuming &&
		parent.readyPending
	for task := s.head; task != nil; task = task.next {
		if task == parent {
			parentQueued = true
		}
	}
	readyFirst = !ready || s.head == readyTask && readyTask.next == parent
	unlock(&s.lock)
	return
}

func CheckStacklessCoroOperationRegistryForTest() bool {
	first := new(stacklessCoroOperation)
	second := new(stacklessCoroOperation)
	firstID := registerStacklessCoroOperation(first)
	// Leave exactly one bucket rotation between consecutive registrations so
	// the test covers a real collision.
	lock(&stacklessCoroOperations.lock)
	for range stacklessCoroOperationRegistryBucketCount - 1 {
		stacklessCoroOperations.next++
		if stacklessCoroOperations.next == 0 {
			stacklessCoroOperations.next++
		}
	}
	unlock(&stacklessCoroOperations.lock)
	secondID := registerStacklessCoroOperation(second)
	valid := firstID != 0 && secondID != 0 && firstID != secondID &&
		firstID%stacklessCoroOperationRegistryBucketCount ==
			secondID%stacklessCoroOperationRegistryBucketCount &&
		findStacklessCoroOperation(firstID) == first &&
		findStacklessCoroOperation(secondID) == second &&
		takeStacklessCoroOperation(firstID) == first &&
		findStacklessCoroOperation(firstID) == nil &&
		takeStacklessCoroOperation(firstID) == nil
	if takeStacklessCoroOperation(secondID) != second {
		valid = false
	}
	return valid
}

func CheckStacklessCoroOperationRegistryScanForTest() bool {
	s := new(stacklessCoroScheduler)
	operations := make([]*stacklessCoroOperation,
		stacklessCoroIdlePollScanLimit+1)
	ids := make([]uint64, len(operations))
	for i := range operations {
		if i != 0 {
			lock(&stacklessCoroOperations.lock)
			for range stacklessCoroOperationRegistryBucketCount - 1 {
				stacklessCoroOperations.next++
				if stacklessCoroOperations.next == 0 {
					stacklessCoroOperations.next++
				}
			}
			unlock(&stacklessCoroOperations.lock)
		}
		op := new(stacklessCoroOperation)
		op.scheduler = s
		op.task = new(stacklessCoroTask)
		op.async = true
		operations[i] = op
		ids[i] = registerStacklessCoroOperation(op)
	}
	bucket := int(ids[0] % stacklessCoroOperationRegistryBucketCount)
	lock(&stacklessCoroOperations.lock)
	oldScan := stacklessCoroOperations.scan
	stacklessCoroOperations.scan = uint16(bucket)
	unlock(&stacklessCoroOperations.lock)
	valid := stacklessCoroPollReadAtIdle(s, nil, nil) == nil
	lock(&stacklessCoroOperations.lock)
	valid = valid && int(stacklessCoroOperations.scan) ==
		(bucket+1)%stacklessCoroOperationRegistryBucketCount
	stacklessCoroOperations.scan = oldScan
	unlock(&stacklessCoroOperations.lock)
	for i := range operations {
		if takeStacklessCoroOperation(ids[i]) != operations[i] {
			valid = false
		}
	}
	return valid
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
