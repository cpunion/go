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

// newStacklessCoroSudog allocates a waiter owned by its channel operation.
// Unlike a parked goroutine, a logical task has no owner that can return the
// waiter to a per-P cache after wakeup. The object becomes garbage after the
// waker removes it from the channel queue.
//
//go:nosplit
func newStacklessCoroSudog() *sudog {
	// Match acquireSudog's allocation rule. A caller may hold an hchan lock,
	// so prevent new from starting a collection that needs a sudog itself.
	mp := acquirem()
	sg := new(sudog)
	releasem(mp)
	return sg
}

// releaseStacklessCoroSudog checks that the waiter no longer retains channel
// state. Its storage is reclaimed by the garbage collector.
func releaseStacklessCoroSudog(sg *sudog) {
	if sg.g != nil || sg.elem.get() != nil || sg.coro.get() != nil ||
		sg.next != nil || sg.prev != nil || sg.waitlink != nil ||
		sg.c.get() != nil || sg.isSelect {
		throw("runtime: invalid released stackless coroutine channel waiter")
	}
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

	sg := newStacklessCoroSudog()
	sg.releasetime = 0
	sg.elem.set(op.element)
	sg.waitlink = nil
	sg.g = nil
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

	sg := newStacklessCoroSudog()
	sg.releasetime = 0
	sg.elem.set(op.element)
	sg.waitlink = nil
	sg.g = nil
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
