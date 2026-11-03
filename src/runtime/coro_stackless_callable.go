// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import (
	"internal/abi"
	"unsafe"
)

// The compiler emits this immutable record beside an admitted factory. The
// gate covers the erased adapter and its exact typed factory independently.
// Source function values and interface method tables retain the Go ABI.
type stacklessCoroCallable struct {
	version     uintptr
	signature   *_type
	gate        *[2]uintptr
	factoryCode uintptr
	deferGate   *[2]uintptr
	deferCode   uintptr
	method      *stacklessCoroCallable
	boundMethod uintptr
	nilCheck    uintptr
}

type stacklessCoroCallableInfo struct {
	version        uint32
	providerOffset uint32
}

// coroLookupCallable only discovers a bounded native companion. Missing,
// mismatched, older, or rejected companions do not acquire native capability.
// Open call sites still need an ordinary-stack execution path for those cases.
func coroLookupCallable(code uintptr, signature unsafe.Pointer) unsafe.Pointer {
	return lookupStacklessCoroCallable(code, signature, false)
}

func lookupStacklessCoroCallable(code uintptr, signature unsafe.Pointer, deferred bool) unsafe.Pointer {
	return unsafe.Pointer(checkStacklessCoroCallable(loadStacklessCoroCallable(code), signature, deferred))
}

func loadStacklessCoroCallable(code uintptr) *stacklessCoroCallable {
	if code == 0 {
		return nil
	}
	fn := findfunc(code)
	if !fn.valid() || fn.entry() != code {
		return nil
	}
	p := funcdata(fn, abi.FUNCDATA_CoroCallable)
	if p == nil {
		return nil
	}
	info := (*stacklessCoroCallableInfo)(p)
	if info.version != abi.CoroCallableVersion {
		return nil
	}
	provider := funcval{fn: fn.datap.textAddr(info.providerOffset)}
	providerData := noescape(unsafe.Pointer(&provider))
	load := *(*func() unsafe.Pointer)(unsafe.Pointer(&providerData))
	entry := (*stacklessCoroCallable)(load())
	if entry == nil || entry.version != abi.CoroCallableVersion {
		return nil
	}
	return entry
}

func checkStacklessCoroCallable(entry *stacklessCoroCallable, signature unsafe.Pointer, deferred bool) *stacklessCoroCallable {
	if entry == nil || entry.version != abi.CoroCallableVersion || signature == nil || entry.signature != (*_type)(signature) {
		return nil
	}
	gate, factory := entry.gate, entry.factoryCode
	if deferred {
		gate, factory = entry.deferGate, entry.deferCode
	}
	if gate == nil || gate[0] != 0 || factory == 0 {
		return nil
	}
	return entry
}

// A frozen starter is invoked once after its registration has been popped.
// Only an independently proved direct-defer companion can run natively. The
// ordinary thunk registers the source target directly on its logical G.
func coroDeferStartFunction(token unsafe.Pointer, value any, request unsafe.Pointer, ordinary func(any, bool, bool)) {
	task := activeStacklessCoroDeferTask(token, "runtime: invalid stackless coroutine function defer")
	ctx := unsafe.Pointer(&task.context)
	entry, environment := lookupStacklessCoroFunction(value, true)
	startStacklessCoroCallableDefer(ctx, entry, environment, request, ordinary)
}

func coroDeferStartInterface(token, table, environment unsafe.Pointer, method uintptr, signature any, request unsafe.Pointer, ordinary func(any, bool, bool)) {
	task := activeStacklessCoroDeferTask(token, "runtime: invalid stackless coroutine interface defer")
	entry := lookupStacklessCoroInterface(table, environment, method, signature, true)
	startStacklessCoroCallableDefer(unsafe.Pointer(&task.context), entry, environment, request, ordinary)
}

func startStacklessCoroCallableDefer(ctx unsafe.Pointer, entry *stacklessCoroCallable, environment, request unsafe.Pointer, ordinary func(any, bool, bool)) {
	if entry != nil {
		data := unsafe.Pointer(&entry.deferCode)
		invoke := *(*func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) (unsafe.Pointer, stacklessCoroResume))(unsafe.Pointer(&data))
		frame, resume := invoke(ctx, environment, request)
		coroDeferAwaitFrame(ctx, frame, resume)
		return
	}

	coroHostDefer(ctx, ordinary)
}

// coroCallableFactory invokes the one erased ABI after lookup has checked its
// source signature. Its three pointer arguments keep aggregate argument copies
// inside the independently proved adapter rather than the runtime bridge.
func coroCallableFactory(ctx, descriptor, environment, request unsafe.Pointer) (unsafe.Pointer, stacklessCoroResume) {
	entry := (*stacklessCoroCallable)(descriptor)
	context := (*stacklessCoroContext)(ctx)
	if entry == nil || entry.version != abi.CoroCallableVersion ||
		entry.gate == nil || entry.gate[0] != 0 || entry.factoryCode == 0 ||
		context == nil || context.scheduler == nil || context.task() == nil {
		throw("runtime: invalid stackless coroutine callable factory")
	}
	data := unsafe.Pointer(&entry.factoryCode)
	invoke := *(*func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) (unsafe.Pointer, stacklessCoroResume))(unsafe.Pointer(&data))
	return invoke(ctx, environment, request)
}
