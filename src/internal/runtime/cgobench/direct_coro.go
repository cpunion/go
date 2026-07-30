// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package cgobench

/*
#include <stdint.h>

#cgo LDFLAGS: -lm

#cgo noescape coro_direct_empty
#cgo nocallback coro_direct_empty
#cgo noescape coro_direct_sin_add
#cgo nocallback coro_direct_sin_add
#cgo noescape coro_direct_errno
#cgo nocallback coro_direct_errno
#cgo noescape coro_direct_handoff
#cgo nocallback coro_direct_handoff
#cgo noescape coro_direct_runnable_handoff
#cgo nocallback coro_direct_runnable_handoff

void coro_cgo_empty(void);
void coro_direct_empty(void);
void coro_cgo_sin_add(double, double *);
void coro_direct_sin_add(double, double *);
int64_t coro_cgo_errno(int64_t);
int64_t coro_direct_errno(int64_t);
void coro_cgo_handoff(int, uint32_t *, uint32_t, uint64_t *);
void coro_direct_handoff(int, uint32_t *, uint32_t, uint64_t *);
void coro_cgo_runnable_handoff(uint64_t *, uint64_t, uint64_t *, uint64_t *);
void coro_direct_runnable_handoff(uint64_t *, uint64_t, uint64_t *, uint64_t *);
*/
import "C"
import (
	"runtime"
	corort "runtime/coro"
	"sync/atomic"
)

// CgoCalls invokes an external C leaf through the ordinary cgo path.
func CgoCalls(iterations int) {
	for i := 0; i < iterations; i++ {
		C.coro_cgo_empty()
	}
}

// CgoCall invokes one external C leaf through the ordinary cgo path.
func CgoCall() {
	C.coro_cgo_empty()
}

// DirectCalls invokes the same C leaf shape through the coroutine direct path.
// The package must be compiled with -d=coro=4 to select coroutine lowering.
func DirectCalls(iterations int) {
	for i := 0; i < iterations; i++ {
		C.coro_direct_empty()
	}
}

// DirectCall invokes one external C leaf through the coroutine direct path.
// Calling it repeatedly from another package includes coroutine root setup.
func DirectCall() {
	C.coro_direct_empty()
}

// CgoLibmCalls invokes a libm function through the ordinary cgo path.
func CgoLibmCalls(iterations int) float64 {
	var sum C.double
	for i := 0; i < iterations; i++ {
		value := 0.5 + float64(i&1)*0.125
		C.coro_cgo_sin_add(C.double(value), &sum)
	}
	return float64(sum)
}

// DirectLibmCalls invokes the same libm function through the coroutine direct
// path. The package must be compiled with -d=coro=4.
func DirectLibmCalls(iterations int) float64 {
	var sum C.double
	for i := 0; i < iterations; i++ {
		value := 0.5 + float64(i&1)*0.125
		C.coro_direct_sin_add(C.double(value), &sum)
	}
	return float64(sum)
}

// CgoErrnoCalls invokes the ordinary cgo two-result path with a zero errno.
func CgoErrnoCalls(iterations int) int64 {
	var value C.int64_t
	for i := 0; i < iterations; i++ {
		next, err := C.coro_cgo_errno(value)
		if err != nil {
			return -1
		}
		value = next
	}
	return int64(value)
}

// DirectErrnoCalls invokes the same two-result operation through the
// coroutine direct path. The package must be compiled with -d=coro=4.
func DirectErrnoCalls(iterations int) int64 {
	var value C.int64_t
	for i := 0; i < iterations; i++ {
		next, err := C.coro_direct_errno(value)
		if err != nil {
			return -1
		}
		value = next
	}
	return int64(value)
}

var cgoHandoffGate uint32
var directHandoffGate uint32

const failedHandoff = ^uint64(0)

// ResetCgoHandoff resets the ordinary cgo handoff fixture.
func ResetCgoHandoff() {
	atomic.StoreUint32(&cgoHandoffGate, 0)
}

// ReleaseCgoHandoff releases the ordinary cgo handoff for epoch.
func ReleaseCgoHandoff(epoch uint32) {
	atomic.StoreUint32(&cgoHandoffGate, epoch)
}

// CgoHandoffs measures blocking handoffs through the ordinary cgo path. It
// returns the total time from the C write until the Go worker releases C.
func CgoHandoffs(iterations, writeFD int) uint64 {
	var elapsed uint64
	for i := 0; i < iterations; i++ {
		C.coro_cgo_handoff(
			C.int(writeFD),
			(*C.uint32_t)(&cgoHandoffGate),
			C.uint32_t(i+1),
			(*C.uint64_t)(&elapsed),
		)
	}
	if atomic.LoadUint32(&cgoHandoffGate) != uint32(iterations) {
		return failedHandoff
	}
	return elapsed
}

// ResetDirectHandoff resets the coroutine direct handoff fixture.
func ResetDirectHandoff() {
	atomic.StoreUint32(&directHandoffGate, 0)
}

// ReleaseDirectHandoff releases the coroutine direct handoff for epoch.
func ReleaseDirectHandoff(epoch uint32) {
	atomic.StoreUint32(&directHandoffGate, epoch)
}

// DirectHandoffs measures the same blocking handoffs through the coroutine
// direct path. It returns the total time from the C write until the Go worker
// releases C. The package must be compiled with -d=coro=4.
func DirectHandoffs(iterations, writeFD int) uint64 {
	var elapsed uint64
	for i := 0; i < iterations; i++ {
		C.coro_direct_handoff(
			C.int(writeFD),
			(*C.uint32_t)(&directHandoffGate),
			C.uint32_t(i+1),
			(*C.uint64_t)(&elapsed),
		)
	}
	if atomic.LoadUint32(&directHandoffGate) != uint32(iterations) {
		return failedHandoff
	}
	return elapsed
}

var cgoRunnableEntered uint64
var cgoRunnableGate uint64
var cgoRunnableEpoch uint64
var directRunnableEntered uint64
var directRunnableGate uint64
var directRunnableEpoch uint64

func cgoRunnableHandoffWorker() {
	epoch := atomic.LoadUint64(&cgoRunnableEpoch)
	for atomic.LoadUint64(&cgoRunnableEntered) < epoch {
	}
	atomic.StoreUint64(&cgoRunnableGate, epoch)
}

func directRunnableHandoffWorker() {
	epoch := atomic.LoadUint64(&directRunnableEpoch)
	for atomic.LoadUint64(&directRunnableEntered) < epoch {
		runtime.Gosched()
	}
	atomic.StoreUint64(&directRunnableGate, epoch)
}

// CgoRunnableHandoffs measures blocking handoffs from ordinary cgo calls to
// a sibling Go goroutine. It returns the total time from each C publication
// until the sibling releases C.
func CgoRunnableHandoffs(iterations int) uint64 {
	if iterations < 0 {
		return failedHandoff
	}
	atomic.StoreUint64(&cgoRunnableEntered, 0)
	atomic.StoreUint64(&cgoRunnableGate, 0)
	var elapsed uint64
	for epoch := uint64(1); epoch <= uint64(iterations); epoch++ {
		atomic.StoreUint64(&cgoRunnableEpoch, epoch)
		go cgoRunnableHandoffWorker()
		C.coro_cgo_runnable_handoff(
			(*C.uint64_t)(&cgoRunnableEntered),
			C.uint64_t(epoch),
			(*C.uint64_t)(&cgoRunnableGate),
			(*C.uint64_t)(&elapsed),
		)
	}
	return runnableHandoffResult(
		iterations,
		atomic.LoadUint64(&cgoRunnableEntered),
		atomic.LoadUint64(&cgoRunnableGate),
		elapsed,
	)
}

// DirectRunnableHandoffs measures blocking handoffs from direct C calls to a
// sibling stackless goroutine. It returns the total time from each C
// publication until the sibling releases C. The package must be compiled with
// -d=coro=4.
func DirectRunnableHandoffs(iterations int) uint64 {
	if iterations < 0 {
		return failedHandoff
	}
	atomic.StoreUint64(&directRunnableEntered, 0)
	atomic.StoreUint64(&directRunnableGate, 0)
	var elapsed uint64
	for epoch := uint64(1); epoch <= uint64(iterations); epoch++ {
		atomic.StoreUint64(&directRunnableEpoch, epoch)
		go directRunnableHandoffWorker()
		C.coro_direct_runnable_handoff(
			(*C.uint64_t)(&directRunnableEntered),
			C.uint64_t(epoch),
			(*C.uint64_t)(&directRunnableGate),
			(*C.uint64_t)(&elapsed),
		)
	}
	return runnableHandoffResult(
		iterations,
		atomic.LoadUint64(&directRunnableEntered),
		atomic.LoadUint64(&directRunnableGate),
		elapsed,
	)
}

func runnableHandoffResult(iterations int, entered, gate, elapsed uint64) uint64 {
	if elapsed == failedHandoff ||
		entered != uint64(iterations) ||
		gate != uint64(iterations) {
		return failedHandoff
	}
	return elapsed
}

// NoBlockCalls measures the compiler-owned nonblocking call class. It is a
// reference for deciding whether a general C-boundary contract is worthwhile.
func NoBlockCalls(iterations int) uint64 {
	var value uint64
	for i := 0; i < iterations; i++ {
		value = corort.DirectAdd(value, 1)
	}
	return value
}

// NoBlockCall invokes one compiler-owned nonblocking foreign call.
func NoBlockCall() uint64 {
	value := corort.DirectAdd(0, 1)
	return value
}
