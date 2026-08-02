// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/runtime/sys"
	"unsafe"
)

func StacklessCoroNativeStackForTest() (native bool, sp, lo, hi, g0lo, g0hi uintptr) {
	gp := getg()
	return gp.stackIsFixed(), sys.GetCallerSP(), gp.stack.lo, gp.stack.hi, gp.m.g0.stack.lo, gp.m.g0.stack.hi
}

func StacklessCoroNativePoolForTest() (count int) {
	lock(&stacklessCoroNativePool.lock)
	count = stacklessCoroNativePool.count
	unlock(&stacklessCoroNativePool.lock)
	return
}

func StacklessCoroNativePoolTaskCacheForTest() (warm, overflow int) {
	available := stacklessCoroNativePool.available
	for range len(available) {
		ctx := <-available
		warm += ctx.freeTaskCount
		available <- ctx
	}
	lock(&stacklessCoroNativePool.lock)
	for ctx := stacklessCoroNativePool.overflow; ctx != nil; ctx = ctx.poolNext {
		overflow += ctx.freeTaskCount
	}
	unlock(&stacklessCoroNativePool.lock)
	return
}

func WriteStacklessCoroNativeForTest(fd int, value byte) int {
	return int(write(uintptr(fd), unsafe.Pointer(&value), 1))
}
