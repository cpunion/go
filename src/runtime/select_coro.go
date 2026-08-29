// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// stacklessCoroSelect owns the channel waiters for one logical select. The
// ordinary select path stores its arbitration bit on g. A stackless logical
// goroutine has no parked g, so its operation owns the bit instead.
type stacklessCoroSelect struct {
	cases     []scase
	lockOrder []uint16
	waiters   []*sudog
	chosen    *int
	received  *bool
	nsends    int
	done      atomic.Uint32
}

// Retaining the common small-select storage with each cached operation keeps
// steady ready selects allocation-free without allowing a rare large select
// to enlarge the bounded operation cache indefinitely.
const stacklessCoroSelectCaseCacheSize = 16

func (selection *stacklessCoroSelect) active() bool {
	return selection != nil && selection.chosen != nil
}

func (selection *stacklessCoroSelect) validCache() bool {
	return selection == nil ||
		(selection.cases == nil && len(selection.lockOrder) == 0 &&
			cap(selection.lockOrder) <= stacklessCoroSelectCaseCacheSize &&
			len(selection.waiters) == 0 &&
			cap(selection.waiters) <= stacklessCoroSelectCaseCacheSize &&
			selection.chosen == nil &&
			selection.received == nil && selection.nsends == 0 &&
			selection.done.Load() == 0)
}

func (selection *stacklessCoroSelect) prepare(cases []scase,
	nlocks, ncases, nsends int, chosen *int, received *bool) {
	if !selection.validCache() {
		throw("runtime: active stackless coroutine select reuse")
	}

	if nlocks <= stacklessCoroSelectCaseCacheSize {
		if cap(selection.lockOrder) < nlocks {
			selection.lockOrder = make([]uint16, nlocks)
		} else {
			selection.lockOrder = selection.lockOrder[:nlocks]
		}
	} else {
		selection.lockOrder = make([]uint16, nlocks)
	}
	if ncases <= stacklessCoroSelectCaseCacheSize {
		if cap(selection.waiters) < ncases {
			selection.waiters = make([]*sudog, ncases)
		} else {
			selection.waiters = selection.waiters[:ncases]
			clear(selection.waiters)
		}
	} else {
		selection.waiters = make([]*sudog, ncases)
	}

	selection.cases = cases
	selection.chosen = chosen
	selection.received = received
	selection.nsends = nsends
}

func (selection *stacklessCoroSelect) clear() {
	for i := range selection.cases {
		selection.cases[i] = scase{}
	}
	selection.cases = nil
	if cap(selection.lockOrder) > stacklessCoroSelectCaseCacheSize {
		selection.lockOrder = nil
	} else {
		selection.lockOrder = selection.lockOrder[:0]
	}
	clear(selection.waiters)
	if cap(selection.waiters) > stacklessCoroSelectCaseCacheSize {
		selection.waiters = nil
	} else {
		selection.waiters = selection.waiters[:0]
	}
	selection.chosen = nil
	selection.received = nil
	selection.nsends = 0
	selection.done.Store(0)
}

// stacklessCoroSelectTry arbitrates a waiter removed from a channel queue.
// The winning producer finishes the operation after dropping channel locks.
func stacklessCoroSelectTry(owner unsafe.Pointer) bool {
	op := (*stacklessCoroOperation)(owner)
	if op == nil || !op.selection.active() {
		throw("runtime: invalid stackless coroutine select waiter")
	}
	return op.selection.done.CompareAndSwap(0, 1)
}

func stacklessCoroSelectCases(cases0 *scase, nsends, nrecvs int,
	chosen *int, received *bool) []scase {
	ncases := nsends + nrecvs
	if nsends < 0 || nrecvs < 0 || ncases < 0 || ncases > 1<<16 ||
		(ncases != 0 && cases0 == nil) || chosen == nil || received == nil {
		throw("runtime: invalid stackless coroutine select")
	}

	if ncases == 0 {
		return nil
	}
	return (*[1 << 16]scase)(unsafe.Pointer(cases0))[:ncases:ncases]
}

// stacklessCoroSelectMayBeReady is a hint that avoids a redundant locked poll
// before a select that is visibly blocked. Both the direct and retained paths
// arbitrate under channel locks, so a stale result only changes which one is
// attempted first.
func stacklessCoroSelectMayBeReady(cases []scase, nsends int) bool {
	for i := range cases {
		c := cases[i].c
		if c == nil {
			continue
		}
		if c.bubble != nil {
			return true
		}
		if i < nsends {
			if c.closed != 0 || !full(c) {
				return true
			}
			continue
		}
		if !empty(c) || atomic.Load(&c.closed) != 0 {
			return true
		}
	}
	return false
}

// tryStacklessCoroSelect completes a small nonblocking select directly on the
// current executor. It reports whether it selected a case or the default.
func tryStacklessCoroSelect(ctx unsafe.Pointer, cases []scase, nsends int,
	block bool, chosen *int, received *bool) bool {
	if raceenabled || len(cases) == 0 ||
		len(cases) > stacklessCoroSelectCaseCacheSize {
		return false
	}
	context := (*stacklessCoroContext)(ctx)
	if context == nil || context.scheduler == nil ||
		stacklessCoroIsPullComparison(context.scheduler) {
		return false
	}
	if block && !stacklessCoroSelectMayBeReady(cases, nsends) {
		return false
	}

	var order [2 * stacklessCoroSelectCaseCacheSize]uint16
	casi, recvOK := selectgo(&cases[0], &order[0], nil, nsends,
		len(cases)-nsends, false)
	if casi < 0 && block {
		return false
	}
	clear(cases)
	*chosen = casi
	*received = recvOK
	return true
}

func startStacklessCoroSelect(ctx unsafe.Pointer, cases0 *scase,
	nsends, nrecvs int, block bool, chosen *int, received *bool) {
	cases := stacklessCoroSelectCases(cases0, nsends, nrecvs, chosen, received)
	startStacklessCoroSelectCases(ctx, cases, nsends, block, chosen, received)
}

func startStacklessCoroSelectCases(ctx unsafe.Pointer, cases []scase,
	nsends int, block bool, chosen *int, received *bool) {
	ncases := len(cases)
	pollOrder := make([]uint16, 0, ncases)
	for i := range cases {
		c := cases[i].c
		if c == nil {
			cases[i].elem = nil
			continue
		}
		if c.bubble != nil && getg().bubble != c.bubble {
			fatal("select on synctest channel from outside bubble")
		}
		if c.timer != nil {
			c.timer.maybeRunChan(c)
		}
		j := int(cheaprandn(uint32(len(pollOrder) + 1)))
		pollOrder = append(pollOrder, 0)
		pollOrder[len(pollOrder)-1] = pollOrder[j]
		pollOrder[j] = uint16(i)
	}
	op := stacklessCoroStartOperation(ctx, "select")
	selection := op.selection
	if selection == nil {
		selection = new(stacklessCoroSelect)
		op.selection = selection
	}
	selection.prepare(cases, len(pollOrder), ncases,
		nsends, chosen, received)
	lockOrder := selection.lockOrder
	sortStacklessCoroSelectLocks(cases, pollOrder, lockOrder)
	op.id = registerStacklessCoroOperation(op)

	sellock(cases, lockOrder)
	var (
		casi int
		cas  *scase
		c    *hchan
		sg   *sudog
		qp   unsafe.Pointer
	)
	for _, casei := range pollOrder {
		casi = int(casei)
		cas = &cases[casi]
		c = cas.c
		if casi >= nsends {
			sg = c.sendq.dequeue()
			if sg != nil {
				goto recv
			}
			if c.qcount > 0 {
				goto bufrecv
			}
			if c.closed != 0 {
				goto rclose
			}
			continue
		}
		if raceenabled {
			racereadpc(c.raceaddr(), sys.GetCallerPC(), chansendpc)
		}
		if c.closed != 0 {
			goto sclose
		}
		sg = c.recvq.dequeue()
		if sg != nil {
			goto send
		}
		if c.qcount < c.dataqsiz {
			goto bufsend
		}
	}

	if !block {
		selunlock(cases, lockOrder)
		completeStacklessCoroSelect(op, -1, false, false)
		return
	}

	for _, casei := range lockOrder {
		casi = int(casei)
		cas = &cases[casi]
		c = cas.c
		sg = newStacklessCoroSudog(op, &selection.waiters[casi])
		sg.releasetime = 0
		sg.elem.set(cas.elem)
		sg.waitlink = nil
		sg.coro.set(unsafe.Pointer(op))
		sg.isSelect = true
		sg.c.set(c)
		if casi < nsends {
			c.sendq.enqueue(sg)
		} else {
			c.recvq.enqueue(sg)
		}
		if c.timer != nil {
			blockTimerChan(c)
		}
		if raceenabled {
			racerelease(unsafe.Pointer(op))
		}
	}
	selunlock(cases, lockOrder)
	return

bufrecv:
	if raceenabled {
		if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, sys.GetCallerPC(), chanrecvpc)
		}
		racenotify(c, c.recvx, nil)
	}
	if msanenabled && cas.elem != nil {
		msanwrite(cas.elem, c.elemtype.Size_)
	}
	if asanenabled && cas.elem != nil {
		asanwrite(cas.elem, c.elemtype.Size_)
	}
	qp = chanbuf(c, c.recvx)
	if cas.elem != nil {
		typedmemmove(c.elemtype, cas.elem, qp)
	}
	typedmemclr(c.elemtype, qp)
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
	selunlock(cases, lockOrder)
	completeStacklessCoroSelect(op, casi, true, false)
	return

bufsend:
	if raceenabled {
		racenotify(c, c.sendx, nil)
		raceReadObjectPC(c.elemtype, cas.elem, sys.GetCallerPC(), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.Size_)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.Size_)
	}
	typedmemmove(c.elemtype, chanbuf(c, c.sendx), cas.elem)
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
	selunlock(cases, lockOrder)
	completeStacklessCoroSelect(op, casi, false, false)
	return

recv:
	recv(c, sg, cas.elem, func() { selunlock(cases, lockOrder) }, 2)
	completeStacklessCoroSelect(op, casi, true, false)
	return

rclose:
	selunlock(cases, lockOrder)
	if cas.elem != nil {
		typedmemclr(c.elemtype, cas.elem)
	}
	if raceenabled {
		raceacquire(c.raceaddr())
	}
	completeStacklessCoroSelect(op, casi, false, false)
	return

send:
	if raceenabled {
		raceReadObjectPC(c.elemtype, cas.elem, sys.GetCallerPC(), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.Size_)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.Size_)
	}
	send(c, sg, cas.elem, func() { selunlock(cases, lockOrder) }, 2)
	completeStacklessCoroSelect(op, casi, false, false)
	return

sclose:
	selunlock(cases, lockOrder)
	completeStacklessCoroSelect(op, casi, false, true)
}

func sortStacklessCoroSelectLocks(cases []scase, pollOrder, lockOrder []uint16) {
	for i := range lockOrder {
		j := i
		o := pollOrder[i]
		c := cases[o].c
		for j > 0 &&
			cases[lockOrder[(j-1)/2]].c.sortkey() < c.sortkey() {
			k := (j - 1) / 2
			lockOrder[j] = lockOrder[k]
			j = k
		}
		lockOrder[j] = o
	}
	for i := len(lockOrder) - 1; i >= 0; i-- {
		o := lockOrder[i]
		c := cases[o].c
		lockOrder[i] = lockOrder[0]
		j := 0
		for {
			k := j*2 + 1
			if k >= i {
				break
			}
			if k+1 < i &&
				cases[lockOrder[k]].c.sortkey() <
					cases[lockOrder[k+1]].c.sortkey() {
				k++
			}
			if c.sortkey() < cases[lockOrder[k]].c.sortkey() {
				lockOrder[j] = lockOrder[k]
				j = k
				continue
			}
			break
		}
		lockOrder[j] = o
	}
}

func finishStacklessCoroSelect(op *stacklessCoroOperation, winner *sudog,
	success bool) {
	if op == nil || winner == nil || !op.selection.active() ||
		op.selection.done.Load() != 1 ||
		takeStacklessCoroOperation(op.id) != op {
		throw("runtime: invalid stackless coroutine select completion")
	}
	selection := op.selection
	chosen := -1
	for i, waiter := range selection.waiters {
		if waiter == winner {
			chosen = i
			break
		}
	}
	if chosen < 0 {
		throw("runtime: stackless coroutine select winner is not registered")
	}

	sellock(selection.cases, selection.lockOrder)
	for _, casei := range selection.lockOrder {
		i := int(casei)
		waiter := selection.waiters[i]
		if waiter == nil {
			selunlock(selection.cases, selection.lockOrder)
			throw("runtime: missing stackless coroutine select waiter")
		}
		c := selection.cases[i].c
		if c.timer != nil {
			unblockTimerChan(c)
		}
		if waiter != winner {
			if i < selection.nsends {
				c.sendq.dequeueSudoG(waiter)
			} else {
				c.recvq.dequeueSudoG(waiter)
			}
		}
		waiter.isSelect = false
		waiter.elem.set(nil)
		waiter.coro.clear()
		waiter.c.set(nil)
		waiter.waitlink = nil
	}
	selunlock(selection.cases, selection.lockOrder)
	for _, casei := range selection.lockOrder {
		i := int(casei)
		waiter := selection.waiters[i]
		releaseStacklessCoroSudog(unsafe.Pointer(op), waiter)
		selection.waiters[i] = nil
	}

	sendClosed := chosen < selection.nsends && !success
	recvOK := chosen >= selection.nsends && success
	publishStacklessCoroSelect(op, selection, chosen, recvOK, sendClosed)
}

func completeStacklessCoroSelect(op *stacklessCoroOperation, chosen int,
	recvOK, sendClosed bool) {
	if op == nil || !op.selection.active() ||
		takeStacklessCoroOperation(op.id) != op {
		throw("runtime: invalid immediate stackless coroutine select completion")
	}
	publishStacklessCoroSelect(op, op.selection, chosen, recvOK, sendClosed)
}

func publishStacklessCoroSelect(op *stacklessCoroOperation,
	selection *stacklessCoroSelect, chosen int, recvOK, sendClosed bool) {
	*selection.chosen = chosen
	*selection.received = recvOK
	selection.clear()
	if sendClosed {
		panicStacklessCoroOperation(op, plainError("send on closed channel"))
		return
	}
	completeStacklessCoroOperation(op)
}
