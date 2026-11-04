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
	task  *stacklessCoroTask
	value any
	next  *stacklessCoroTerminalValue
}

// Callers hold s.lock, or exclusively own a private or completed scheduler.
// The nil task key is reserved for the ordinary host list.
func (s *stacklessCoroScheduler) terminalValue(task *stacklessCoroTask) (any, bool) {
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			return entry.value, true
		}
	}
	return nil, false
}

func (s *stacklessCoroScheduler) setTerminalValue(task *stacklessCoroTask, value any) {
	for entry := s.terminalValues; entry != nil; entry = entry.next {
		if entry.task == task {
			entry.value = value
			return
		}
	}
	s.terminalValues = &stacklessCoroTerminalValue{task: task, value: value, next: s.terminalValues}
}

func (s *stacklessCoroScheduler) clearTerminalValue(task *stacklessCoroTask) {
	for link := &s.terminalValues; *link != nil; link = &(*link).next {
		entry := *link
		if entry.task == task {
			*link = entry.next
			entry.task, entry.value, entry.next = nil, nil, nil
			return
		}
	}
}
