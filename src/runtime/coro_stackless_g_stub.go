// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.coro

package runtime

import "unsafe"

type stacklessCoroG struct{}

const stacklessCoroChannelWaiterScanning = false

type stacklessCoroSudog struct{}

func (*stacklessCoroSudog) get() unsafe.Pointer {
	return nil
}

func (*stacklessCoroSudog) set(unsafe.Pointer) {
	throw("runtime: stackless coroutine channel waiter without experiment")
}

func (*stacklessCoroSudog) clear() {}

func finishStacklessCoroChannel(unsafe.Pointer, *sudog, bool) {
	throw("runtime: stackless coroutine channel waiter without experiment")
}

func stacklessCoroSelectTry(unsafe.Pointer) bool {
	throw("runtime: stackless coroutine select without experiment")
	return false
}

func releaseStacklessCoroSudog(unsafe.Pointer, *sudog) {
	throw("runtime: stackless coroutine channel waiter without experiment")
}

func (*g) stackIsFixed() bool {
	return false
}

func stacklessCoroRecover(*g) any {
	return nil
}

//go:nosplit
func stacklessCoroExitsyscallNoP(*g) bool {
	return false
}

//go:nosplit
func stacklessCoroExitsyscallDone(*g) {}

// sudog (pseudo-g) represents a g in a wait list, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are ordinarily allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them. Stackless coroutine channel
// operations retain their own bounded waiter; see newStacklessCoroSudog.
// Stackless semaphore operations retain a waiter until acquisition succeeds.
// Stackless notification operations retain a waiter until its ticket is notified.
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.

	g *g

	// coro holds an owner only for a stackless coroutine waiter.
	// Such a waiter has no parked g; synchronization completion publishes the
	// operation to its logical scheduler instead. The field has zero size
	// when the coroutine experiment is disabled.
	coro stacklessCoroSudog

	next *sudog
	prev *sudog

	elem maybeTraceablePtr // data element (may point to stack)

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.

	acquiretime int64
	releasetime int64
	ticket      uint32

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	isSelect bool

	// success indicates whether communication over channel c
	// succeeded. It is true if the goroutine was awoken because a
	// value was delivered over channel c, and false if awoken
	// because c was closed.
	success bool

	// waiters is a count of semaRoot waiting list other than head of list,
	// clamped to a uint16 to fit in unused space.
	// Only meaningful at the head of the list.
	// (If we wanted to be overly clever, we could store a high 16 bits
	// in the second entry in the list.)
	waiters uint16

	parent   *sudog             // semaRoot binary tree
	waitlink *sudog             // g.waiting list or semaRoot
	waittail *sudog             // semaRoot
	c        maybeTraceableChan // channel
}

func isCompactStacklessCoroSudog(*sudog, unsafe.Pointer) bool { return false }

func adjustsudogs(gp *g, adjinfo *adjustinfo) {
	// the data elements pointed to by a SudoG structure
	// might be in the stack.
	for s := gp.waiting; s != nil; s = s.waitlink {
		adjustpointer(adjinfo, unsafe.Pointer(&s.elem.vu))
		adjustpointer(adjinfo, unsafe.Pointer(&s.elem.vp))
	}
}

// The disabled experiment keeps the original conditional-pointer layout and
// the same inlinable accesses to its stored addresses.
//
//go:nosplit
func (sg *sudog) getElem() unsafe.Pointer { return sg.elem.get() }

//go:nosplit
func (sg *sudog) setElem(v unsafe.Pointer) { sg.elem.set(v) }

//go:nosplit
func (sg *sudog) getChannel() *hchan { return sg.c.get() }

//go:nosplit
func (sg *sudog) setChannel(c *hchan) { sg.c.set(c) }

//go:nosplit
func (sg *sudog) elemUintptr() uintptr { return sg.elem.uintptr() }

//go:nosplit
func (sg *sudog) channelUintptr() uintptr { return sg.c.uintptr() }

//go:nosplit
func (sg *sudog) setElemUntraceable() { sg.elem.setUntraceable() }

//go:nosplit
func (sg *sudog) setElemTraceable() { sg.elem.setTraceable() }

//go:nosplit
func (sg *sudog) setChannelUntraceable() { sg.c.setUntraceable() }

//go:nosplit
func (sg *sudog) setChannelTraceable() { sg.c.setTraceable() }

// Full-only accesses are used where the caller already owns a complete sudog.
// Native channel accesses are used only with a live coroutine owner; channel
// waiters never enter g.waiting and never need untraceable address mirrors.
//
//go:nosplit
func (sg *sudog) getFullElem() unsafe.Pointer { return sg.elem.get() }

//go:nosplit
func (sg *sudog) setFullElem(v unsafe.Pointer) { sg.elem.set(v) }

//go:nosplit
func (sg *sudog) getNativeElem() unsafe.Pointer { return sg.elem.get() }

//go:nosplit
func (sg *sudog) setNativeElem(v unsafe.Pointer) { sg.elem.set(v) }

//go:nosplit
func (sg *sudog) setNativeChannel(c *hchan) { sg.c.set(c) }

// The disabled experiment uses the original sudog and field accesses.
type channelWaiter = sudog

func (sg *sudog) channelWaiter() *channelWaiter { return sg }
func (sg *sudog) fullSudog() *sudog             { return sg }
func (sg *sudog) channelSelect() bool           { return sg.isSelect }
