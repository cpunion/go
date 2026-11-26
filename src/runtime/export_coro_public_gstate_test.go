// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

func CheckStacklessCoroPublicGStateForTest() string {
	gp := getg()
	if gp.stacklessCoro != nil {
		return "ordinary test goroutine already has stacklessCoro state"
	}
	pub := acquireStacklessCoroPublicState(gp)
	if pub == nil || stacklessCoroPublicState(gp) != pub || gp.stackIsFixed() {
		return "acquireStacklessCoroPublicState did not install tagged public state"
	}
	// Verify private public record lifecycle and STW enumeration visibility.
	outer := &stacklessCoroScheduler{wake: make(chan struct{}, stacklessCoroWarmExecutorCount)}
	lockInit(&outer.lock, lockRankStacklessCoro)
	r1 := &stacklessCoroGoroutine{
		root:          &outer.root,
		leaf:          &outer.root,
		scheduler:     outer,
		publicG:       gp,
		publicState:   pub,
		goid:          gp.goid,
		public:        true,
		profileCaller: make([]uintptr, 0, maxCPUProfStack),
	}
	outer.root.setSource(r1)
	pub.publicRecord.Store(r1)

	seen := false
	stw := stopTheWorld(stwGoroutineProfile)
	visitStacklessCoroGoroutines(func(r *stacklessCoroGoroutine) {
		if r == r1 {
			seen = true
		}
	})
	startTheWorld(stw)
	if !seen {
		pub.publicRecord.Store(nil)
		outer.root.setSource(nil)
		return "visitStacklessCoroGoroutines missed private public root record"
	}
	if !stacklessCoroHasActiveGoroutines() {
		pub.publicRecord.Store(nil)
		outer.root.setSource(nil)
		return "stacklessCoroHasActiveGoroutines returned false with active private public record"
	}

	// Completing the outer private root must clear publicRecord, clear root.source(),
	// and recycle r1 into pub.publicFree without touching global records.
	baseRecords := stacklessCoroGoroutines.records.Load()
	stacklessCoroCompleteLogicalFrame(&outer.root)
	if pub.publicRecord.Load() != nil {
		return "private publicRecord was not cleared on completion"
	}
	if outer.root.source() != nil {
		return "root.source() was not cleared on completion"
	}
	if pub.publicFree != r1 {
		return "completed private public record was not recycled into pub.publicFree"
	}
	if stacklessCoroGoroutines.records.Load() != baseRecords {
		return "private public root completion mutated global records count"
	}

	// Verify nested public root fallback: with pub.publicRecord occupied, a nested
	// registration falls back to the global registry (inRegistry == true).
	*r1 = stacklessCoroGoroutine{
		root:          &outer.root,
		leaf:          &outer.root,
		scheduler:     outer,
		publicG:       gp,
		publicState:   pub,
		goid:          gp.goid,
		public:        true,
		profileCaller: r1.profileCaller[:0],
	}
	pub.publicFree = nil
	pub.publicRecord.Store(r1)
	outer.root.setSource(r1)
	inner := &stacklessCoroScheduler{wake: make(chan struct{}, stacklessCoroWarmExecutorCount)}
	lockInit(&inner.lock, lockRankStacklessCoro)
	lock(&stacklessCoroGoroutines.lock)
	r2 := inner.registerPublicGoroutineLocked(gp, &inner.root, []uintptr{})
	unlock(&stacklessCoroGoroutines.lock)
	if !r2.inRegistry || pub.publicRecord.Load() != r1 {
		stacklessCoroCompleteLogicalFrame(&inner.root)
		stacklessCoroCompleteLogicalFrame(&outer.root)
		return "nested public root did not stay isolated in global registry"
	}
	seenOuter, seenInner := false, false
	stw = stopTheWorld(stwGoroutineProfile)
	visitStacklessCoroGoroutines(func(r *stacklessCoroGoroutine) {
		if r == r1 {
			seenOuter = true
		}
		if r == r2 {
			seenInner = true
		}
	})
	startTheWorld(stw)
	if !seenOuter || !seenInner {
		stacklessCoroCompleteLogicalFrame(&inner.root)
		stacklessCoroCompleteLogicalFrame(&outer.root)
		return "visitStacklessCoroGoroutines missed outer private or inner nested public root"
	}
	stacklessCoroCompleteLogicalFrame(&inner.root)
	if pub.publicRecord.Load() != r1 {
		stacklessCoroCompleteLogicalFrame(&outer.root)
		return "completing nested public root clobbered outer private publicRecord"
	}
	stacklessCoroCompleteLogicalFrame(&outer.root)
	if pub.publicRecord.Load() != nil {
		return "outer private publicRecord remained non-nil after cleanup"
	}
	pub.publicFree = nil
	stacklessCoroClearGState(gp)
	if stacklessCoroPublicState(gp) != nil {
		return "stacklessCoroClearGState did not clear public state"
	}
	_ = unsafe.Pointer(pub)
	return ""
}
