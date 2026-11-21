// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

func stacklessCoroPublicCaller(gp *g) *g { return gp }

func stacklessCoroCanPairChannel(*g, *stacklessCoroScheduler) bool { return false }

func yieldStacklessCoroNative(*stacklessCoroScheduler, *stacklessCoroTask) bool {
	Gosched()
	return false
}

type stacklessCoroNativeDriver struct{}

type stacklessCoroDrainCacheAge struct{}

func (*stacklessCoroDrainCacheAge) complete() {}

func (*stacklessCoroNativeDriver) run(*stacklessCoroScheduler, bool, bool) *stacklessCoroScheduler {
	return nil
}

func (*stacklessCoroNativeDriver) close(*stacklessCoroScheduler, bool) {}

func cacheStacklessCoroDrainedTasks(*stacklessCoroScheduler) {}

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

func hasStacklessCoroNativeFusedChunk(stacklessCoroResume, uintptr, *_type) bool {
	return false
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

func stacklessCoroDeferPrivateSleep(*stacklessCoroContext, int64) bool { return false }

func stacklessCoroPrivateSleep(*stacklessCoroScheduler, *stacklessCoroTask) (resume, suspend bool) {
	return false, false
}

func stacklessCoroDeferSleep(*stacklessCoroScheduler,
	*stacklessCoroOperation, int64) bool {
	return false
}
