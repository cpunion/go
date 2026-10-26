// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && goexperiment.greenteagc && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

func stacklessCoroLockRankProbeResume(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }

// Check the actual pool/registry mutexes and the scheduler's production
// initializer. A write-barrier flush can acquire either GC queue lock below.
// The probe takes those real locks without depending on stochastic GC timing.
func StacklessCoroWriteBarrierLockForTest(kind int, spans bool) {
	var m *mutex
	switch kind {
	case 0:
		s := new(stacklessCoroScheduler)
		s.wake = make(chan struct{}, stacklessCoroWarmExecutorCount)
		initializeStacklessCoroScheduler(s, nil, stacklessCoroLockRankProbeResume, 0)
		m = &s.lock
	case 1:
		m = &stacklessCoroOperations.lock
	case 2:
		m = &stacklessCoroSharedOperations.lock
	case 3:
		m = &stacklessCoroReadPool.lock
	case 4:
		m = &stacklessCoroNativePool.lock
	default:
		throw("runtime: invalid coroutine lock-rank test kind")
	}
	lock(m)
	if spans {
		systemstack(func() { lock(&work.spanSPMCs.lock); unlock(&work.spanSPMCs.lock) })
	} else {
		systemstack(func() { lock(&work.wbufSpans.lock); unlock(&work.wbufSpans.lock) })
	}
	unlock(m)
}
