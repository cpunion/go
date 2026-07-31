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
#cgo noescape probe_block_group
#cgo nocallback probe_block_group

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
uint64_t probe_block_group(uint64_t, uint64_t *, uint64_t *, uint64_t);
*/
import "C"

import (
	"runtime"
	"sync/atomic"
	"time"
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

type cBlockingGroupState struct {
	entered  uint64
	release  uint64
	done     uint64
	timeouts uint64
	epoch    uint64
	timeout  uint64
}

var activeCBlockingGroup *cBlockingGroupState

func cBlockingGroupWorker() {
	state := activeCBlockingGroup
	released := C.probe_block_group(
		C.uint64_t(state.epoch),
		(*C.uint64_t)(unsafe.Pointer(&state.entered)),
		(*C.uint64_t)(unsafe.Pointer(&state.release)),
		C.uint64_t(state.timeout),
	)
	if released == 0 {
		atomic.AddUint64(&state.timeouts, 1)
	}
	atomic.AddUint64(&state.done, 1)
}

func cBlockingGroup(rounds, calls int, timeout time.Duration) (entryElapsed, timeouts uint64) {
	if rounds <= 0 || calls <= 0 || timeout <= 0 {
		return 0, 0
	}
	state := new(cBlockingGroupState)
	activeCBlockingGroup = state
	for round := 0; round < rounds; round++ {
		epoch := uint64(round + 1)
		atomic.StoreUint64(&state.entered, 0)
		atomic.StoreUint64(&state.release, epoch-1)
		atomic.StoreUint64(&state.done, 0)
		atomic.StoreUint64(&state.timeouts, 0)
		state.epoch = epoch
		state.timeout = uint64(timeout)

		start := time.Now()
		for call := 0; call < calls; call++ {
			go cBlockingGroupWorker()
		}
		for atomic.LoadUint64(&state.entered) < uint64(calls) &&
			atomic.LoadUint64(&state.done) == 0 {
			runtime.Gosched()
		}
		entryElapsed += uint64(time.Since(start))
		atomic.StoreUint64(&state.release, epoch)
		for atomic.LoadUint64(&state.done) < uint64(calls) {
			runtime.Gosched()
		}
		timeouts += atomic.LoadUint64(&state.timeouts)
	}
	return entryElapsed, timeouts
}
