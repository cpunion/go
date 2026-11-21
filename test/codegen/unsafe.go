// asmcheck

// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codegen

import "unsafe"

func f(p unsafe.Pointer, x, y uintptr) int64 {
	p = unsafe.Pointer(uintptr(p) + x + y)
	// amd64:`MOVQ \(.*\)\(.*\*1\), `
	// arm64:`MOVD \(R[0-9]+\)\(R[0-9]+\), `
	return *(*int64)(p)
}

func loadUint16Before(p unsafe.Pointer) uint16 {
	// arm64:`MOVHU\t-6\(R[0-9]+\),`
	return *(*uint16)(unsafe.Add(p, -6))
}

func loadInt64Before(p unsafe.Pointer) int64 {
	// arm64:`MOVD\t-16\(R[0-9]+\),`
	return *(*int64)(unsafe.Add(p, -16))
}
