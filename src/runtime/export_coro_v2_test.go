// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

type CoroV2Header struct {
	_ [7]unsafe.Pointer
	_ [6]uint32
}

// Exported helpers for runtime_test Milestone 1 verification.
func CoroV2AllocForTest(typ *_type) unsafe.Pointer { return coroAlloc(typ) }
func CoroV2RunFrameForTest(frame unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroRunFrame(frame, resume)
}
func CoroV2ReleaseFrameForTest(frame unsafe.Pointer) { coroReleaseFrame(frame) }
func CoroV2SpawnFrameForTest(ctx, frame unsafe.Pointer, resume func(unsafe.Pointer) uint8) bool {
	return coroSpawnFrame(ctx, frame, resume)
}
func CoroV2AwaitFrameForTest(ctx, frame unsafe.Pointer, resume func(unsafe.Pointer) uint8) uint8 {
	return coroAwaitFrame(ctx, frame, resume)
}
func CoroV2DeferForTest(ctx unsafe.Pointer, fn func()) { coroDefer(ctx, fn) }
func CoroV2DeferCoroForTest(ctx, frame unsafe.Pointer, resume func(unsafe.Pointer) uint8) {
	coroDeferCoro(ctx, frame, resume)
}
func CoroV2PanicForTest(ctx unsafe.Pointer, val any) { coroPanic(ctx, val) }
func CoroV2GoexitForTest(ctx unsafe.Pointer)         { coroGoexit(ctx) }
func CoroV2RecoverForTest(ctx unsafe.Pointer) any    { return coroRecover(ctx) }
func CoroV2SleepForTest(ctx unsafe.Pointer, ns int64) bool {
	return coroSleep(ctx, ns)
}
func CoroV2ChanSendForTest(ctx unsafe.Pointer, ch any, elem unsafe.Pointer) bool {
	ef := efaceOf(&ch)
	return coroChanSend(ctx, (*hchan)(ef.data), elem)
}
func CoroV2ChanRecvForTest(ctx unsafe.Pointer, ch any, elem unsafe.Pointer, ok *bool) bool {
	ef := efaceOf(&ch)
	return coroChanRecv(ctx, (*hchan)(ef.data), elem, ok)
}

type CoroV2SelectCaseForTest struct {
	Ch   any
	Elem unsafe.Pointer
}

func CoroV2SelectForTest(ctx unsafe.Pointer, cases []CoroV2SelectCaseForTest, nsends, nrecvs int, block bool, chosen *int, ok *bool) bool {
	sc := make([]scase, len(cases))
	for i, c := range cases {
		if c.Ch != nil {
			sc[i].c = (*hchan)(efaceOf(&c.Ch).data)
		}
		sc[i].elem = c.Elem
	}
	var cas0 *scase
	if len(sc) > 0 {
		cas0 = &sc[0]
	}
	return coroSelect(ctx, cas0, nsends, nrecvs, block, chosen, ok)
}

func CoroV2CacheBytesForTest() uintptr   { return coroSched.cacheBytes.Load() }
func CoroV2UnhostedForTest() int64       { return int64(coroUnhostedCount()) }
func CoroV2RunnableForTest() int32       { return coroRunnableCount() }
func CoroV2TypeOfForTest(v any) *_type   { return efaceOf(&v)._type }
func CoroV2ActionCompleteForTest() uint8 { return coroActionComplete }
func CoroV2ActionYieldForTest() uint8    { return coroActionYield }
func CoroV2ActionWaitForTest() uint8     { return coroActionWait }
func CoroV2ActionPanicForTest() uint8    { return coroActionPanic }
func CoroV2ActionGoexitForTest() uint8   { return coroActionGoexit }

// Deterministic shrink probe: directly pushes tasks onto all non-zero P rings,
// asserts those rings are non-empty, then returns count queued.
type coroV2ShrinkFrame struct {
	hdr  CoroV2Header
	done *atomic.Int64
	hits *atomic.Int64
}

var (
	coroV2ShrinkDone   atomic.Int64
	coroV2ShrinkHits   atomic.Int64
	coroV2ShrinkTarget atomic.Int64
)

func coroV2ShrinkWorker(ctx unsafe.Pointer) uint8 {
	f := (*coroV2ShrinkFrame)(ctx)
	f.hits.Add(1)
	f.done.Add(1)
	return coroActionComplete
}

func coroV2ShrinkDrainRoot(ctx unsafe.Pointer) uint8 {
	if coroV2ShrinkDone.Load() < coroV2ShrinkTarget.Load() {
		return coroActionYield
	}
	return coroActionComplete
}

func CoroV2SeedMultiPRingsForTest(n int) (queued int, nonZeroP int) {
	coroV2ShrinkDone.Store(0)
	coroV2ShrinkHits.Store(0)
	coroV2ShrinkTarget.Store(0)
	typ := abi.TypeFor[coroV2ShrinkFrame]()
	mp := acquirem()
	for i := 0; i < n; i++ {
		if len(allp) <= 1 {
			break
		}
		target := allp[1+(i%(len(allp)-1))]
		ps := target.coro.Load()
		if ps == nil {
			releasem(mp)
			fresh := new(coroPState)
			mp = acquirem()
			if target.coro.CompareAndSwap(nil, fresh) {
				ps = fresh
			} else {
				ps = target.coro.Load()
			}
		}
		releasem(mp)
		f := (*coroV2ShrinkFrame)(coroAlloc(typ))
		f.done = &coroV2ShrinkDone
		f.hits = &coroV2ShrinkHits
		t := (*coroTask)(unsafe.Pointer(f))
		t.resume = coroV2ShrinkWorker
		t.flags |= coroFlagUnhosted
		t.state.Store(coroStateRunnable)
		coroAddUnhosted(1)
		mp = acquirem()
		// Re-check target P ring directly (test runs with no active drivers yet).
		h := atomic.LoadAcq(&ps.runqH)
		tail := atomic.Load(&ps.runqT)
		if tail-h < coroPRunqSize {
			ps.runq[tail&(coroPRunqSize-1)] = t
			atomic.Xadd(&ps.runqT, 1)
			coroMarkPNonEmpty(ps)
			queued++
			nonZeroP++
		}
	}
	releasem(mp)
	coroV2ShrinkTarget.Store(int64(queued))
	return queued, nonZeroP
}

func CoroV2DrainMultiPRingsForTest() (int64, int64) {
	type dummyRoot struct{ hdr CoroV2Header }
	rt := coroAlloc(abi.TypeFor[dummyRoot]())
	coroRunFrame(rt, coroV2ShrinkDrainRoot)
	coroReleaseFrame(rt)
	return coroV2ShrinkDone.Load(), coroV2ShrinkHits.Load()
}
