// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/goexperiment"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// Keep the locked channel decision logic in this experiment-only file.
// Factoring it out of chansend and chanrecv would add a non-inlinable call to
// every ordinary channel operation when the experiment is disabled. Waiter
// matching and completion still use the common send and recv paths.

// newStacklessCoroSudog returns a waiter owned by op. A completed operation
// retains one cleared waiter with the operation cache. The retained pointer
// remains installed while the waiter is active so it is a stable GC root while
// a native executor initializes and queues it. Extra select waiters are rooted
// in slot before allocation can reach a GC safe point.
//
//go:nosplit
func newStacklessCoroSudog(op *stacklessCoroOperation, slot **channelWaiter) *channelWaiter {
	if op == nil {
		throw("runtime: nil stackless coroutine channel waiter owner")
	}
	if !op.waiterActive {
		op.waiterActive = true
		if sg := op.retainedWaiter(); sg != nil {
			if !validReleasedStacklessCoroSudog(op, sg) {
				throw("runtime: invalid cached stackless coroutine channel waiter")
			}
			if slot != nil {
				*slot = sg
			}
			return sg
		}

		// Match acquireSudog's allocation rule. A caller may hold an hchan
		// lock, so prevent new from starting a collection that needs a sudog
		// itself. Install both possible heap roots before releasing the M.
		mp := acquirem()
		sg := op.newRetainedWaiter()
		if slot != nil {
			*slot = sg
		}
		releasem(mp)
		return sg
	}
	if slot == nil {
		throw("runtime: duplicate stackless coroutine channel waiter")
	}

	// A select may need more than the one waiter retained by its operation.
	// Publish each additional waiter in the selection before releasing the M.
	mp := acquirem()
	sg := new(sudog).channelWaiter()
	*slot = sg
	releasem(mp)
	return sg
}

// releaseStacklessCoroSudog checks that the waiter no longer retains channel
// state. Each cached operation retains at most one waiter; extra select
// waiters become garbage after their selection descriptor is cleared.
func releaseStacklessCoroSudog(owner unsafe.Pointer, sg *channelWaiter) {
	op := (*stacklessCoroOperation)(owner)
	if op == nil || !validReleasedStacklessCoroSudog(op, sg) {
		throw("runtime: invalid released stackless coroutine channel waiter")
	}
	if isCompactStacklessCoroSudog(sg, owner) || op.retainedWaiter() == sg {
		if !op.waiterActive {
			throw("runtime: duplicate released stackless coroutine channel waiter")
		}
		op.waiterActive = false
	}
}

func validReleasedStacklessCoroSudog(op *stacklessCoroOperation, sg *channelWaiter) bool {
	if sg == nil || sg.coro.get() != nil || sg.next != nil || sg.prev != nil || sg.elem != nil || sg.c != nil {
		return false
	}
	// The explicit operation identifies compact storage even after owner clears.
	// In that case, inspect only the prefix; full address mirrors overlap op.
	if isCompactStacklessCoroSudog(sg, unsafe.Pointer(op)) {
		return op.waiterState&stacklessCoroWaiterSelect == 0
	}
	full := sg.fullSudog()
	return !full.isSelect && full.g == nil && full.waitlink == nil && full.elemAddr == 0 && full.chanAddr == 0
}

// tryStacklessCoroChanSend completes a ready send on the current executor.
func tryStacklessCoroChanSend(ctx unsafe.Pointer, channel *hchan,
	element unsafe.Pointer) bool {
	if raceenabled {
		return false
	}
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil ||
		stacklessCoroIsPullComparison(context.scheduler) {
		return false
	}
	if channel != nil && channel.dataqsiz != 0 {
		return coroEntryChanSend(channel, element)
	}
	return chansend(channel, element, false, sys.GetCallerPC())
}

// tryStacklessCoroChanRecv completes a ready receive on the current executor.
func tryStacklessCoroChanRecv(ctx unsafe.Pointer, channel *hchan,
	element unsafe.Pointer, received *bool) bool {
	if raceenabled {
		return false
	}
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil ||
		stacklessCoroIsPullComparison(context.scheduler) {
		return false
	}
	var selected, success bool
	if channel != nil && channel.dataqsiz != 0 {
		selected, success = coroEntryChanRecv(element, channel)
	} else {
		selected, success = chanrecv(channel, element, false)
	}
	if !selected {
		return false
	}
	if received != nil {
		*received = success
	}
	return true
}

// Public entries and native executors share the non-blocking buffered path.
// A full or empty open buffer can report a miss before acquiring the lock,
// using the same observations as the ordinary channel implementation.
// While holding the lock the GC write barrier phase cannot change. Keep the
// typed operations when the barrier or cgo pointer checking is enabled.
func coroEntryChanSend(c *hchan, element unsafe.Pointer) bool {
	if !raceenabled && !debugChan && c != nil && c.dataqsiz != 0 && c.bubble == nil {
		if c.closed == 0 && c.qcount == c.dataqsiz {
			return false
		}
		lock(&c.lock)
		if c.closed == 0 && c.recvq.first == nil {
			if c.qcount < c.dataqsiz {
				if c.elemsize != 0 {
					slot := chanbuf(c, c.sendx)
					if writeBarrier.enabled || goexperiment.CgoCheck2 {
						typedmemmove(c.elemtype, slot, element)
					} else {
						memmove(slot, element, uintptr(c.elemsize))
					}
				}
				advanceStacklessCoroSend(c)
				unlock(&c.lock)
				return true
			}
			unlock(&c.lock)
			return false
		}
		if c.closed == 0 {
			return coroBufferedSendPeer(c, element, sys.GetCallerPC())
		}
		unlock(&c.lock)
	}
	return chansend(c, element, false, sys.GetCallerPC())
}

func coroEntryChanRecv(element unsafe.Pointer, c *hchan) (bool, bool) {
	if !raceenabled && !debugChan && c != nil && c.dataqsiz != 0 && c.bubble == nil && c.timer == nil {
		if atomic.Loaduint(&c.qcount) == 0 && atomic.Load(&c.closed) == 0 {
			return false, false
		}
		lock(&c.lock)
		if c.sendq.first == nil && (c.qcount != 0 || c.closed == 0) {
			if c.qcount != 0 {
				if c.elemsize != 0 {
					slot := chanbuf(c, c.recvx)
					if writeBarrier.enabled || goexperiment.CgoCheck2 {
						if element != nil {
							typedmemmove(c.elemtype, element, slot)
						}
						typedmemclr(c.elemtype, slot)
					} else {
						if element != nil {
							memmove(element, slot, uintptr(c.elemsize))
						}
						memclrNoHeapPointers(slot, uintptr(c.elemsize))
					}
				}
				advanceStacklessCoroRecv(c)
				unlock(&c.lock)
				return true, true
			}
			unlock(&c.lock)
			return false, false
		}
		if c.closed == 0 {
			return coroBufferedRecvPeer(c, element)
		}
		unlock(&c.lock)
	}
	return chanrecv(c, element, false)
}

// The caller holds the channel lock and has observed a real or stale waiter.
func coroBufferedSendPeer(c *hchan, element unsafe.Pointer, callerPC uintptr) bool {
	if sg := c.recvq.dequeue(); sg != nil {
		send(c, sg, element, func() { unlock(&c.lock) }, 3)
		return true
	}
	unlock(&c.lock)
	return chansend(c, element, false, callerPC)
}

func coroBufferedRecvPeer(c *hchan, element unsafe.Pointer) (bool, bool) {
	if sg := c.sendq.dequeue(); sg != nil {
		recv(c, sg, element, func() { unlock(&c.lock) }, 3)
		return true, true
	}
	unlock(&c.lock)
	return chanrecv(c, element, false)
}

// Callers move the slot's contents and advance its index while holding c.lock.
func advanceStacklessCoroSend(c *hchan) {
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
}

func advanceStacklessCoroRecv(c *hchan) {
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
}

// chansendStackless starts op without parking the executor goroutine.
func chansendStackless(op *stacklessCoroOperation, c *hchan, element unsafe.Pointer) {
	if c == nil {
		return
	}

	if raceenabled {
		racereadpc(c.raceaddr(), sys.GetCallerPC(),
			abi.FuncPCABIInternal(chansendStackless))
	}
	if c.bubble != nil && getg().bubble != c.bubble {
		fatal("send on synctest channel from outside bubble")
	}

	lock(&c.lock)
	if c.closed != 0 {
		unlock(&c.lock)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, false)
		return
	}
	if sg := c.recvq.dequeue(); sg != nil {
		send(c, sg, element, func() { unlock(&c.lock) }, 3)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}
	if c.qcount < c.dataqsiz {
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		typedmemmove(c.elemtype, qp, element)
		advanceStacklessCoroSend(c)
		unlock(&c.lock)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}

	sg := newStacklessCoroSudog(op, nil)
	sg.coro.set(unsafe.Pointer(op))
	sg.setNativeElem(element)
	// Acquisition validates a cleared waiter; only select sets its select bit.
	sg.setNativeChannel(c)
	c.sendq.enqueue(sg)
	if raceenabled {
		racerelease(unsafe.Pointer(op))
	}
	unlock(&c.lock)
}

// chanrecvStackless starts op without parking the executor goroutine.
func chanrecvStackless(op *stacklessCoroOperation, c *hchan, element unsafe.Pointer) {
	if c == nil {
		return
	}

	if c.bubble != nil && getg().bubble != c.bubble {
		fatal("receive on synctest channel from outside bubble")
	}
	if c.timer != nil {
		c.timer.maybeRunChan(c)
	}

	lock(&c.lock)
	if c.closed != 0 {
		if c.qcount == 0 {
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			unlock(&c.lock)
			if element != nil {
				typedmemclr(c.elemtype, element)
			}
			finishStacklessCoroChannel(unsafe.Pointer(op), nil, false)
			return
		}
	} else if sg := c.sendq.dequeue(); sg != nil {
		recv(c, sg, element, func() { unlock(&c.lock) }, 3)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}
	if c.qcount > 0 {
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		if element != nil {
			typedmemmove(c.elemtype, element, qp)
		}
		typedmemclr(c.elemtype, qp)
		advanceStacklessCoroRecv(c)
		unlock(&c.lock)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}

	sg := newStacklessCoroSudog(op, nil)
	sg.coro.set(unsafe.Pointer(op))
	sg.setNativeElem(element)
	sg.setNativeChannel(c)
	c.recvq.enqueue(sg)
	if c.timer != nil {
		blockTimerChan(c)
		op.timerWait = true
	}
	if raceenabled {
		racerelease(unsafe.Pointer(op))
	}
	unlock(&c.lock)
}
