// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

type stacklessCoroNativeDriver struct{}

func (*stacklessCoroNativeDriver) run(*stacklessCoroScheduler, bool, bool) *stacklessCoroScheduler {
	return nil
}

func (*stacklessCoroNativeDriver) close(*stacklessCoroScheduler, bool) {}

func (*stacklessCoroNativeDriver) waitForWork(s *stacklessCoroScheduler) bool {
	return s.waitForWork()
}

func coroRunOnNativeStack(*stacklessCoroScheduler) *stacklessCoroScheduler {
	return nil
}

func takeStacklessCoroNativeSelfFrame(stacklessCoroResume,
	uintptr, *_type, bool) unsafe.Pointer {
	return nil
}

func takeStacklessCoroNativeFusedChunk(stacklessCoroResume,
	uintptr, *_type) unsafe.Pointer {
	return nil
}

func cacheStacklessCoroNativeSelfFrame(stacklessCoroResume, unsafe.Pointer,
	uint8) {
}

//go:nosplit
func stacklessCoroNativeSchedulerFor(*g) *stacklessCoroScheduler {
	return nil
}

func stacklessCoroDeferSleep(*stacklessCoroScheduler,
	*stacklessCoroOperation) bool {
	return false
}
