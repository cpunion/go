// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

const (
	StacklessCoroActionInvalid  = stacklessCoroActionInvalid
	StacklessCoroActionYield    = stacklessCoroActionYield
	StacklessCoroActionWait     = stacklessCoroActionWait
	StacklessCoroActionComplete = stacklessCoroActionComplete
)

func RunStacklessCoroForTest(resume func(unsafe.Pointer) uint8) {
	coroRun(resume)
}

func AwaitStacklessCoroForTest(ctx unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroAwait(ctx, resume)
}

func SpawnStacklessCoroForTest(ctx unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroSpawn(ctx, resume)
}
