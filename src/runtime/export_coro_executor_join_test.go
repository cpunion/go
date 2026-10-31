// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

// StacklessCoroExecutorJoinForTest supplies controlled completion producers.
// Increments precede the manager's completion, as in the production manager.
func StacklessCoroExecutorJoinForTest() (grow func(), complete func(bool), join func()) {
	s := &stacklessCoroScheduler{executorDone: make(chan bool)}
	s.executorState.Store(stacklessCoroExecutorStateStopping)
	s.executorCount.Store(4) // One driver and three replacements.
	return func() { s.executorCount.Add(2) },
		func(manager bool) { s.executorDone <- manager },
		s.stopReplacementExecutors
}
