// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

const (
	StacklessCoroActionInvalid  = stacklessCoroActionInvalid
	StacklessCoroActionYield    = stacklessCoroActionYield
	StacklessCoroActionWait     = stacklessCoroActionWait
	StacklessCoroActionComplete = stacklessCoroActionComplete
	StacklessCoroActionPanic    = stacklessCoroActionPanic
	StacklessCoroActionGoexit   = stacklessCoroActionGoexit
)

func RunStacklessCoroForTest(resume func(unsafe.Pointer) uint8) {
	coroRun(resume)
}

func RunStacklessCoroInlineForTest(resume func(unsafe.Pointer) uint8) {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	rootTask := &stacklessCoroTask{resume: resume}
	s.root = rootTask
	s.ready(rootTask, false)
	s.run(false)
	s.finish()
}

func RunDetachedStacklessCoroForTest(root, detached func(unsafe.Pointer) uint8) {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	rootTask := &stacklessCoroTask{resume: root}
	s.root = rootTask
	s.ready(rootTask, false)
	s.ready(&stacklessCoroTask{resume: detached}, false)
	s.run(false)
	throw("runtime: detached stackless coroutine test returned")
}

func AwaitStacklessCoroForTest(ctx unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroAwait(ctx, resume)
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

func DeferPanicStacklessCoroForTest(token unsafe.Pointer, value any) {
	coroDeferPanic(token, value)
}

func DeferRecoverStacklessCoroForTest(token unsafe.Pointer) any {
	return coroDeferRecover(token)
}

func StacklessCoroTaskSizeForTest() uintptr {
	return unsafe.Sizeof(stacklessCoroTask{})
}

func SpawnStacklessCoroForTest(ctx unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroSpawn(ctx, resume)
}

func SleepStacklessCoroForTest(ctx unsafe.Pointer, ns int64) {
	coroSleep(ctx, ns)
}

func StartSleepStacklessCoroForTest(ctx unsafe.Pointer, ns int64) uint64 {
	return startStacklessCoroTimer(ctx, ns)
}

func CancelSleepStacklessCoroForTest(id uint64) bool {
	return cancelStacklessCoroTimer(id)
}

func FileReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	coroFileRead(ctx, fd, buffer, n, errno)
}

func SocketReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	coroSocketRead(ctx, fd, buffer, n, errno)
}

func CallReadStacklessCoroForTest(ctx unsafe.Pointer, call func()) {
	coroCallRead(ctx, call)
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

func CheckEarlyReadyStacklessCoroForTest() bool {
	s := &stacklessCoroScheduler{
		wake: make(chan struct{}, stacklessCoroExecutorCount),
	}
	lockInit(&s.lock, lockRankLeafRank)
	task := &stacklessCoroTask{
		state:    stacklessCoroTaskRunning,
		resuming: true,
	}
	s.root = task
	context := stacklessCoroContext{
		scheduler: s,
		task:      task,
	}

	stacklessCoroStartOperation(unsafe.Pointer(&context), "early ready test")
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
	stacklessCoroReadEnqueue(op)
	return op.id
}

func FailAsyncStacklessCoroForTest(ctx unsafe.Pointer, result *uint64, errno *uintptr, submitErr uintptr) {
	op := startStacklessCoroAsync(ctx, -1, result, errno)
	failStacklessCoroAsync(op.id, submitErr)
}
