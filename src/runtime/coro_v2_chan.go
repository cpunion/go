// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// coroChanSend performs a channel send for a stackless coroutine task.
// Returns false if the send completed immediately; returns true if the task
// parked on c.sendq and its state machine must return coroActionWait.
func coroChanSend(ctx unsafe.Pointer, c *hchan, elem unsafe.Pointer) bool {
	task := (*coroTask)(ctx)
	if c == nil {
		// Blocking forever on a nil channel: park permanently.
		task.state.Store(coroStateWaiting)
		return true
	}
	if raceenabled {
		racereadpc(c.raceaddr(), sys.GetCallerPC(), abi.FuncPCABIInternal(coroChanSend))
	}
	// Fast check for full unclosed channel? Always lock for blocking send unless immediate.
	lock(&c.lock)
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}
	if sg := c.recvq.dequeue(); sg != nil {
		send(c, sg, elem, func() { unlock(&c.lock) }, 3)
		return false
	}
	if c.qcount < c.dataqsiz {
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		typedmemmove(c.elemtype, qp, elem)
		c.sendx++
		if c.sendx == c.dataqsiz {
			c.sendx = 0
		}
		c.qcount++
		unlock(&c.lock)
		return false
	}
	// Must park on c.sendq.
	sg := acquireSudog()
	sg.g = nil
	sg.coro = task
	sg.elem.set(elem)
	sg.c.set(c)
	sg.isSelect = false
	sg.waitlink = nil
	sg.releasetime = 0
	task.flags |= coroFlagChanSend
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racerelease(unsafe.Pointer(task))
	}
	c.sendq.enqueue(sg)
	unlock(&c.lock)
	return true
}

// coroChanRecv performs a channel receive for a stackless coroutine task.
// Returns false if the receive completed immediately (writing *elem and *ok);
// returns true if the task parked on c.recvq and must return coroActionWait.
func coroChanRecv(ctx unsafe.Pointer, c *hchan, elem unsafe.Pointer, ok *bool) bool {
	task := (*coroTask)(ctx)
	if c == nil {
		task.state.Store(coroStateWaiting)
		return true
	}
	if raceenabled {
		racereadpc(c.raceaddr(), sys.GetCallerPC(), abi.FuncPCABIInternal(coroChanRecv))
	}
	lock(&c.lock)
	if c.closed != 0 {
		if c.qcount == 0 {
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			unlock(&c.lock)
			if elem != nil {
				typedmemclr(c.elemtype, elem)
			}
			if ok != nil {
				*ok = false
			}
			return false
		}
	} else {
		if sg := c.sendq.dequeue(); sg != nil {
			recv(c, sg, elem, func() { unlock(&c.lock) }, 3)
			if ok != nil {
				*ok = true
			}
			return false
		}
	}
	if c.qcount > 0 {
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		if elem != nil {
			typedmemmove(c.elemtype, elem, qp)
		}
		typedmemclr(c.elemtype, qp)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.qcount--
		unlock(&c.lock)
		if ok != nil {
			*ok = true
		}
		return false
	}
	// Must park on c.recvq.
	sg := acquireSudog()
	sg.g = nil
	sg.coro = task
	sg.elem.set(elem)
	sg.c.set(c)
	sg.isSelect = false
	sg.waitlink = nil
	sg.releasetime = 0
	// Stash ok pointer in ioState only when caller asks for comma-ok; otherwise
	// single-value recv needs zero extra allocation.
	if ok != nil {
		io := task.ensureIOState()
		io.chanOK = ok
	}
	task.flags &^= coroFlagChanSend
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racerelease(unsafe.Pointer(task))
	}
	c.recvq.enqueue(sg)
	if c.timer != nil {
		blockTimerChan(c)
	}
	unlock(&c.lock)
	return true
}

// coroChanWake is called from chan.go send()/recv() after unlocking c.lock.
func coroChanWake(sg *sudog, ok bool) {
	task := sg.coro
	if task == nil {
		return
	}
	if raceenabled {
		raceacquire(unsafe.Pointer(task))
	}
	c := sg.c.get()
	if c != nil && c.timer != nil && !sg.isSelect {
		unblockTimerChan(c)
	}
	if sg.isSelect {
		// Record winning case & winner sudog; defer full select unlock/release to coroSelectFinish.
		io := task.ioState
		io.selectWinner = sg
		io.selectCase = int(sg.ticket)
		io.selectOK = ok
		task.flags |= coroFlagSelectPending
		coroReady(task)
		return
	}
	sendPanic := task.flags&coroFlagChanSend != 0 && !ok
	task.flags &^= coroFlagChanSend
	if !sendPanic && task.ioState != nil && task.ioState.chanOK != nil {
		*task.ioState.chanOK = ok
		task.ioState.chanOK = nil
	}
	if sendPanic {
		coroPanic(unsafe.Pointer(task), plainError("send on closed channel"))
	}
	// Clear and release sg back to Go's per-P sudogcache.
	sg.coro = nil
	sg.g = nil
	sg.elem.set(nil)
	sg.c.set(nil)
	sg.next = nil
	sg.prev = nil
	sg.waitlink = nil
	sg.isSelect = false
	releaseSudog(sg)
	coroReady(task)
}

// coroChanCloseWake wakes a linked list of coroutine sudogs dequeued by closechan.
func coroChanCloseWake(head *sudog) {
	var readyHead, readyTail *coroTask
	var readyN int32
	for sg := head; sg != nil; {
		next := sg.next
		sg.next = nil
		sg.prev = nil
		task := sg.coro
		if task == nil {
			sg = next
			continue
		}
		if raceenabled {
			raceacquire(unsafe.Pointer(task))
		}
		if sg.isSelect {
			io := task.ioState
			io.selectWinner = sg
			io.selectCase = int(sg.ticket)
			io.selectOK = false
			task.flags |= coroFlagSelectPending
			coroReady(task)
			sg = next
			continue
		}
		c := sg.c.get()
		if c != nil && c.timer != nil {
			unblockTimerChan(c)
		}
		if task.flags&coroFlagChanSend != 0 {
			task.flags &^= coroFlagChanSend
			coroPanic(unsafe.Pointer(task), plainError("send on closed channel"))
		} else if task.ioState != nil && task.ioState.chanOK != nil {
			*task.ioState.chanOK = false
			task.ioState.chanOK = nil
		}
		sg.coro = nil
		sg.g = nil
		sg.elem.set(nil)
		sg.c.set(nil)
		sg.waitlink = nil
		sg.isSelect = false
		releaseSudog(sg)

		// Fast path: batch-collect already-Parked tasks for a single coroEnqueueBatch!
		if task.state.CompareAndSwap(coroStateParked, coroStateRunnable) {
			task.next = nil
			if readyTail == nil {
				readyHead = task
			} else {
				readyTail.next = task
			}
			readyTail = task
			readyN++
		} else {
			coroReady(task)
		}
		sg = next
	}
	if readyHead != nil {
		coroEnqueueBatch(readyHead, readyTail, readyN)
	}
}

// coroSelect performs a multi-case Go select for a stackless coroutine task.
// Returns false if a case (or default) completed immediately (writing *chosen
// and *ok); returns true if the task parked on channels and must return ActionWait.
func coroSelect(ctx unsafe.Pointer, cas0 *scase, nsends, nrecvs int, block bool, chosen *int, ok *bool) bool {
	task := (*coroTask)(ctx)
	ncases := nsends + nrecvs
	if ncases == 0 {
		if !block {
			*chosen = -1
			if ok != nil {
				*ok = false
			}
			return false
		}
		// select{} blocks forever.
		task.state.Store(coroStateWaiting)
		return true
	}
	scases := unsafe.Slice(cas0, ncases)
	pollorder := make([]uint16, 0, ncases)
	lockorder := make([]uint16, ncases)
	norder := 0
	for i := range scases {
		cas := &scases[i]
		if cas.c == nil {
			cas.elem = nil
			continue
		}
		if cas.c.timer != nil {
			cas.c.timer.maybeRunChan(cas.c)
		}
		j := cheaprandn(uint32(norder + 1))
		pollorder = append(pollorder, 0)
		pollorder[norder] = pollorder[j]
		pollorder[j] = uint16(i)
		norder++
	}
	// Build lockorder heap over non-nil channels only (matching select.go).
	lockorder = lockorder[:norder]
	for i := range lockorder {
		j := i
		c := scases[pollorder[i]].c
		for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
			k := (j - 1) / 2
			lockorder[j] = lockorder[k]
			j = k
		}
		lockorder[j] = pollorder[i]
	}
	for i := len(lockorder) - 1; i >= 0; i-- {
		o := lockorder[i]
		c := scases[o].c
		lockorder[i] = lockorder[0]
		j := 0
		for {
			k := j*2 + 1
			if k >= i {
				break
			}
			if k+1 < i && scases[lockorder[k]].c.sortkey() < scases[lockorder[k+1]].c.sortkey() {
				k++
			}
			if c.sortkey() < scases[lockorder[k]].c.sortkey() {
				lockorder[j] = lockorder[k]
				j = k
				continue
			}
			break
		}
		lockorder[j] = o
	}

	sellock(scases, lockorder)

	// Pass 1: look for immediately ready case in random pollorder.
	for _, casei := range pollorder {
		casi := int(casei)
		cas := &scases[casi]
		c := cas.c
		if casi >= nsends {
			// recv case
			if sg := c.sendq.dequeue(); sg != nil {
				recv(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 3)
				*chosen = casi
				if ok != nil {
					*ok = true
				}
				return false
			}
			if c.qcount > 0 {
				qp := chanbuf(c, c.recvx)
				if raceenabled {
					racenotify(c, c.recvx, nil)
				}
				if cas.elem != nil {
					typedmemmove(c.elemtype, cas.elem, qp)
				}
				typedmemclr(c.elemtype, qp)
				c.recvx++
				if c.recvx == c.dataqsiz {
					c.recvx = 0
				}
				c.qcount--
				selunlock(scases, lockorder)
				*chosen = casi
				if ok != nil {
					*ok = true
				}
				return false
			}
			if c.closed != 0 {
				if raceenabled {
					raceacquire(c.raceaddr())
				}
				selunlock(scases, lockorder)
				if cas.elem != nil {
					typedmemclr(c.elemtype, cas.elem)
				}
				*chosen = casi
				if ok != nil {
					*ok = false
				}
				return false
			}
		} else {
			// send case
			if c.closed != 0 {
				selunlock(scases, lockorder)
				panic(plainError("send on closed channel"))
			}
			if sg := c.recvq.dequeue(); sg != nil {
				send(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 3)
				*chosen = casi
				if ok != nil {
					*ok = false
				}
				return false
			}
			if c.qcount < c.dataqsiz {
				qp := chanbuf(c, c.sendx)
				if raceenabled {
					racenotify(c, c.sendx, nil)
				}
				typedmemmove(c.elemtype, qp, cas.elem)
				c.sendx++
				if c.sendx == c.dataqsiz {
					c.sendx = 0
				}
				c.qcount++
				selunlock(scases, lockorder)
				*chosen = casi
				if ok != nil {
					*ok = false
				}
				return false
			}
		}
	}

	if !block {
		selunlock(scases, lockorder)
		*chosen = -1
		if ok != nil {
			*ok = false
		}
		return false
	}

	// Pass 2: enqueue on all non-nil channels.
	io := task.ensureIOState()
	io.selectCases = make([]scase, ncases)
	copy(io.selectCases, scases)
	io.selectLockOrder = lockorder
	io.selectNSends = nsends
	io.selectChosen = chosen
	io.selectOKOut = ok
	io.selectWinner = nil
	task.selectDone.Store(0)
	task.state.Store(coroStateWaiting)
	if raceenabled {
		racerelease(unsafe.Pointer(task))
	}

	var sglist *sudog
	nextp := &sglist
	for _, casei := range lockorder {
		casi := int(casei)
		cas := &io.selectCases[casi]
		c := cas.c
		if c == nil {
			continue
		}
		sg := acquireSudog()
		sg.g = nil
		sg.coro = task
		sg.isSelect = true
		sg.ticket = uint32(casi)
		sg.elem.set(cas.elem)
		sg.c.set(c)
		sg.waitlink = nil
		*nextp = sg
		nextp = &sg.waitlink
		if casi < nsends {
			c.sendq.enqueue(sg)
		} else {
			c.recvq.enqueue(sg)
		}
		if c.timer != nil {
			blockTimerChan(c)
		}
	}
	io.selectSGList = sglist
	selunlock(io.selectCases, lockorder)
	return true
}

// coroSelectFinish runs on the active driver right before resuming task after a
// multi-case select completes. Returns true if the winning send case hit a closed channel.
func coroSelectFinish(task *coroTask) bool {
	task.flags &^= coroFlagSelectPending
	io := task.ioState
	scases := io.selectCases
	lockorder := io.selectLockOrder
	winner := io.selectWinner
	chosenIdx := io.selectCase
	recvOK := io.selectOK
	nsends := io.selectNSends

	sellock(scases, lockorder)
	sglist := io.selectSGList
	for _, casei := range lockorder {
		k := &scases[casei]
		if k.c == nil {
			continue
		}
		sg := sglist
		sglist = sg.waitlink
		if k.c.timer != nil {
			unblockTimerChan(k.c)
		}
		if sg != winner {
			if int(casei) < nsends {
				k.c.sendq.dequeueSudoG(sg)
			} else {
				k.c.recvq.dequeueSudoG(sg)
			}
		}
		sg.coro = nil
		sg.g = nil
		sg.isSelect = false
		sg.elem.set(nil)
		sg.c.set(nil)
		sg.waitlink = nil
		sg.next = nil
		sg.prev = nil
		releaseSudog(sg)
	}
	selunlock(scases, lockorder)

	task.selectDone.Store(0)
	if io.selectChosen != nil {
		*io.selectChosen = chosenIdx
	}
	if io.selectOKOut != nil {
		*io.selectOKOut = recvOK
	}
	io.selectCases = nil
	io.selectLockOrder = nil
	io.selectSGList = nil
	io.selectWinner = nil
	io.selectChosen = nil
	io.selectOKOut = nil

	if chosenIdx < nsends && !recvOK {
		coroPanic(unsafe.Pointer(task), plainError("send on closed channel"))
		return true
	}
	return false
}

// Unused import guard for atomic if needed elsewhere in package.
var _ = atomic.Load
