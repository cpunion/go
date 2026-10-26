// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package abi

// CoroFrameFresh marks an unused homogeneous frame returned by the typed
// chunk allocator. The factory consumes it before publishing the frame. The
// bit is otherwise reserved for heterogeneous suspended-frame state, which
// never uses this allocation path.
const CoroFrameFresh uint8 = 1 << 6

// CoroFrameZeroMinBytes limits conditional initialization to substantial
// zero-only payloads; small frame initializers remain straight-line stores.
const CoroFrameZeroMinBytes = 1024

// The optional 64-bit task/frame allocation has six task words. Word three
// contains scalar task state; the other five words are GC pointers. These
// describe private allocation metadata, not the frame factory ABI.
const (
	CoroTaskFrameHeaderWords       = 6
	CoroTaskFrameHeaderPointerMask = 0x37
	CoroTaskFrameMaxBytes          = 208
)
