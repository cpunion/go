// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

// coroCallableEntry maps an ordinary Go function/method entry PC to its
// compiler-generated coroutine factory funcval and optional result copier.
type coroCallableEntry struct {
	pc        uintptr
	factory   unsafe.Pointer
	resCopier unsafe.Pointer // func(frame unsafe.Pointer, out0 *R0, ...); nil for void
	isMethod  bool
}

type coroCallableTable struct {
	mask    uintptr
	count   uintptr
	entries []coroCallableEntry
}

var coroCallables struct {
	lock  mutex
	table atomic.Pointer[coroCallableTable]
}

// coroIOEntry maps a concrete stdlib Read/Write method PC (e.g. (*os.File).Read,
// (*net.TCPConn).Read) to direct netpoll start/finish adapters for interface I/O.
type coroIOEntry struct {
	pc     uintptr
	start  unsafe.Pointer
	finish unsafe.Pointer
}

type coroIOTable struct {
	mask    uintptr
	count   uintptr
	entries []coroIOEntry
}

var coroIOAdapters struct {
	lock  mutex
	table atomic.Pointer[coroIOTable]
}

// coroAcceptEntry maps a concrete stdlib Listener.Accept method PC to a direct
// netpoll step adapter.
type coroAcceptEntry struct {
	pc   uintptr
	step unsafe.Pointer
}

type coroAcceptTable struct {
	mask    uintptr
	count   uintptr
	entries []coroAcceptEntry
}

var coroAcceptAdapters struct {
	lock  mutex
	table atomic.Pointer[coroAcceptTable]
}

func init() {
	lockInit(&coroCallables.lock, lockRankCoroSched)
	lockInit(&coroIOAdapters.lock, lockRankCoroSched)
	lockInit(&coroAcceptAdapters.lock, lockRankCoroSched)
}

func coroCallableHash(pc uintptr) uintptr {
	x := pc ^ (pc >> 16)
	x *= 0x45d9f3b
	return x ^ (x >> 16)
}

func coroLookupPCEntry(pc uintptr, wantMethod bool) (factory unsafe.Pointer, resCopier unsafe.Pointer) {
	tbl := coroCallables.table.Load()
	if tbl == nil || pc == 0 {
		return nil, nil
	}
	idx := coroCallableHash(pc) & tbl.mask
	for {
		e := tbl.entries[idx]
		if e.pc == pc {
			if e.isMethod != wantMethod {
				return nil, nil
			}
			return e.factory, e.resCopier
		}
		if e.pc == 0 {
			return nil, nil
		}
		idx = (idx + 1) & tbl.mask
	}
}

func coroLookupPC(pc uintptr, wantMethod bool) unsafe.Pointer {
	fac, _ := coroLookupPCEntry(pc, wantMethod)
	return fac
}

func coroRegisterCallableEntry(pc uintptr, facPtr, resPtr unsafe.Pointer, isMethod bool) {
	if pc == 0 || facPtr == nil {
		return
	}
	lock(&coroCallables.lock)
	old := coroCallables.table.Load()
	var nCap uintptr = 64
	if old != nil {
		idx := coroCallableHash(pc) & old.mask
		for {
			if old.entries[idx].pc == pc {
				old.entries[idx].factory = facPtr
				old.entries[idx].resCopier = resPtr
				old.entries[idx].isMethod = isMethod
				unlock(&coroCallables.lock)
				return
			}
			if old.entries[idx].pc == 0 {
				break
			}
			idx = (idx + 1) & old.mask
		}
		if (old.count+1)*2 <= uintptr(len(old.entries)) {
			nCap = uintptr(len(old.entries))
		} else {
			nCap = uintptr(len(old.entries)) * 2
		}
	}
	nt := &coroCallableTable{
		mask:    nCap - 1,
		entries: make([]coroCallableEntry, nCap),
	}
	if old != nil {
		for _, e := range old.entries {
			if e.pc != 0 {
				coroCallableInsert(nt, e)
			}
		}
	}
	coroCallableInsert(nt, coroCallableEntry{pc: pc, factory: facPtr, resCopier: resPtr, isMethod: isMethod})
	coroCallables.table.Store(nt)
	unlock(&coroCallables.lock)
}

// coroRegisterCallable registers a lowered coroutine factory for dynamic
// function / interface dispatch (called from compiler-emitted package init).
func coroRegisterCallable(fn any, factory any) {
	coroRegisterCallableAwait(fn, factory, nil, false)
}

// coroRegisterCallableAwait registers a lowered coroutine factory plus its
// optional result-copier funcval for cross-package and dynamic zero-stack await.
func coroRegisterCallableAwait(fn any, factory any, resCopier any, isMethod bool) {
	ef := efaceOf(&fn)
	ff := efaceOf(&factory)
	if ef == nil || ef.data == nil || ff == nil || ff.data == nil {
		return
	}
	fv := (*funcval)(ef.data)
	if fv == nil || fv.fn == 0 {
		return
	}
	var resPtr unsafe.Pointer
	if resCopier != nil {
		rf := efaceOf(&resCopier)
		if rf != nil {
			resPtr = rf.data
		}
	}
	coroRegisterCallableEntry(fv.fn, ff.data, resPtr, isMethod)
}

func coroCallableInsert(nt *coroCallableTable, e coroCallableEntry) {
	idx := coroCallableHash(e.pc) & nt.mask
	for nt.entries[idx].pc != 0 {
		idx = (idx + 1) & nt.mask
	}
	nt.entries[idx] = e
	nt.count++
}

// coroLookupFunction resolves a dynamic Go function value (direct-iface *funcval)
// to its coroutine factory funcval and closure pointer (lock-free).
func coroLookupFunction(fn any) (factory unsafe.Pointer, closure unsafe.Pointer) {
	ef := efaceOf(&fn)
	if ef == nil || ef.data == nil {
		return nil, nil
	}
	fv := (*funcval)(ef.data)
	if fv == nil || fv.fn == 0 {
		return nil, nil
	}
	fac := coroLookupPC(fv.fn, false)
	if fac == nil {
		return nil, nil
	}
	return fac, unsafe.Pointer(fv)
}

// coroLookupAwaitFunction resolves a function or method expression to its
// coroutine factory, closure pointer, and optional result-copier funcval (lock-free).
func coroLookupAwaitFunction(fn any, wantMethod bool) (factory, closure, resCopier unsafe.Pointer) {
	ef := efaceOf(&fn)
	if ef == nil || ef.data == nil {
		return nil, nil, nil
	}
	fv := (*funcval)(ef.data)
	if fv == nil || fv.fn == 0 {
		return nil, nil, nil
	}
	fac, res := coroLookupPCEntry(fv.fn, wantMethod)
	if fac == nil {
		return nil, nil, nil
	}
	return fac, unsafe.Pointer(fv), res
}

// coroLookupInterface resolves a dynamic interface method call (itab.Fun[methodIdx])
// to its coroutine factory funcval and concrete receiver data pointer (lock-free).
func coroLookupInterface(itab, data unsafe.Pointer, methodIdx uintptr) (factory unsafe.Pointer, receiver unsafe.Pointer) {
	if itab == nil {
		return nil, nil
	}
	tab := (*abi.ITab)(itab)
	fnPC := *(*uintptr)(add(unsafe.Pointer(&tab.Fun[0]), methodIdx*unsafe.Sizeof(uintptr(0))))
	fac := coroLookupPC(fnPC, true)
	if fac == nil {
		return nil, nil
	}
	return fac, data
}

// coroLookupAwaitInterface resolves an interface method call to its coroutine
// factory, receiver data pointer, and optional result-copier funcval (lock-free).
func coroLookupAwaitInterface(itab, data unsafe.Pointer, methodIdx uintptr) (factory, receiver, resCopier unsafe.Pointer) {
	if itab == nil {
		return nil, nil, nil
	}
	tab := (*abi.ITab)(itab)
	fnPC := *(*uintptr)(add(unsafe.Pointer(&tab.Fun[0]), methodIdx*unsafe.Sizeof(uintptr(0))))
	fac, res := coroLookupPCEntry(fnPC, true)
	if fac == nil {
		return nil, nil, nil
	}
	return fac, data, res
}

// coroRegisterIO registers a concrete Read/Write method PC with its direct
// netpoll start and finish adapters for zero-stack io.Reader/io.Writer/net.Conn I/O.
func coroRegisterIO(methodFn, startFn, finishFn any) {
	ef := efaceOf(&methodFn)
	sf := efaceOf(&startFn)
	ff := efaceOf(&finishFn)
	if ef == nil || ef.data == nil || sf == nil || sf.data == nil || ff == nil || ff.data == nil {
		return
	}
	fv := (*funcval)(ef.data)
	if fv == nil || fv.fn == 0 {
		return
	}
	pc := fv.fn
	lock(&coroIOAdapters.lock)
	old := coroIOAdapters.table.Load()
	var nCap uintptr = 32
	if old != nil {
		idx := coroCallableHash(pc) & old.mask
		for {
			if old.entries[idx].pc == pc {
				old.entries[idx].start = sf.data
				old.entries[idx].finish = ff.data
				unlock(&coroIOAdapters.lock)
				return
			}
			if old.entries[idx].pc == 0 {
				break
			}
			idx = (idx + 1) & old.mask
		}
		if (old.count+1)*2 <= uintptr(len(old.entries)) {
			nCap = uintptr(len(old.entries))
		} else {
			nCap = uintptr(len(old.entries)) * 2
		}
	}
	nt := &coroIOTable{mask: nCap - 1, entries: make([]coroIOEntry, nCap)}
	if old != nil {
		for _, e := range old.entries {
			if e.pc != 0 {
				idx := coroCallableHash(e.pc) & nt.mask
				for nt.entries[idx].pc != 0 {
					idx = (idx + 1) & nt.mask
				}
				nt.entries[idx] = e
				nt.count++
			}
		}
	}
	idx := coroCallableHash(pc) & nt.mask
	for nt.entries[idx].pc != 0 {
		idx = (idx + 1) & nt.mask
	}
	nt.entries[idx] = coroIOEntry{pc: pc, start: sf.data, finish: ff.data}
	nt.count++
	coroIOAdapters.table.Store(nt)
	unlock(&coroIOAdapters.lock)
}

//go:linkname os_coroRegisterIO os.runtime_coroRegisterIO
func os_coroRegisterIO(methodFn, startFn, finishFn any) {
	coroRegisterIO(methodFn, startFn, finishFn)
}

//go:linkname net_coroRegisterIO net.runtime_coroRegisterIO
func net_coroRegisterIO(methodFn, startFn, finishFn any) {
	coroRegisterIO(methodFn, startFn, finishFn)
}

// coroLookupIO resolves an io.Reader/io.Writer/net.Conn interface method call
// to a registered direct netpoll start/finish adapter pair (lock-free).
func coroLookupIO(itab, data unsafe.Pointer, methodIdx uintptr) (start, finish, recv unsafe.Pointer) {
	tbl := coroIOAdapters.table.Load()
	if tbl == nil || itab == nil {
		return nil, nil, nil
	}
	tab := (*abi.ITab)(itab)
	fnPC := *(*uintptr)(add(unsafe.Pointer(&tab.Fun[0]), methodIdx*unsafe.Sizeof(uintptr(0))))
	if fnPC == 0 {
		return nil, nil, nil
	}
	idx := coroCallableHash(fnPC) & tbl.mask
	for {
		e := tbl.entries[idx]
		if e.pc == fnPC {
			return e.start, e.finish, data
		}
		if e.pc == 0 {
			return nil, nil, nil
		}
		idx = (idx + 1) & tbl.mask
	}
}

//go:linkname net_coroRegisterAccept net.runtime_coroRegisterAccept
func net_coroRegisterAccept(methodFn, stepFn any) {
	ef := efaceOf(&methodFn)
	sf := efaceOf(&stepFn)
	if ef == nil || ef.data == nil || sf == nil || sf.data == nil {
		return
	}
	fv := (*funcval)(ef.data)
	if fv == nil || fv.fn == 0 {
		return
	}
	pc := fv.fn
	lock(&coroAcceptAdapters.lock)
	old := coroAcceptAdapters.table.Load()
	var nCap uintptr = 16
	if old != nil {
		idx := coroCallableHash(pc) & old.mask
		for {
			if old.entries[idx].pc == pc {
				old.entries[idx].step = sf.data
				unlock(&coroAcceptAdapters.lock)
				return
			}
			if old.entries[idx].pc == 0 {
				break
			}
			idx = (idx + 1) & old.mask
		}
		if (old.count+1)*2 <= uintptr(len(old.entries)) {
			nCap = uintptr(len(old.entries))
		} else {
			nCap = uintptr(len(old.entries)) * 2
		}
	}
	nt := &coroAcceptTable{mask: nCap - 1, entries: make([]coroAcceptEntry, nCap)}
	if old != nil {
		for _, e := range old.entries {
			if e.pc != 0 {
				idx := coroCallableHash(e.pc) & nt.mask
				for nt.entries[idx].pc != 0 {
					idx = (idx + 1) & nt.mask
				}
				nt.entries[idx] = e
				nt.count++
			}
		}
	}
	idx := coroCallableHash(pc) & nt.mask
	for nt.entries[idx].pc != 0 {
		idx = (idx + 1) & nt.mask
	}
	nt.entries[idx] = coroAcceptEntry{pc: pc, step: sf.data}
	nt.count++
	coroAcceptAdapters.table.Store(nt)
	unlock(&coroAcceptAdapters.lock)
}

// coroLookupAccept resolves a net.Listener.Accept interface call to a registered
// direct netpoll accept step adapter (lock-free).
func coroLookupAccept(itab, data unsafe.Pointer, methodIdx uintptr) (step, recv unsafe.Pointer) {
	tbl := coroAcceptAdapters.table.Load()
	if tbl == nil || itab == nil {
		return nil, nil
	}
	tab := (*abi.ITab)(itab)
	fnPC := *(*uintptr)(add(unsafe.Pointer(&tab.Fun[0]), methodIdx*unsafe.Sizeof(uintptr(0))))
	if fnPC == 0 {
		return nil, nil
	}
	idx := coroCallableHash(fnPC) & tbl.mask
	for {
		e := tbl.entries[idx]
		if e.pc == fnPC {
			return e.step, data
		}
		if e.pc == 0 {
			return nil, nil
		}
		idx = (idx + 1) & tbl.mask
	}
}

// coroSpawnOpaque spawns an ordinary Go func() that could not be resolved to a
// lowered coroutine factory, preserving full Go semantics seamlessly.
func coroSpawnOpaque(ctx unsafe.Pointer, fn func()) bool {
	go fn()
	return false
}
