// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

/*
#cgo LDFLAGS: -lm

#cgo noescape probe_scalar
#cgo nocallback probe_scalar
#cgo noescape probe_pair_add
#cgo nocallback probe_pair_add
#cgo noescape probe_errno
#cgo nocallback probe_errno
#cgo noescape probe_sin
#cgo nocallback probe_sin
#cgo noescape probe_block
#cgo nocallback probe_block

#include <stdint.h>

typedef struct {
	uint64_t integer;
	double floating;
} probe_pair;

uint64_t probe_scalar(uint64_t);
probe_pair probe_pair_add(probe_pair, probe_pair);
int64_t probe_errno(int64_t);
double probe_sin(double);
uint64_t probe_block(uint64_t, uint64_t *, uint64_t *);
*/
import "C"

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

func cScalarCalls(iterations int) uint64 {
	value := C.uint64_t(1)
	for i := 0; i < iterations; i++ {
		value = C.probe_scalar(value)
	}
	return uint64(value)
}

func cPairCalls(iterations int) (uint64, float64) {
	value := C.probe_pair{integer: 1, floating: 0.5}
	step := C.probe_pair{integer: 2, floating: 0.25}
	for i := 0; i < iterations; i++ {
		value = C.probe_pair_add(value, step)
	}
	return uint64(value.integer), float64(value.floating)
}

func cErrnoCalls(iterations int) (int64, error) {
	value := C.int64_t(1)
	for i := 0; i < iterations; i++ {
		var err error
		value, err = C.probe_errno(value)
		if err != nil {
			return int64(value), err
		}
	}
	return int64(value), nil
}

func cLibmCalls(iterations int) float64 {
	value := C.double(0.5)
	for i := 0; i < iterations; i++ {
		value = C.probe_sin(value)
	}
	return float64(value)
}

var (
	handoffEpoch   uint64
	handoffEntered uint64
	handoffGate    uint64
)

func handoffWorker() {
	epoch := atomic.LoadUint64(&handoffEpoch)
	for atomic.LoadUint64(&handoffEntered) < epoch {
		runtime.Gosched()
	}
	atomic.StoreUint64(&handoffGate, epoch)
}

func cBlockingHandoffs(iterations int) uint64 {
	if iterations <= 0 {
		return 0
	}
	atomic.StoreUint64(&handoffEntered, 0)
	atomic.StoreUint64(&handoffGate, 0)
	var elapsed uint64
	for epoch := uint64(1); epoch <= uint64(iterations); epoch++ {
		atomic.StoreUint64(&handoffEpoch, epoch)
		go handoffWorker()
		sample := C.probe_block(
			C.uint64_t(epoch),
			(*C.uint64_t)(unsafe.Pointer(&handoffEntered)),
			(*C.uint64_t)(unsafe.Pointer(&handoffGate)),
		)
		elapsed += uint64(sample)
	}
	return elapsed
}
