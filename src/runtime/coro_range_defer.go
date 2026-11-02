// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// A range defer belongs to the activation that created its anchor, even when
// a yielded body runs in a separate task. Frame/resume identity also distinguishes
// activations when a task supports fused calls. The compiler keeps the marker in
// that owner's cleanup stack until next reports that the anchor is drained.
type stacklessCoroRangeDefer struct {
	head    atomic.Pointer[stacklessCoroRangeDeferCall]
	owner   atomic.Pointer[stacklessCoroTask]
	frame   unsafe.Pointer
	resume  *funcval
	pending *stacklessCoroRangeDeferCall
	sealed  bool // pending and sealed are accessed only by the cleanup owner
}

type stacklessCoroRangeDeferCall struct {
	fn   func()
	next *stacklessCoroRangeDeferCall
}

// A real static object keeps the poisoned atomic head valid for GC scanning.
// Its identity cannot equal any allocated registration record, including a nil
// deferred function. No callback is ever invoked from this sentinel.
var stacklessCoroRangeDeferClosed stacklessCoroRangeDeferCall

func checkedStacklessCoroRangeDefer(frame any) *stacklessCoroRangeDefer {
	a, ok := frame.(*stacklessCoroRangeDefer)
	if !ok || a == nil {
		throw("runtime: invalid stackless coroutine range defer anchor")
	}
	return a
}

// The representation stays native after draining. An ordinary callback must
// never pass a completed native anchor to ordinary deferprocat, whose token
// has a different concrete runtime type. No current-task assumption is made.
func coroRangeDeferIsNative(frame any) bool {
	a, ok := frame.(*stacklessCoroRangeDefer)
	return ok && a != nil
}

func coroRangeDefer(ctx unsafe.Pointer) any {
	owner := (*stacklessCoroTask)(coroDeferToken(ctx))
	a := &stacklessCoroRangeDefer{
		frame:  owner.context.frame,
		resume: stacklessCoroResumeIdentity(owner.resume),
	}
	a.owner.Store(owner)
	return a
}

// coroRangeDeferOwner snapshots the enclosing cleanup token during a valid
// registration. The registering callback need not be the current owner task.
// Terminal/recover wrappers capture this value rather than their child's token.
func coroRangeDeferOwner(frame any) unsafe.Pointer {
	a := checkedStacklessCoroRangeDefer(frame)
	owner := a.owner.Load()
	if a.head.Load() == &stacklessCoroRangeDeferClosed || owner == nil {
		throw("defer after range func returned")
	}
	return unsafe.Pointer(owner)
}

func coroRangeDeferPush(frame any, fn func()) {
	a := checkedStacklessCoroRangeDefer(frame)
	d := &stacklessCoroRangeDeferCall{fn: fn}
	for {
		d.next = a.head.Load()
		if d.next == &stacklessCoroRangeDeferClosed {
			throw("defer after range func returned")
		}
		if a.head.CompareAndSwap(d.next, d) {
			return
		}
	}
}

// coroRangeDeferNext seals the registration list at its marker position and
// removes exactly one callback before returning it. It never invokes user code.
// A callback panic/Goexit therefore leaves the remaining records reachable from
// the owner's still-registered marker. A nil fn with ok=true is a real deferred
// nil call and must retain its ordinary invocation-time panic.
func coroRangeDeferNext(ctx unsafe.Pointer, frame any) (fn func(), ok bool) {
	current := (*stacklessCoroTask)(coroDeferToken(ctx))
	a := checkedStacklessCoroRangeDefer(frame)
	owner := a.owner.Load()
	if owner == nil {
		return nil, false
	}
	if current != owner || current.context.frame != a.frame ||
		stacklessCoroResumeIdentity(current.resume) != a.resume {
		throw("runtime: invalid stackless coroutine range defer owner")
	}
	if !a.sealed {
		for {
			head := a.head.Load()
			if a.head.CompareAndSwap(head, &stacklessCoroRangeDeferClosed) {
				a.pending = head
				break
			}
		}
		a.sealed = true
	}
	d := a.pending
	if d != nil {
		fn, ok = d.fn, true
		a.pending = d.next
		d.fn, d.next = nil, nil
	}
	if a.pending == nil {
		// An escaped yield closure can retain the poisoned anchor. Release
		// the completed activation/scheduler and frame even in that case.
		a.frame, a.resume = nil, nil
		a.owner.Store(nil)
	}
	return fn, ok
}
