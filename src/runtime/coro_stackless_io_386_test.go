// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && linux

package runtime

import "unsafe"

// Check the 32-bit layout even when the test binary can only be cross-compiled.
// Both bounds must be non-negative, so the sizes must match exactly.
var (
	_ [76 - unsafe.Sizeof(stacklessCoroOperation{})]byte
	_ [unsafe.Sizeof(stacklessCoroOperation{}) - 76]byte
	_ [144 - unsafe.Sizeof(stacklessCoroChannelOperation{})]byte
	_ [unsafe.Sizeof(stacklessCoroChannelOperation{}) - 144]byte
	_ [108 - unsafe.Sizeof(stacklessCoroIOOperation{})]byte
	_ [unsafe.Sizeof(stacklessCoroIOOperation{}) - 108]byte
)
