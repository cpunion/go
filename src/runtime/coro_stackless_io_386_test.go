// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && linux

package runtime

import "unsafe"

// Check the 32-bit layout even when the test binary can only be cross-compiled.
// Both bounds must be non-negative, so the sizes must match exactly.
var (
	_ [44 - unsafe.Sizeof(stacklessCoroOperation{})]byte
	_ [unsafe.Sizeof(stacklessCoroOperation{}) - 44]byte
	_ [84 - unsafe.Sizeof(stacklessCoroChannelOperation{})]byte
	_ [unsafe.Sizeof(stacklessCoroChannelOperation{}) - 84]byte
	_ [112 - unsafe.Sizeof(stacklessCoroFullChannelOperation{})]byte
	_ [unsafe.Sizeof(stacklessCoroFullChannelOperation{}) - 112]byte
	_ [100 - unsafe.Sizeof(stacklessCoroIOOperation{})]byte
	_ [unsafe.Sizeof(stacklessCoroIOOperation{}) - 100]byte
)
