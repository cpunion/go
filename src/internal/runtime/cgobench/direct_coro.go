// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package cgobench

/*
#cgo noescape coro_direct_empty
#cgo nocallback coro_direct_empty

void coro_cgo_empty(void);
void coro_direct_empty(void);
*/
import "C"
import corort "runtime/coro"

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
