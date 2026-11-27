// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

//go:linkname runtime_coroSemacquire
func runtime_coroSemacquire(ctx unsafe.Pointer, s *uint32, lifo bool) bool

const (
	coroMutexActive      uint64 = 1 << 0
	coroMutexStarving    uint64 = 1 << 1
	coroMutexAwoke       uint64 = 1 << 2
	coroMutexWaitingSema uint64 = 1 << 3
)

// CoroLock acquires m for a stackless coroutine without blocking the host
// driver goroutine. st is a 2-word scratch slot in the coroutine frame (zeroed
// before the first call). Returns true if the coroutine parked on m.sema and
// must return coroActionWait; returns false once m is locked.
func (m *Mutex) CoroLock(ctx unsafe.Pointer, st *[2]uint64) bool {
	flags := st[1]
	if flags == 0 {
		// Uncontended fast path: identical single CAS to (*Mutex).Lock.
		if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
			if race.Enabled {
				race.Acquire(unsafe.Pointer(m))
			}
			return false
		}
		flags = coroMutexActive
	}

	waitStartTime := int64(st[0])
	starving := flags&coroMutexStarving != 0
	awoke := flags&coroMutexAwoke != 0

	if flags&coroMutexWaitingSema != 0 {
		// Resuming from semaphore wait; re-check acquisition (handles spurious wake).
		queueLifo := waitStartTime != 0
		if runtime_coroSemacquire(ctx, &m.sema, queueLifo) {
			return true
		}
		flags &^= coroMutexWaitingSema
		starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
		old := atomic.LoadInt32(&m.state)
		if old&mutexStarving != 0 {
			if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
				throw("sync: inconsistent mutex state")
			}
			delta := int32(mutexLocked - 1<<mutexWaiterShift)
			if !starving || old>>mutexWaiterShift == 1 {
				delta -= mutexStarving
			}
			atomic.AddInt32(&m.state, delta)
			st[0] = 0
			st[1] = 0
			if race.Enabled {
				race.Acquire(unsafe.Pointer(m))
			}
			return false
		}
		awoke = true
	}

	old := atomic.LoadInt32(&m.state)
	for {
		new := old
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke {
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 {
				st[0] = 0
				st[1] = 0
				if race.Enabled {
					race.Acquire(unsafe.Pointer(m))
				}
				return false
			}
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			if runtime_coroSemacquire(ctx, &m.sema, queueLifo) {
				nf := coroMutexActive | coroMutexWaitingSema
				if starving {
					nf |= coroMutexStarving
				}
				st[0] = uint64(waitStartTime)
				st[1] = nf
				return true
			}
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = atomic.LoadInt32(&m.state)
			if old&mutexStarving != 0 {
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				st[0] = 0
				st[1] = 0
				if race.Enabled {
					race.Acquire(unsafe.Pointer(m))
				}
				return false
			}
			awoke = true
		} else {
			old = atomic.LoadInt32(&m.state)
		}
	}
}
