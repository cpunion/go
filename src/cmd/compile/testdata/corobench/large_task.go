// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"runtime"
	"sync/atomic"
)

var mixedValue *uint64

func mixedPointerWorker() {
	value := mixedValue
	atomic.AddUint64(&taskParkReady, 1)
	<-taskParkGate
	atomic.AddUint64(&taskParkDone, *value)
}

func mixedTaskParkUntilReleased(tasks, offset int) uint64 {
	taskParkGate = make(chan struct{})
	atomic.StoreUint64(&taskParkReady, 0)
	atomic.StoreUint64(&taskParkDone, 0)
	atomic.StoreUint64(&taskParkRelease, 0)
	mixedValue = new(uint64)
	*mixedValue = 1
	for task := 0; task < tasks; task++ {
		if (task+offset)%4 == 0 {
			go taskParkWorker()
		} else {
			go mixedPointerWorker()
		}
	}
	for atomic.LoadUint64(&taskParkReady) < uint64(tasks) {
		runtime.Gosched()
	}
	for atomic.LoadUint64(&taskParkRelease) == 0 {
		runtime.Gosched()
	}
	close(taskParkGate)
	for atomic.LoadUint64(&taskParkDone) < uint64(tasks) {
		runtime.Gosched()
	}
	return atomic.LoadUint64(&taskParkDone)
}

func mixedTaskParkBursts(rounds, tasks, offset int) uint64 {
	var total uint64
	for round := 0; round < rounds; round++ {
		taskParkGate = make(chan struct{})
		atomic.StoreUint64(&taskParkReady, 0)
		atomic.StoreUint64(&taskParkDone, 0)
		mixedValue = new(uint64)
		*mixedValue = 1
		for task := 0; task < tasks; task++ {
			if (task+offset)%4 == 0 {
				go taskParkWorker()
			} else {
				go mixedPointerWorker()
			}
		}
		for atomic.LoadUint64(&taskParkReady) < uint64(tasks) {
			runtime.Gosched()
		}
		close(taskParkGate)
		for atomic.LoadUint64(&taskParkDone) < uint64(tasks) {
			runtime.Gosched()
		}
		total += atomic.LoadUint64(&taskParkDone)
	}
	return total
}
