// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux) && (race || !((darwin && arm64) || (linux && amd64)))

package runtime

import "unsafe"

func StacklessCoroClearNativeTaskCachesForTest() int { return 0 }

func StacklessCoroClearCurrentRootCacheForTest(unsafe.Pointer) {}

func StacklessCoroNativePoolSelfFrameCacheForTest() (warm, overflow,
	perContextLimit, chunkLimit uintptr) {
	return 0, 0, 0, 0
}
