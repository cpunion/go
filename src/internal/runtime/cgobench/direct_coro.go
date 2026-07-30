// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package cgobench

/*
#include <stdint.h>

#cgo noescape coro_direct_empty
#cgo nocallback coro_direct_empty
#cgo noescape coro_direct_handoff
#cgo nocallback coro_direct_handoff
#cgo noescape coro_direct_stackless_handoff
#cgo nocallback coro_direct_stackless_handoff

void coro_cgo_empty(void);
void coro_direct_empty(void);
void coro_cgo_handoff(int, uint32_t *, uint32_t, uint64_t *);
void coro_direct_handoff(int, uint32_t *, uint32_t, uint64_t *);
void coro_cgo_stackless_handoff(uint64_t *, uint64_t, uint64_t *, uint64_t *);
void coro_direct_stackless_handoff(uint64_t *, uint64_t, uint64_t *, uint64_t *);
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

var cgoStacklessEntered uint64
var cgoStacklessGate uint64
var directStacklessEntered uint64
var directStacklessGate uint64

func stacklessHandoffWorker(iterations int, entered, gate *uint64) {
	for epoch := uint64(1); epoch <= uint64(iterations); epoch++ {
		for atomic.LoadUint64(entered) < epoch {
			runtime.Gosched()
		}
		atomic.StoreUint64(gate, epoch)
	}
}

// CgoStacklessHandoffs measures blocking handoffs from ordinary cgo calls to
// a sibling stackless goroutine. It returns the total time from each C
// publication until the sibling releases C.
func CgoStacklessHandoffs(iterations int) uint64 {
	if iterations < 0 {
		return failedHandoff
	}
	atomic.StoreUint64(&cgoStacklessEntered, 0)
	atomic.StoreUint64(&cgoStacklessGate, 0)
	go stacklessHandoffWorker(
		iterations, &cgoStacklessEntered, &cgoStacklessGate)
	runtime.Gosched()
	var elapsed uint64
	for epoch := uint64(1); epoch <= uint64(iterations); epoch++ {
		C.coro_cgo_stackless_handoff(
			(*C.uint64_t)(&cgoStacklessEntered),
			C.uint64_t(epoch),
			(*C.uint64_t)(&cgoStacklessGate),
			(*C.uint64_t)(&elapsed),
		)
	}
	return stacklessHandoffResult(
		iterations,
		atomic.LoadUint64(&cgoStacklessEntered),
		atomic.LoadUint64(&cgoStacklessGate),
		elapsed,
	)
}

// DirectStacklessHandoffs measures blocking handoffs from direct C calls to a
// sibling stackless goroutine. It returns the total time from each C
// publication until the sibling releases C. The package must be compiled with
// -d=coro=4.
func DirectStacklessHandoffs(iterations int) uint64 {
	if iterations < 0 {
		return failedHandoff
	}
	atomic.StoreUint64(&directStacklessEntered, 0)
	atomic.StoreUint64(&directStacklessGate, 0)
	go stacklessHandoffWorker(
		iterations, &directStacklessEntered, &directStacklessGate)
	runtime.Gosched()
	var elapsed uint64
	for epoch := uint64(1); epoch <= uint64(iterations); epoch++ {
		C.coro_direct_stackless_handoff(
			(*C.uint64_t)(&directStacklessEntered),
			C.uint64_t(epoch),
			(*C.uint64_t)(&directStacklessGate),
			(*C.uint64_t)(&elapsed),
		)
	}
	return stacklessHandoffResult(
		iterations,
		atomic.LoadUint64(&directStacklessEntered),
		atomic.LoadUint64(&directStacklessGate),
		elapsed,
	)
}

func stacklessHandoffResult(iterations int, entered, gate, elapsed uint64) uint64 {
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
