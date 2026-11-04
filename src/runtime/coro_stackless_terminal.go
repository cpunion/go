// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

// Terminal values and ordinary hosts are cold scheduler metadata. Do not use
// Go maps here: their explicit race hooks do not see the runtime lock that
// protects these records. Annotating that lock would instead introduce false
// synchronization between unrelated logical goroutines' user memory accesses.
// Keep the scheduler's single optional pointer and trace values normally.
type stacklessCoroTerminalValue struct {
	task       *stacklessCoroTask
	value      any
	trace      *stacklessCoroPanicTrace
	next       *stacklessCoroTerminalValue
	deferTrace *stacklessCoroPanicTrace
	exitTrace  *stacklessCoroPanicTrace
	hasValue   bool
}

func (s *stacklessCoroScheduler) terminalTrace(task *stacklessCoroTask) *stacklessCoroPanicTrace {
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			return entry.trace
		}
	}
	return nil
}

func (s *stacklessCoroScheduler) setTerminalPanic(task *stacklessCoroTask, value any, trace *stacklessCoroPanicTrace) {
	s.setTerminalValue(task, value)
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			entry.trace = trace
			return
		}
	}
}

// Callers hold s.lock, or exclusively own a private or completed scheduler.
// The nil task key is reserved for the ordinary host list.
func (s *stacklessCoroScheduler) terminalValue(task *stacklessCoroTask) (any, bool) {
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			return entry.value, entry.hasValue
		}
	}
	return nil, false
}

func (s *stacklessCoroScheduler) setTerminalValue(task *stacklessCoroTask, value any) {
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			entry.value, entry.hasValue = value, true
			return
		}
	}
	s.terminalValues = &stacklessCoroTerminalValue{task: task, value: value, hasValue: true, next: s.terminalValues}
}

func (s *stacklessCoroScheduler) clearTerminalValue(task *stacklessCoroTask) {
	for link := &s.terminalValues; *link != nil; link = &(*link).next {
		entry := *link
		if entry.task == task {
			entry.value, entry.trace, entry.hasValue = nil, nil, false
			if entry.deferTrace == nil && entry.exitTrace == nil {
				*link = entry.next
				entry.task, entry.next = nil, nil
			}
			return
		}
	}
}

// A deferred activation outlives a successful recover until the deferred
// function returns. Keep the former panic's positions through that interval,
// independently of whether a panic value is still present.
func (s *stacklessCoroScheduler) setDeferTrace(task *stacklessCoroTask, trace *stacklessCoroPanicTrace) {
	if trace == nil {
		return
	}
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			entry.deferTrace = trace
			return
		}
	}
	s.terminalValues = &stacklessCoroTerminalValue{task: task, deferTrace: trace, next: s.terminalValues}
}

func (s *stacklessCoroScheduler) deferTrace(task *stacklessCoroTask) *stacklessCoroPanicTrace {
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			return entry.deferTrace
		}
	}
	return nil
}

func (s *stacklessCoroScheduler) clearDeferTrace(task *stacklessCoroTask) {
	for link := &s.terminalValues; *link != nil; link = &(*link).next {
		entry := *link
		if entry.task == task {
			entry.deferTrace = nil
			if !entry.hasValue && entry.exitTrace == nil {
				*link = entry.next
				entry.task, entry.next = nil, nil
			}
			return
		}
	}
}

func (s *stacklessCoroScheduler) terminalSourceTrace(task *stacklessCoroTask) *stacklessCoroPanicTrace {
	if trace := s.terminalTrace(task); trace != nil {
		return trace
	}
	return s.terminalExitTrace(task)
}

func (s *stacklessCoroScheduler) terminalExitTrace(task *stacklessCoroTask) *stacklessCoroPanicTrace {
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			return entry.exitTrace
		}
	}
	return nil
}

func (s *stacklessCoroScheduler) setExitTrace(task *stacklessCoroTask, trace *stacklessCoroPanicTrace) {
	if trace == nil {
		return
	}
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			entry.exitTrace = trace
			return
		}
	}
	s.terminalValues = &stacklessCoroTerminalValue{task: task, exitTrace: trace, next: s.terminalValues}
}

func (s *stacklessCoroScheduler) clearExitTrace(task *stacklessCoroTask) {
	for link := &s.terminalValues; *link != nil; link = &(*link).next {
		entry := *link
		if entry.task == task {
			entry.exitTrace = nil
			if !entry.hasValue && entry.deferTrace == nil {
				*link = entry.next
				entry.task, entry.next = nil, nil
			}
			return
		}
	}
}
