// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"runtime"
	"sync/atomic"
)

func wideMixedWorker(value *uint64, a, b int) {
	atomic.AddUint64(&taskParkReady, 1)
	<-taskParkGate
	atomic.AddUint64(&taskParkDone, *value+uint64(a+b))
}
func alternatingTypedTasks(tasks int) uint64 {
	taskParkGate = make(chan struct{})
	mixedValue = new(uint64)
	*mixedValue = 1
	atomic.StoreUint64(&taskParkReady, 0)
	atomic.StoreUint64(&taskParkDone, 0)
	for task := 0; task < tasks; task++ {
		if task%2 == 0 {
			go mixedPointerWorker()
		} else {
			go wideMixedWorker(mixedValue, 7, -7)
		}
	}
	for atomic.LoadUint64(&taskParkReady) < uint64(tasks) {
		runtime.Gosched()
	}
	close(taskParkGate)
	for atomic.LoadUint64(&taskParkDone) < uint64(tasks) {
		runtime.Gosched()
	}
	return atomic.LoadUint64(&taskParkDone)
}
