// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

type StacklessCoroExecutorJoinTest struct {
	s stacklessCoroScheduler
}

// NewStacklessCoroExecutorJoinForTest supplies controlled completion producers.
func NewStacklessCoroExecutorJoinForTest(capacity int) *StacklessCoroExecutorJoinTest {
	j := new(StacklessCoroExecutorJoinTest)
	j.s.executorDone = make(chan bool, capacity)
	j.s.executorState.Store(stacklessCoroExecutorStateStopping)
	j.s.executorCount.Store(4) // One driver and three replacements.
	return j
}

// Increments precede the manager's completion, as in the production manager.
func (j *StacklessCoroExecutorJoinTest) Grow() { j.s.executorCount.Add(2) }

func (j *StacklessCoroExecutorJoinTest) Complete(manager bool) { j.s.executorDone <- manager }

func (j *StacklessCoroExecutorJoinTest) Join() { j.s.stopReplacementExecutors() }
