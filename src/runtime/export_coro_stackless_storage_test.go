// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroPlainStorageForTest() bool {
	op := new(stacklessCoroOperation)
	s := &stacklessCoroScheduler{
		freeOperations:     op,
		freeOperationCount: 1,
	}
	task := &s.root
	task.context.scheduler = s
	task.state = stacklessCoroTaskRunning
	task.resuming = true
	got := stacklessCoroStartOperation(unsafe.Pointer(&task.context), "plain storage test")
	return got == op && got.stacklessCoroIOState == nil && got.waiter == nil
}
