// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !darwin && !linux

package runtime

import "unsafe"

type coroIOState struct {
	chanOK          *bool
	sleepTimer      *timer
	selectCases     []scase
	selectLockOrder []uint16
	selectSGList    *sudog
	selectWinner    *sudog
	selectNSends    int
	selectCase      int
	selectOK        bool
	selectChosen    *int
	selectOKOut     *bool
}

func (task *coroTask) ensureIOState() *coroIOState {
	if task.ioState == nil {
		task.ioState = new(coroIOState)
	}
	return task.ioState
}

func coroNetpollHasReady() bool                        { return false }
func coroDrainNetpollOne() *coroTask                   { return nil }
func coroTryPollParkedIO(e1, e2 *coroTask) *coroTask   { return nil }
func coroCompleteIO(task *coroTask) bool               { return false }

//go:nowritebarrier
func coroNetpollWake(taskPtr uintptr) *g { return nil }

func coroSleep(ctx unsafe.Pointer, ns int64) bool {
	if ns <= 0 {
		return false
	}
	task := (*coroTask)(ctx)
	when := nanotime() + ns
	if when < 0 {
		when = maxWhen
	}
	io := task.ensureIOState()
	t := io.sleepTimer
	if t == nil {
		t = new(timer)
		io.sleepTimer = t
	}
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racereleasemerge(unsafe.Pointer(task))
	}
	t.init(coroTimerWake, task)
	t.modify(when, 0, coroTimerWake, task, 0)
	return true
}

func coroPrepareRecycleIO(task *coroTask) {
	if io := task.ioState; io != nil {
		*io = coroIOState{}
	}
}

func coroTimerWake(arg any, seq uintptr, delay int64) {
	task := arg.(*coroTask)
	coroReady(task)
}

func coroGC(ctx unsafe.Pointer) { GC() }
