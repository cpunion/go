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
	head    *stacklessCoroTask
	tail    *stacklessCoroTask
	current *stacklessCoroTask
	root    *stacklessCoroTask
}

// coroRun drives one stackless logical goroutine and its children. The
// compiler emits calls to coroRun only for coroutine root adapters.
func coroRun(resume stacklessCoroResume) {
	if resume == nil {
		throw("runtime: nil stackless coroutine resume function")
	}
	s := new(stacklessCoroScheduler)
	root := &stacklessCoroTask{resume: resume}
	s.root = root
	s.ready(root)

	for root.state != stacklessCoroTaskComplete {
		task := s.take()
		if task == nil {
			throw("runtime: stackless coroutine has no runnable task")
		}
		s.current = task
		task.state = stacklessCoroTaskRunning
		action := task.resume(unsafe.Pointer(s))
		s.current = nil

		switch action {
		case stacklessCoroActionYield:
			if task.state != stacklessCoroTaskRunning {
				throw("runtime: invalid stackless coroutine yield")
			}
			s.ready(task)
			// Cooperate with the host scheduler until native executors own
			// independent fixed stacks.
			Gosched()
		case stacklessCoroActionWait:
			if task.state != stacklessCoroTaskWaiting {
				throw("runtime: stackless coroutine wait without operation")
			}
		case stacklessCoroActionComplete:
			if task.state != stacklessCoroTaskRunning {
				throw("runtime: invalid stackless coroutine completion")
			}
			s.complete(task)
		default:
			throw("runtime: invalid stackless coroutine action")
		}
	}
}

// coroAwait transfers execution to child. Completion makes the current task
// runnable; it does not call the parent continuation.
func coroAwait(ctx unsafe.Pointer, child stacklessCoroResume) {
	s := (*stacklessCoroScheduler)(ctx)
	if s == nil || s.current == nil || child == nil {
		throw("runtime: invalid stackless coroutine await")
	}
	parent := s.current
	if parent.state != stacklessCoroTaskRunning {
		throw("runtime: stackless coroutine await outside resume")
	}
	parent.state = stacklessCoroTaskWaiting
	s.ready(&stacklessCoroTask{resume: child, parent: parent})
}

// coroSpawn enqueues an independent logical goroutine. The current task keeps
// running until it returns its next scheduler action.
func coroSpawn(ctx unsafe.Pointer, child stacklessCoroResume) {
	s := (*stacklessCoroScheduler)(ctx)
	if s == nil || s.current == nil || child == nil {
		throw("runtime: invalid stackless coroutine spawn")
	}
	if s.current.state != stacklessCoroTaskRunning {
		throw("runtime: stackless coroutine spawn outside resume")
	}
	s.ready(&stacklessCoroTask{resume: child})
}

func (s *stacklessCoroScheduler) ready(task *stacklessCoroTask) {
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
	task := s.head
	if task == nil {
		return nil
	}
	s.head = task.next
	if s.head == nil {
		s.tail = nil
	}
	task.next = nil
	if task.state != stacklessCoroTaskRunnable {
		throw("runtime: dequeued non-runnable stackless coroutine")
	}
	return task
}

func (s *stacklessCoroScheduler) complete(task *stacklessCoroTask) {
	task.state = stacklessCoroTaskComplete
	task.resume = nil
	parent := task.parent
	task.parent = nil
	if parent == nil {
		return
	}
	if parent.state != stacklessCoroTaskWaiting {
		throw("runtime: stackless coroutine completed for non-waiting parent")
	}
	s.ready(parent)
}
