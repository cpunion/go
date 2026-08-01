// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

const (
	StacklessCoroExecutorCount = stacklessCoroExecutorCount

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

func DeferRunStacklessCoroForTest(token unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroDeferRun(token, resume)
}

func DeferGoexitStacklessCoroForTest(token unsafe.Pointer) {
	coroDeferGoexit(token)
}

func StacklessCoroDeferOutcomeErrorsForTest() []string {
	check := func(state stacklessCoroTaskState,
		terminal stacklessCoroTerminalKind, hasValue bool) string {
		root := &stacklessCoroTask{
			state:    state,
			terminal: terminal,
		}
		s := &stacklessCoroScheduler{root: root}
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

func SpawnStacklessCoroForTest(ctx unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroSpawn(ctx, resume)
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

func ForeignReturnersStacklessCoroForTest(ctx unsafe.Pointer) uint32 {
	context := (*stacklessCoroContext)(ctx)
	return context.scheduler.foreignReturners.Load()
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
