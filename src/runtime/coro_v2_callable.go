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
// compiler-generated coroutine factory funcval.
type coroCallableEntry struct {
	pc      uintptr
	factory unsafe.Pointer
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

func init() {
	lockInit(&coroCallables.lock, lockRankCoroSched)
}

func coroCallableHash(pc uintptr) uintptr {
	x := pc ^ (pc >> 16)
	x *= 0x45d9f3b
	return x ^ (x >> 16)
}

func coroLookupPC(pc uintptr) unsafe.Pointer {
	tbl := coroCallables.table.Load()
	if tbl == nil || pc == 0 {
		return nil
	}
	idx := coroCallableHash(pc) & tbl.mask
	for {
		e := tbl.entries[idx]
		if e.pc == pc {
			return e.factory
		}
		if e.pc == 0 {
			return nil
		}
		idx = (idx + 1) & tbl.mask
	}
}

// coroRegisterCallable registers a lowered coroutine factory for dynamic
// function / interface dispatch (called from compiler-emitted package init).
// fn must be a non-nil Go func value (direct-iface: ef.data == *funcval);
// factory must be a non-nil Go func value of the typed adapter.
func coroRegisterCallable(fn any, factory any) {
	ef := efaceOf(&fn)
	ff := efaceOf(&factory)
	if ef == nil || ef.data == nil || ff == nil || ff.data == nil {
		return
	}
	fv := (*funcval)(ef.data)
	if fv == nil || fv.fn == 0 {
		return
	}
	facPtr := ff.data
	pc := fv.fn

	lock(&coroCallables.lock)
	old := coroCallables.table.Load()
	var nCap uintptr = 64
	if old != nil {
		// Check update-in-place or grow at 50% load factor.
		idx := coroCallableHash(pc) & old.mask
		for {
			if old.entries[idx].pc == pc {
				old.entries[idx].factory = facPtr
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
	// Copy-on-write so lock-free readers always see a consistent snapshot.
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
	coroCallableInsert(nt, coroCallableEntry{pc: pc, factory: facPtr})
	coroCallables.table.Store(nt)
	unlock(&coroCallables.lock)
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
	fac := coroLookupPC(fv.fn)
	if fac == nil {
		return nil, nil
	}
	return fac, unsafe.Pointer(fv)
}

// coroLookupInterface resolves a dynamic interface method call (itab.Fun[methodIdx])
// to its coroutine factory funcval and concrete receiver data pointer (lock-free).
func coroLookupInterface(itab, data unsafe.Pointer, methodIdx uintptr) (factory unsafe.Pointer, receiver unsafe.Pointer) {
	if itab == nil {
		return nil, nil
	}
	tab := (*abi.ITab)(itab)
	fnPC := *(*uintptr)(add(unsafe.Pointer(&tab.Fun[0]), methodIdx*unsafe.Sizeof(uintptr(0))))
	fac := coroLookupPC(fnPC)
	if fac == nil {
		return nil, nil
	}
	return fac, data
}

// coroSpawnOpaque spawns an ordinary Go func() that could not be resolved to a
// lowered coroutine factory, preserving full Go semantics seamlessly.
func coroSpawnOpaque(ctx unsafe.Pointer, fn func()) bool {
	go fn()
	return false
}
