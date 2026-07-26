// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

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

func SleepStacklessCoroForTest(ctx unsafe.Pointer, ns int64) {
	coroSleep(ctx, ns)
}

func StartSleepStacklessCoroForTest(ctx unsafe.Pointer, ns int64) uint64 {
	return startStacklessCoroTimer(ctx, ns)
}

func CancelSleepStacklessCoroForTest(id uint64) bool {
	return cancelStacklessCoroTimer(id)
}

func FileReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	coroFileRead(ctx, fd, buffer, n, errno)
}

func SocketReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, buffer []byte, n *int, errno *uintptr) {
	coroSocketRead(ctx, fd, buffer, n, errno)
}

func CallReadStacklessCoroForTest(ctx unsafe.Pointer, call func()) {
	coroCallRead(ctx, call)
}

func CheckStacklessCoroOperationRegistryForTest() bool {
	first := new(stacklessCoroOperation)
	second := new(stacklessCoroOperation)
	firstID := registerStacklessCoroOperation(first)
	secondID := registerStacklessCoroOperation(second)
	if firstID == 0 || secondID == 0 || firstID == secondID {
		return false
	}
	if findStacklessCoroOperation(firstID) != first {
		return false
	}
	if takeStacklessCoroOperation(firstID) != first {
		return false
	}
	if findStacklessCoroOperation(firstID) != nil {
		return false
	}
	if takeStacklessCoroOperation(firstID) != nil {
		return false
	}
	return takeStacklessCoroOperation(secondID) == second
}

func AsyncReadStacklessCoroForTest(ctx unsafe.Pointer, fd int, result *uint64, errno *uintptr) uint64 {
	op := startStacklessCoroAsync(ctx, fd, result, errno)
	stacklessCoroReadEnqueue(op)
	return op.id
}

func FailAsyncStacklessCoroForTest(ctx unsafe.Pointer, result *uint64, errno *uintptr, submitErr uintptr) {
	op := startStacklessCoroAsync(ctx, -1, result, errno)
	failStacklessCoroAsync(op.id, submitErr)
}
