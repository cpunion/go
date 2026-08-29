// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
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
func newStacklessCoroSudog(op *stacklessCoroOperation, slot **sudog) *sudog {
	if op == nil {
		throw("runtime: nil stackless coroutine channel waiter owner")
	}
	if !op.waiterActive {
		op.waiterActive = true
		if sg := op.waiter; sg != nil {
			if !validReleasedStacklessCoroSudog(sg) {
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
		sg := new(sudog)
		op.waiter = sg
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
	sg := new(sudog)
	*slot = sg
	releasem(mp)
	return sg
}

// releaseStacklessCoroSudog checks that the waiter no longer retains channel
// state. Each cached operation retains at most one waiter; extra select
// waiters become garbage after their selection descriptor is cleared.
func releaseStacklessCoroSudog(owner unsafe.Pointer, sg *sudog) {
	op := (*stacklessCoroOperation)(owner)
	if op == nil || !validReleasedStacklessCoroSudog(sg) {
		throw("runtime: invalid released stackless coroutine channel waiter")
	}
	if op.waiter == sg {
		if !op.waiterActive {
			throw("runtime: duplicate released stackless coroutine channel waiter")
		}
		op.waiterActive = false
	}
}

func validReleasedStacklessCoroSudog(sg *sudog) bool {
	return sg != nil && sg.g == nil && sg.elem.get() == nil &&
		sg.coro.get() == nil && sg.next == nil && sg.prev == nil &&
		sg.waitlink == nil && sg.c.get() == nil && !sg.isSelect
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
	selected, success := chanrecv(channel, element, false)
	if !selected {
		return false
	}
	if received != nil {
		*received = success
	}
	return true
}

// chansendStackless starts op without parking the executor goroutine.
func chansendStackless(op *stacklessCoroOperation) {
	c := op.channel
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
		send(c, sg, op.element, func() { unlock(&c.lock) }, 3)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}
	if c.qcount < c.dataqsiz {
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		typedmemmove(c.elemtype, qp, op.element)
		c.sendx++
		if c.sendx == c.dataqsiz {
			c.sendx = 0
		}
		c.qcount++
		unlock(&c.lock)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}

	sg := newStacklessCoroSudog(op, nil)
	sg.releasetime = 0
	sg.elem.set(op.element)
	sg.waitlink = nil
	sg.coro.set(unsafe.Pointer(op))
	sg.isSelect = false
	sg.c.set(c)
	c.sendq.enqueue(sg)
	if raceenabled {
		racerelease(unsafe.Pointer(op))
	}
	unlock(&c.lock)
}

// chanrecvStackless starts op without parking the executor goroutine.
func chanrecvStackless(op *stacklessCoroOperation) {
	c := op.channel
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
			if op.element != nil {
				typedmemclr(c.elemtype, op.element)
			}
			finishStacklessCoroChannel(unsafe.Pointer(op), nil, false)
			return
		}
	} else if sg := c.sendq.dequeue(); sg != nil {
		recv(c, sg, op.element, func() { unlock(&c.lock) }, 3)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}
	if c.qcount > 0 {
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		if op.element != nil {
			typedmemmove(c.elemtype, op.element, qp)
		}
		typedmemclr(c.elemtype, qp)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.qcount--
		unlock(&c.lock)
		finishStacklessCoroChannel(unsafe.Pointer(op), nil, true)
		return
	}

	sg := newStacklessCoroSudog(op, nil)
	sg.releasetime = 0
	sg.elem.set(op.element)
	sg.waitlink = nil
	sg.coro.set(unsafe.Pointer(op))
	sg.isSelect = false
	sg.c.set(c)
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
