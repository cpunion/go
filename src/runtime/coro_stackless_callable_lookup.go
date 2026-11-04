// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import (
	"internal/abi"
	"unsafe"
)

// The compiler boxes an unnamed signature, so named source function types
// match the companion's canonical callable signature. Nil function values
// remain an ordinary fallback; lookup itself must not trigger their panic.
func coroLookupFunction(value any) (unsafe.Pointer, unsafe.Pointer) {
	entry, environment := lookupStacklessCoroFunction(value, false)
	return unsafe.Pointer(entry), environment
}

func lookupStacklessCoroFunction(value any, deferred bool) (*stacklessCoroCallable, unsafe.Pointer) {
	words := efaceOf(&value)
	if words._type == nil || words._type.Kind() != abi.Func || words.data == nil {
		return nil, nil
	}
	entry := loadStacklessCoroCallable((*funcval)(words.data).fn)
	if entry == nil || entry.signature != words._type {
		return nil, nil
	}
	if entry.boundMethod != 0 {
		// Only an explicitly recorded interface method-value wrapper has this
		// marker. Its environment is the unchanged {code, interface} Go layout.
		receiver := (*iface)(add(words.data, unsafe.Sizeof(uintptr(0))))
		entry = lookupStacklessCoroInterface(unsafe.Pointer(receiver.tab), receiver.data, entry.boundMethod-1, value, deferred)
		return entry, receiver.data
	}
	return checkStacklessCoroCallable(entry, unsafe.Pointer(words._type), deferred), words.data
}

func coroLookupInterface(table, environment unsafe.Pointer, method uintptr, signature any) (unsafe.Pointer, unsafe.Pointer) {
	return unsafe.Pointer(lookupStacklessCoroInterface(table, environment, method, signature, false)), environment
}

func lookupStacklessCoroInterface(table, environment unsafe.Pointer, method uintptr, signature any, deferred bool) *stacklessCoroCallable {
	tab := (*itab)(table)
	typ := efaceOf(&signature)._type
	if tab == nil || tab.Inter == nil || method >= uintptr(len(tab.Inter.Methods)) || typ == nil || typ.Kind() != abi.Func {
		return nil
	}
	code := *(*uintptr)(add(unsafe.Pointer(&tab.Fun[0]), method*unsafe.Sizeof(uintptr(0))))
	entry := loadStacklessCoroCallable(code)
	if entry == nil {
		return nil
	}
	entry = checkStacklessCoroCallable(entry.method, unsafe.Pointer(typ), deferred)
	// A nil pointer receiver must execute the original wrapper's adjustment
	// or panicwrap in the called activation, including a new go child. Bypass
	// it only after proving the receiver exists, preserving panic ownership
	// and the public panic text.
	if entry != nil && entry.nilCheck != 0 && environment == nil {
		return nil
	}
	return entry
}
