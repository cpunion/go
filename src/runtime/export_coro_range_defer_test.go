// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func NewStacklessCoroRangeDeferForTest() (any, unsafe.Pointer) {
	s := newStacklessCoroTestScheduler()
	task := &s.root
	task.context.scheduler = s
	task.state, task.resuming = stacklessCoroTaskRunning, true
	task.resume = func(unsafe.Pointer) uint8 { return stacklessCoroActionComplete }
	ctx := unsafe.Pointer(&task.context)
	return coroRangeDefer(ctx), ctx
}

var PushStacklessCoroRangeDeferForTest = coroRangeDeferPush
var NextStacklessCoroRangeDeferForTest = coroRangeDeferNext
var OwnerStacklessCoroRangeDeferForTest = coroRangeDeferOwner
var NewStacklessCoroRangeDeferAtForTest = coroRangeDefer

func StacklessCoroRangeDeferReleasedForTest(frame any) bool {
	a := checkedStacklessCoroRangeDefer(frame)
	return a.sealed && a.head.Load() == &stacklessCoroRangeDeferClosed &&
		a.owner.Load() == nil && a.frame == nil && a.resume == nil && a.pending == nil
}

func StacklessCoroRangeDeferInvalidForTest(mode string) {
	a, ctx := NewStacklessCoroRangeDeferForTest()
	coroRangeDeferPush(a, nil)
	switch mode {
	case "late-push":
		coroRangeDeferNext(ctx, a)
		coroRangeDeferPush(a, nil)
	case "late-owner":
		coroRangeDeferNext(ctx, a)
		coroRangeDeferOwner(a)
	case "wrong-task":
		_, other := NewStacklessCoroRangeDeferForTest()
		coroRangeDeferNext(other, a)
	case "wrong-frame":
		(*stacklessCoroContext)(ctx).frame = unsafe.Pointer(new(byte))
		coroRangeDeferNext(ctx, a)
	case "wrong-resume":
		(*stacklessCoroContext)(ctx).task().resume = func(unsafe.Pointer) uint8 {
			return stacklessCoroActionYield
		}
		coroRangeDeferNext(ctx, a)
	case "bad-anchor":
		coroRangeDeferPush(17, nil)
	default:
		throw("unknown range defer invalid test mode")
	}
}
