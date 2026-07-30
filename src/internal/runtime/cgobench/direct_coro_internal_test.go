// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package cgobench

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnableHandoffResult(t *testing.T) {
	tests := []struct {
		name       string
		iterations int
		entered    uint64
		gate       uint64
		elapsed    uint64
		want       uint64
	}{
		{"success", 2, 2, 2, 3, 3},
		{"elapsed failure", 2, 2, 2, failedHandoff, failedHandoff},
		{"entered mismatch", 2, 1, 2, 3, failedHandoff},
		{"gate mismatch", 2, 2, 1, 3, failedHandoff},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runnableHandoffResult(
				test.iterations, test.entered, test.gate, test.elapsed,
			); got != test.want {
				t.Fatalf("runnableHandoffResult() = %d, want %d",
					got, test.want)
			}
		})
	}
}

func TestDirectRunnableHandoffWorkerWaits(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(oldProcs)

	atomic.StoreUint64(&directRunnableEntered, 0)
	atomic.StoreUint64(&directRunnableGate, 0)
	atomic.StoreUint64(&directRunnableEpoch, 1)
	timer := time.AfterFunc(time.Millisecond, func() {
		atomic.StoreUint64(&directRunnableEntered, 1)
	})
	defer timer.Stop()

	directRunnableHandoffWorker()
	if got := atomic.LoadUint64(&directRunnableGate); got != 1 {
		t.Fatalf("direct runnable gate = %d, want 1", got)
	}
}
