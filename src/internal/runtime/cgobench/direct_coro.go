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

void coro_cgo_empty(void);
void coro_direct_empty(void);
void coro_cgo_handoff(int, uint32_t *, uint32_t, uint64_t *);
void coro_direct_handoff(int, uint32_t *, uint32_t, uint64_t *);
*/
import "C"
import (
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
