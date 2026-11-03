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
	words := efaceOf(&value)
	if words._type == nil || words._type.Kind_&abi.KindMask != abi.Func || words.data == nil {
		return nil, nil
	}
	return coroLookupCallable((*funcval)(words.data).fn, unsafe.Pointer(words._type)), words.data
}
