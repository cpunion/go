// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package sync

import (
	"internal/race"
	"internal/synctest"
	"unsafe"
)

func runtime_coroSemacquire(ctx unsafe.Pointer, s *uint32, lifo bool) bool
func runtime_coroNotifyListWait(ctx unsafe.Pointer, l *notifyList, t uint32) bool

// coroLock acquires m without blocking the host driver goroutine.
func (m *Mutex) coroLock(ctx unsafe.Pointer, st *[2]uint64) bool {
	return m.mu.CoroLock(ctx, st)
}

// coroRLock acquires rw for reading without blocking the host driver goroutine.
func (rw *RWMutex) coroRLock(ctx unsafe.Pointer, st *[2]uint64) bool {
	if st[1] == 0 {
		if race.Enabled {
			race.Read(unsafe.Pointer(&rw.w))
			race.Disable()
		}
		needWait := rw.readerCount.Add(1) < 0
		if needWait {
			st[1] = 1
			parked := runtime_coroSemacquire(ctx, &rw.readerSem, false)
			if race.Enabled {
				race.Enable()
			}
			if parked {
				return true
			}
			st[1] = 0
			if race.Enabled {
				race.Acquire(unsafe.Pointer(&rw.readerSem))
			}
			return false
		}
		if race.Enabled {
			race.Enable()
			race.Acquire(unsafe.Pointer(&rw.readerSem))
		}
		return false
	}
	if race.Enabled {
		race.Disable()
	}
	parked := runtime_coroSemacquire(ctx, &rw.readerSem, false)
	if race.Enabled {
		race.Enable()
	}
	if parked {
		return true
	}
	st[1] = 0
	if race.Enabled {
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
	return false
}

const coroRWPhaseWaitReaders uint64 = 1 << 8

// coroLock acquires rw for writing without blocking the host driver goroutine.
func (rw *RWMutex) coroLock(ctx unsafe.Pointer, st *[2]uint64) bool {
	if st[1]&coroRWPhaseWaitReaders == 0 {
		if st[1] == 0 && race.Enabled {
			race.Read(unsafe.Pointer(&rw.w))
		}
		if race.Enabled {
			race.Disable()
		}
		parked := rw.w.mu.CoroLock(ctx, st)
		if parked {
			if race.Enabled {
				race.Enable()
			}
			return true
		}
		r := rw.readerCount.Add(-rwmutexMaxReaders) + rwmutexMaxReaders
		if r != 0 && rw.readerWait.Add(r) != 0 {
			st[0] = 0
			st[1] = coroRWPhaseWaitReaders
			parked = runtime_coroSemacquire(ctx, &rw.writerSem, false)
			if race.Enabled {
				race.Enable()
			}
			if parked {
				return true
			}
		} else if race.Enabled {
			race.Enable()
		}
		st[0] = 0
		st[1] = 0
		if race.Enabled {
			race.Acquire(unsafe.Pointer(&rw.readerSem))
			race.Acquire(unsafe.Pointer(&rw.writerSem))
		}
		return false
	}
	if race.Enabled {
		race.Disable()
	}
	parked := runtime_coroSemacquire(ctx, &rw.writerSem, false)
	if race.Enabled {
		race.Enable()
	}
	if parked {
		return true
	}
	st[0] = 0
	st[1] = 0
	if race.Enabled {
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
	return false
}

// coroWait waits until wg's counter reaches zero without blocking the host driver.
func (wg *WaitGroup) coroWait(ctx unsafe.Pointer, st *[2]uint64) bool {
	if st[1] == 0 {
		if race.Enabled {
			race.Disable()
		}
		for {
			state := wg.state.Load()
			v := int32(state >> 32)
			w := uint32(state & 0x7fffffff)
			if v == 0 {
				if race.Enabled {
					race.Enable()
					race.Acquire(unsafe.Pointer(wg))
				}
				if w == 0 && state&waitGroupBubbleFlag != 0 && synctest.IsAssociated(wg) {
					if wg.state.CompareAndSwap(state, 0) {
						synctest.Disassociate(wg)
					}
				}
				return false
			}
			if wg.state.CompareAndSwap(state, state+1) {
				if race.Enabled && w == 0 {
					race.Write(unsafe.Pointer(&wg.sema))
				}
				st[1] = 1
				parked := runtime_coroSemacquire(ctx, &wg.sema, false)
				if race.Enabled {
					race.Enable()
				}
				if parked {
					return true
				}
				isReset := wg.state.Load() != 0
				st[1] = 0
				if race.Enabled {
					race.Acquire(unsafe.Pointer(wg))
				}
				if isReset {
					panic("sync: WaitGroup is reused before previous Wait has returned")
				}
				return false
			}
		}
	}
	if race.Enabled {
		race.Disable()
	}
	parked := runtime_coroSemacquire(ctx, &wg.sema, false)
	if race.Enabled {
		race.Enable()
	}
	if parked {
		return true
	}
	isReset := wg.state.Load() != 0
	st[1] = 0
	if race.Enabled {
		race.Acquire(unsafe.Pointer(wg))
	}
	if isReset {
		panic("sync: WaitGroup is reused before previous Wait has returned")
	}
	return false
}

const (
	coroCondPhaseShift = 16
	coroCondPhaseMask  = uint64(0xff) << coroCondPhaseShift
	coroCondWaiting    = uint64(1) << coroCondPhaseShift
	coroCondRelockM    = uint64(2) << coroCondPhaseShift
	coroCondRelockRW   = uint64(3) << coroCondPhaseShift
	coroCondRelockRL   = uint64(4) << coroCondPhaseShift
)

// coroWait unlocks c.L, parks on c.notify, and re-locks c.L without blocking
// the host driver goroutine (supporting stackless re-lock for *Mutex, *RWMutex,
// and (*RWMutex).RLocker()).
func (c *Cond) coroWait(ctx unsafe.Pointer, st *[2]uint64) bool {
	phase := st[1] & coroCondPhaseMask
	if phase == 0 {
		c.checker.check()
		t := runtime_notifyListAdd(&c.notify)
		c.L.Unlock()
		st[0] = uint64(t)
		st[1] = coroCondWaiting
		if runtime_coroNotifyListWait(ctx, &c.notify, t) {
			return true
		}
		phase = coroCondWaiting
	}
	if phase == coroCondWaiting {
		// Transition to re-locking c.L cleanly.
		st[0] = 0
		switch l := c.L.(type) {
		case *Mutex:
			st[1] = coroCondRelockM
			return c.coroRelockStep(ctx, l, nil, nil, st)
		case *RWMutex:
			st[1] = coroCondRelockRW
			return c.coroRelockStep(ctx, nil, l, nil, st)
		case *rlocker:
			st[1] = coroCondRelockRL
			return c.coroRelockStep(ctx, nil, nil, l, st)
		default:
			st[1] = 0
			c.L.Lock()
			return false
		}
	}
	switch phase {
	case coroCondRelockM:
		return c.coroRelockStep(ctx, c.L.(*Mutex), nil, nil, st)
	case coroCondRelockRW:
		return c.coroRelockStep(ctx, nil, c.L.(*RWMutex), nil, st)
	case coroCondRelockRL:
		return c.coroRelockStep(ctx, nil, nil, c.L.(*rlocker), st)
	}
	st[0] = 0
	st[1] = 0
	return false
}

func (c *Cond) coroRelockStep(ctx unsafe.Pointer, m *Mutex, rw *RWMutex, rl *rlocker, st *[2]uint64) bool {
	phase := st[1] & coroCondPhaseMask
	sub := [2]uint64{st[0], st[1] &^ coroCondPhaseMask}
	var parked bool
	switch {
	case m != nil:
		parked = m.coroLock(ctx, &sub)
	case rw != nil:
		parked = rw.coroLock(ctx, &sub)
	case rl != nil:
		parked = (*RWMutex)(rl).coroRLock(ctx, &sub)
	}
	if parked {
		st[0] = sub[0]
		st[1] = phase | sub[1]
		return true
	}
	st[0] = 0
	st[1] = 0
	return false
}
