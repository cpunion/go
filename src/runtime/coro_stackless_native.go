// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"internal/runtime/sys"
	"unsafe"
)

const (
	// Bound total fixed descriptor storage. Active and idle-stack holders
	// consume slots from the same pool; overflow descriptors are heap allocated.
	stacklessCoroSchedulerDescriptorCacheSize = 64

	stacklessCoroSelfFrameChunkCacheSize = 256 << 10
	// Large frames may reuse a burst between collections. The first idle GC
	// trims this to the ordinary budget; the second drops all idle payload.
	// A global quota also bounds bursts on active contexts. Parking a root
	// immediately trims its completed chunks and returns its reservation.
	// Bounded parked-cache slots let GC expire storage during longer waits.
	stacklessCoroSelfFrameBurstCacheSize = 4 << 20
	stacklessCoroSelfFrameBurstMinSize   = 1024

	// coroNativeStart runs in a frame entered by mcall on the original g0
	// stack. Leave that frame and its caller above the executor's synthetic
	// stack root.
	stacklessCoroBootstrapReserve = 4 << 10
)

type stacklessCoroNativeContext struct {
	gState              stacklessCoroGState
	scheduler           *stacklessCoroScheduler
	freeTasks           *stacklessCoroTask
	selfFrames          unsafe.Pointer
	selfFrameChunks     unsafe.Pointer
	selfFrameResume     *funcval
	selfFrameChunkType  *_type
	selfFrameSize       uintptr
	selfFrameChunkSize  uintptr
	freePlainTaskCount  uint16
	freeFrameBytes      uint16
	cachedFrameTasks    uint16
	cachedFrameBytes    uint16
	selfFrameChunkCount uint16
	selfFrameCount      uint8
	selfFrameBurst      bool // occupies padding before cacheIdleCycle
	cacheIdleCycle      uint32
	selfFrameMissResume *funcval
	selfFrameMissType   *_type
	selfFrameMissSize   uintptr
	selfFrameMissRoots  uint8
	selfFrameMissedRoot bool
	selfFrameCacheUsed  bool
	schedulerG          *g
	executor            *g
	caller              *g
	nativeG0            *g
	lockedG             guintptr
	lockedInt           uint32
	g0Accurate          bool
	initialRoot         bool
	public              bool
	deferredYield       bool
	poolNext            *stacklessCoroNativeContext
	// deferredSleep is owned by the driver between adjacent fixed-stack
	// episodes. Keeping it here avoids growing the shared logical scheduler.
	deferredSleep     *stacklessCoroOperation
	deferredSleepWhen int64
}

// stacklessCoroNativeDriver owns one native context across the fixed-stack
// episodes of a public root. Replacement executors use a driver for one
// episode and park on their ordinary G between activations.
type stacklessCoroNativeDriver struct {
	context *stacklessCoroNativeContext
	root    bool
}

// This stamp occupies the scheduler's existing scalar padding. Completion,
// rather than delayed owner teardown, starts the idle cache generation.
type stacklessCoroDrainCacheAge struct{ cycle uint32 }

func (age *stacklessCoroDrainCacheAge) complete() { age.cycle = work.cycles.Load() }

// At most four contexts, active or idle, may exceed the ordinary chunk budget.
// Other active contexts retain the existing256KiB limit.
var stacklessCoroNativeBurstContexts atomic.Uint32

// Registered contexts have no running native episode. Their ordinary driver
// removes them before resuming native work. Only this bounded set retains
// completed frame storage across a wait; the GC can age it as idle storage.
var stacklessCoroNativeParkedCaches [stacklessCoroWarmExecutorCount]atomic.Pointer[stacklessCoroNativeContext]

// Parked contexts keep their executor identity, but need no scheduler stack.
// Completed warm contexts retain their stack for the ordinary fast path; this
// separate bounded pool serves contexts resuming after an ordinary wait. GC
// drops these detached stacks; recently completed warm contexts keep theirs
// until their payload cache expires after a full idle generation.
var stacklessCoroNativeSchedulerStacks [stacklessCoroWarmExecutorCount]atomic.Pointer[g]

// Native scheduler Gs carry no goroutine activation while free. As with
// runtime's global g0, fixed descriptor storage is a typed global GC root.
// The64 descriptors serve active and retained idle stacks as well as empty
// slots; overflow uses ordinary heap descriptors. A free bit owns its G until
// stack initialization, and is published only after all saved state is cleared.
var stacklessCoroNativeSchedulerDescriptors struct {
	free  atomic.Uint64
	slots [stacklessCoroSchedulerDescriptorCacheSize]g
}

var stacklessCoroNativePool struct {
	lock  mutex
	count int
	// slots preserve the common lock-free reuse path. Contexts beyond the
	// warm capacity remain reusable through overflow.
	slots    [stacklessCoroWarmExecutorCount]atomic.Pointer[stacklessCoroNativeContext]
	overflow *stacklessCoroNativeContext
}

func init() {
	lockInit(&stacklessCoroNativePool.lock, lockRankStacklessCoro)
	stacklessCoroNativeSchedulerDescriptors.free.Store(^uint64(0))
}

// run drives one episode on a fixed portion of the current operating system
// thread stack. It returns s reloaded from the heap context after a successful
// stack switch; callers must not reuse their pre-switch local. The race
// runtime has its own stack and goroutine bookkeeping, so race builds retain
// the managed-stack driver for now.
func (d *stacklessCoroNativeDriver) run(s *stacklessCoroScheduler,
	initial, public bool) *stacklessCoroScheduler {
	if raceenabled {
		return nil
	}
	if d.context == nil {
		// A private public driver has one executor and no replacements.
		// It may acquire another context after returning one before a wait.
		d.root = s.executorState.Load() == stacklessCoroExecutorStateOff &&
			s.executorCount.Load() == 1
		d.context = acquireStacklessCoroNativeContext()
		// Only a new scheduler can inherit the context's task cache. A
		// resumed scheduler still owns plain slots and active/reserved frame
		// charges, even if its previous context was returned to the pool.
		if initial && d.root && d.context.freeTasks != nil {
			s.freeTasks = d.context.freeTasks
			s.freePlainTaskCount = d.context.freePlainTaskCount
			s.freeFrameBytes = d.context.freeFrameBytes
			s.cachedFrameTasks = d.context.cachedFrameTasks
			s.cachedFrameBytes = d.context.cachedFrameBytes
			d.context.freeTasks = nil
			d.context.freePlainTaskCount = 0
			d.context.freeFrameBytes = 0
			d.context.cachedFrameTasks = 0
			d.context.cachedFrameBytes = 0
		}
	}

	ctx := d.context
	gp := getg()
	if gp != gp.m.curg || gp == gp.m.g0 || gp == gp.m.gsignal {
		return nil
	}
	if gp.param != nil {
		throw("runtime: stackless coroutine caller has pending parameter")
	}
	if ctx.schedulerG == nil {
		ctx.schedulerG = acquireStacklessCoroNativeSchedulerStack()
	}

	ctx.scheduler = s
	ctx.caller = gp
	ctx.initialRoot = initial
	ctx.public = public
	gp.param = unsafe.Pointer(ctx)
	mcall(coroNativeStart)
	mp := gp.m
	if mp == nil || mp.curg != gp || mp.g0 != ctx.schedulerG || ctx.schedulerG.m != mp || ctx.nativeG0 == nil {
		throw("runtime: invalid stackless coroutine native return")
	}
	mp.g0 = ctx.nativeG0
	mp.g0StackAccurate = ctx.g0Accurate
	ctx.schedulerG.m = nil
	ctx.nativeG0 = nil
	ctx.lockedG = 0
	ctx.lockedInt = 0
	ctx.g0Accurate = false

	if gp.param != unsafe.Pointer(ctx) {
		throw("runtime: lost stackless coroutine native context")
	}
	gp.param = nil
	scheduler := ctx.scheduler
	ctx.scheduler = nil
	ctx.caller = nil
	return scheduler
}

func (d *stacklessCoroNativeDriver) waitForWork(s *stacklessCoroScheduler) bool {
	ctx := d.context
	// run restored m.g0 and detached this scheduler G before returning. No
	// native activation or saved continuation can use its stack while we wait.
	releaseStacklessCoroNativeSchedulerStack(ctx)
	if ctx.deferredYield {
		ctx.deferredYield = false
		// The public lineage is already queued and signaled. Yield only after
		// restoring the ordinary caller and releasing the native thread lock.
		// A private root also returns its inactive context before yielding;
		// otherwise concurrent runnable roots each retain an executor identity
		// before any of them reaches its channel wait.
		d.releasePrivateWaitContext(s)
		Gosched()
		return s.waitForWork()
	}
	// No native episode is running on this context while its driver waits.
	// Do not retain a completed burst for the lifetime of a parked root.
	if ctx.selfFrameBurst {
		trimStacklessCoroNativeSelfFrameChunks(ctx)
		releaseStacklessCoroNativeBurst(ctx)
	}
	if ctx.selfFrames != nil || ctx.selfFrameChunks != nil ||
		(d.root && s.executorState.Load() == stacklessCoroExecutorStateOff && s.freeFrameBytes != 0) {
		return d.waitWithCachedFrames(s)
	}
	if d.releasePrivateWaitContext(s) {
		return s.waitForWork()
	}
	return d.waitForOperation(s)
}

// releasePrivateWaitContext returns an inactive context whose public root can
// wait without it. Deferred work and shared/foreign executors keep their
// existing driver. No caller or defer may touch ctx after this succeeds.
func (d *stacklessCoroNativeDriver) releasePrivateWaitContext(s *stacklessCoroScheduler) bool {
	ctx := d.context
	if !d.root || !ctx.public ||
		s.executorState.Load() != stacklessCoroExecutorStateOff ||
		ctx.deferredYield || ctx.deferredSleep != nil {
		return false
	}
	ctx.scheduler = nil
	d.context = nil
	releaseStacklessCoroNativeContext(ctx)
	return true
}

// A context is published only after its native episode has ended, and is
// unpublished before the next episode can mutate its cache. GC may clear its
// unused storage while the ordinary driver waits. It must keep the slot itself
// intact until that driver removes it. Overflow drops storage immediately.
func registerStacklessCoroNativeParkedCache(ctx *stacklessCoroNativeContext) *atomic.Pointer[stacklessCoroNativeContext] {
	ctx.cacheIdleCycle = work.cycles.Load()
	for i := range stacklessCoroNativeParkedCaches {
		slot := &stacklessCoroNativeParkedCaches[i]
		if slot.CompareAndSwap(nil, ctx) {
			return slot
		}
	}
	clearStacklessCoroNativeParkedCache(ctx)
	return nil
}

func (d *stacklessCoroNativeDriver) waitWithCachedFrames(s *stacklessCoroScheduler) bool {
	ctx := d.context
	leased := d.root && s.executorState.Load() == stacklessCoroExecutorStateOff && s.freeFrameBytes != 0
	if leased {
		// Reuse the inactive episode link as an exclusive parked-cache lease.
		// No other driver can start an episode on this scheduler while we wait.
		// Unpublish the registry slot before clearing the lease and resuming.
		ctx.scheduler = s
	}
	slot := registerStacklessCoroNativeParkedCache(ctx)
	if slot == nil && d.releasePrivateWaitContext(s) {
		// Overflow already dropped the idle cache. Clear its lease before
		// pooling, and install no defer that could clear a later borrower's
		// scheduler link through the released context pointer.
		return s.waitForWork()
	}
	if leased {
		defer func() { ctx.scheduler = nil }()
	}
	if slot != nil {
		defer slot.Store(nil)
	}
	return d.waitForOperation(s)
}

func clearStacklessCoroNativeParkedCache(ctx *stacklessCoroNativeContext) {
	if s := ctx.scheduler; s != nil {
		if ctx.schedulerG != nil || ctx.caller != nil ||
			s.executorState.Load() != stacklessCoroExecutorStateOff {
			throw("runtime: invalid parked stackless coroutine cache lease")
		}
		s.discardIdleFrameTasks()
	}
	clearStacklessCoroNativePayloadCache(ctx)
}

func (d *stacklessCoroNativeDriver) waitForOperation(s *stacklessCoroScheduler) bool {
	ctx := d.context
	op := ctx.deferredSleep
	if op == nil {
		return s.waitForWork()
	}
	when := ctx.deferredSleepWhen
	ctx.deferredSleep = nil
	ctx.deferredSleepWhen = 0

	if when > nanotime() {
		armStacklessCoroTimerAt(op, when)
		return s.waitForWork()
	}
	// A positive sleep remains a scheduling point after the fixed-stack
	// teardown has already consumed its deadline.
	Gosched()

	scheduler, task := clearStacklessCoroOperation(op)
	if scheduler != s {
		throw("runtime: deferred sleep changed scheduler")
	}
	lock(&s.lock)
	s.readyLocked(task)
	cachedLocally := s.recycleOperationLocked(op)
	unlock(&s.lock)
	if !cachedLocally {
		cacheStacklessCoroSharedOperation(op)
	}
	return true
}

// close releases the native context. A completed public root returns its
// bounded task cache to that context only after its final episode.
func (d *stacklessCoroNativeDriver) close(s *stacklessCoroScheduler, rootComplete bool) {
	ctx := d.context
	if ctx == nil {
		return
	}
	if d.root && rootComplete &&
		s.executorState.Load() == stacklessCoroExecutorStateOff &&
		s.reservedTasks == nil && s.freeFrameBytes == s.cachedFrameBytes &&
		s.freeTasks != nil {
		s.discardFreeOverflowTasks()
		s.discardFreeBatchedTasks()
		ctx.freeTasks = s.freeTasks
		ctx.freePlainTaskCount = s.freePlainTaskCount
		ctx.freeFrameBytes = s.freeFrameBytes
		ctx.cachedFrameTasks = s.cachedFrameTasks
		ctx.cachedFrameBytes = s.cachedFrameBytes
		s.freeTasks = nil
		s.freePlainTaskCount = 0
		s.freeFrameBytes = 0
		s.cachedFrameTasks = 0
		s.cachedFrameBytes = 0
	}
	releaseStacklessCoroNativeContext(ctx)
	d.context = nil
	d.root = false
}

// Spawned schedulers cannot give their cache to the public driver when it
// hands off to the owner: detached children may still be using it. Once the
// owner has joined all replacements, the same bounded native pool can retain
// its completed holders. No native activation or saved continuation is moved.
func cacheStacklessCoroDrainedTasks(s *stacklessCoroScheduler) {
	if raceenabled || !s.spawned() || !s.drained() || s.freeTasks == nil ||
		s.reservedTasks != nil || s.freeFrameBytes != s.cachedFrameBytes {
		return
	}
	s.discardFreeOverflowTasks()
	s.discardFreeBatchedTasks()
	if s.freeTasks == nil {
		return
	}
	ctx := acquireStacklessCoroNativeContext()
	// GC may run between drainage, joining the replacements and acquiring a
	// context. Do not rejuvenate the completed holders at publication. Keep
	// the final age check and cache publication in one non-preemptible region;
	// the release path does not allocate, including on a full warm pool.
	mp := acquirem()
	cycle := work.cycles.Load()
	age := s.drainCacheAge.cycle
	if cycle-age < 2 {
		ctx.freeTasks = s.freeTasks
		ctx.freePlainTaskCount = s.freePlainTaskCount
		ctx.freeFrameBytes = s.freeFrameBytes
		ctx.cachedFrameTasks = s.cachedFrameTasks
		ctx.cachedFrameBytes = s.cachedFrameBytes
	} else {
		// The completed prefix has already spent its two idle collections on
		// the old scheduler. Preserve any cache the borrowed context had.
		age = cycle
	}
	s.freeTasks = nil
	s.freePlainTaskCount = 0
	s.freeFrameBytes = 0
	s.cachedFrameTasks = 0
	s.cachedFrameBytes = 0
	releaseStacklessCoroNativeContextAt(ctx, age)
	releasem(mp)
}

// coroRunOnNativeStack drives one replacement episode with a short-lived
// driver. Public roots keep their driver across idle episodes instead.
func coroRunOnNativeStack(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	var native stacklessCoroNativeDriver
	scheduler := native.run(s, false, false)
	if scheduler != nil {
		native.close(scheduler, false)
	} else {
		native.close(s, false)
	}
	return scheduler
}

func acquireStacklessCoroNativeContext() *stacklessCoroNativeContext {
	for i := range stacklessCoroNativePool.slots {
		slot := &stacklessCoroNativePool.slots[i]
		ctx := slot.Load()
		if ctx != nil && slot.CompareAndSwap(ctx, nil) {
			ctx.selfFrameMissedRoot = false
			ctx.selfFrameCacheUsed = false
			return ctx
		}
	}
	lock(&stacklessCoroNativePool.lock)
	if ctx := stacklessCoroNativePool.overflow; ctx != nil {
		stacklessCoroNativePool.overflow = ctx.poolNext
		ctx.poolNext = nil
		unlock(&stacklessCoroNativePool.lock)
		return ctx
	}
	stacklessCoroNativePool.count++
	unlock(&stacklessCoroNativePool.lock)
	return newStacklessCoroNativeContext()
}

func releaseStacklessCoroNativeContext(ctx *stacklessCoroNativeContext) {
	releaseStacklessCoroNativeContextAt(ctx, work.cycles.Load())
}

func releaseStacklessCoroNativeContextAt(ctx *stacklessCoroNativeContext, idleCycle uint32) {
	if ctx.deferredYield || ctx.deferredSleep != nil || ctx.deferredSleepWhen != 0 {
		throw("runtime: releasing native context with deferred driver work")
	}
	// Replace an unused cache only after two consecutive roots requested the
	// same new chunk family. A single interleaved root keeps the old working
	// set; no identity is changed while this executor is running a lineage.
	if ctx.selfFrameMissedRoot && !ctx.selfFrameCacheUsed {
		ctx.selfFrameMissRoots++
		if ctx.selfFrameMissRoots >= 2 {
			clearStacklessCoroNativePayloadCache(ctx)
		}
	} else {
		ctx.selfFrameMissRoots = 0
	}
	ctx.cacheIdleCycle = idleCycle
	for i := range stacklessCoroNativePool.slots {
		if stacklessCoroNativePool.slots[i].CompareAndSwap(nil, ctx) {
			return
		}
	}
	// Only the bounded warm pool retains task caches. The overflow list may
	// grow with peak root concurrency and must not retain one cache per peak.
	clearStacklessCoroNativePayloadCache(ctx)
	releaseStacklessCoroNativeSchedulerStack(ctx)
	lock(&stacklessCoroNativePool.lock)
	ctx.poolNext = stacklessCoroNativePool.overflow
	stacklessCoroNativePool.overflow = ctx
	unlock(&stacklessCoroNativePool.lock)
}

// yieldStacklessCoroNativeRoot ends a public episode before a host yield,
// including structured children of its root. Yielding a thread-bound executor
// could otherwise require a new M for every runnable public root. Keep spawned
// roots on their existing handoff: their public driver closes without waiting
// and cannot consume deferred work. The ordinary driver consumes the queued wake.
func yieldStacklessCoroNativeRoot(s *stacklessCoroScheduler, task *stacklessCoroTask) bool {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	if ctx == nil || ctx.executor != gp || !ctx.public || s.publicDriverDone() {
		Gosched()
		return false
	}
	task.context.scheduler = nil
	s.yield(task, false)
	ctx.deferredYield = true
	s.signal()
	return true
}

// clearStacklessCoroNativePayloadCache drops only idle typed storage. The
// native executor identity and its fixed scheduler stack remain reusable.
func clearStacklessCoroNativePayloadCache(ctx *stacklessCoroNativeContext) {
	ctx.freeTasks = nil
	ctx.freePlainTaskCount = 0
	ctx.freeFrameBytes = 0
	ctx.cachedFrameTasks = 0
	ctx.cachedFrameBytes = 0
	ctx.selfFrames = nil
	ctx.selfFrameChunks = nil
	ctx.selfFrameResume = nil
	ctx.selfFrameChunkType = nil
	ctx.selfFrameSize = 0
	ctx.selfFrameChunkSize = 0
	ctx.selfFrameChunkCount = 0
	ctx.selfFrameCount = 0
	ctx.selfFrameMissResume = nil
	ctx.selfFrameMissType = nil
	ctx.selfFrameMissSize = 0
	ctx.selfFrameMissRoots = 0
	ctx.selfFrameMissedRoot = false
	ctx.selfFrameCacheUsed = false
	releaseStacklessCoroNativeBurst(ctx)
}

// clearStacklessCoroNativeCaches runs with the world stopped, before the GC
// cycle number advances. Idle pool and registered parked contexts have no
// running native episode, so their unused cache storage can be examined.
// Trim recently used chunks to the ordinary budget for one collection, then
// drop payload caches that remained idle for a complete GC generation.
// A user GC that first waited on an automatic mark phase coalesces only the
// shared, fully cleared payload cache's two aging steps. This can retain its
// existing bounded payload through one extra collection; other caches age as
// usual, and a subsequent idle user request expires the shared old generation.
func clearStacklessCoroNativeCaches(coalescePayload bool) {
	if !coalescePayload {
		clearStacklessCoroChannelCache()
	}
	cycle := work.cycles.Load()
	for i := range stacklessCoroNativePool.slots {
		ctx := stacklessCoroNativePool.slots[i].Load()
		if ctx == nil {
			continue
		}
		if cycle-ctx.cacheIdleCycle >= 1 {
			clearStacklessCoroNativePayloadCache(ctx)
			if gp := ctx.schedulerG; gp != nil {
				ctx.schedulerG = nil
				freeStacklessCoroNativeSchedulerStack(gp)
			}
		} else {
			trimStacklessCoroNativeSelfFrameChunks(ctx)
			releaseStacklessCoroNativeBurst(ctx)
		}
	}
	for i := range stacklessCoroNativeParkedCaches {
		ctx := stacklessCoroNativeParkedCaches[i].Load()
		if ctx != nil && cycle-ctx.cacheIdleCycle >= 1 {
			clearStacklessCoroNativeParkedCache(ctx)
		}
	}
	for i := range stacklessCoroNativeSchedulerStacks {
		slot := &stacklessCoroNativeSchedulerStacks[i]
		if gp := slot.Load(); gp != nil && slot.CompareAndSwap(gp, nil) {
			freeStacklessCoroNativeSchedulerStack(gp)
		}
	}
}

// stacklessCoroSelfFrameChunkBudget bounds chunks by their allocator class.
// Small recursive frames keep the ordinary cache bound.
func stacklessCoroSelfFrameChunkBudget(ctx *stacklessCoroNativeContext) uintptr {
	if ctx.selfFrameBurst {
		return stacklessCoroSelfFrameBurstCacheSize
	}
	return stacklessCoroSelfFrameChunkCacheSize
}

// takeStacklessCoroNativeBurst reserves only additional cache capacity. Live
// frames do not consume this quota. The context remains single-owner; only
// the global reservation count needs atomic operations.
func takeStacklessCoroNativeBurst(ctx *stacklessCoroNativeContext) {
	if ctx.selfFrameBurst {
		return
	}
	for {
		n := stacklessCoroNativeBurstContexts.Load()
		if n >= stacklessCoroWarmExecutorCount {
			return
		}
		if stacklessCoroNativeBurstContexts.CompareAndSwap(n, n+1) {
			ctx.selfFrameBurst = true
			return
		}
	}
}

func releaseStacklessCoroNativeBurst(ctx *stacklessCoroNativeContext) {
	if ctx.selfFrameBurst {
		ctx.selfFrameBurst = false
		stacklessCoroNativeBurstContexts.Add(-1)
	}
}

// trimStacklessCoroNativeSelfFrameChunks keeps the head of an idle chunk list.
// Walking only retained chunks bounds stop-the-world work by the ordinary
// budget, independently of the burst length. The list contains only cleared,
// unowned typed allocations; cutting its tail releases the excess to GC.
func trimStacklessCoroNativeSelfFrameChunks(ctx *stacklessCoroNativeContext) {
	if ctx.selfFrameChunkCount == 0 {
		return
	}
	if ctx.selfFrameChunkSize == 0 {
		throw("runtime: invalid stackless coroutine self-frame chunk size")
	}
	keep := stacklessCoroSelfFrameChunkCacheSize / ctx.selfFrameChunkSize
	if uintptr(ctx.selfFrameChunkCount) <= keep {
		return
	}
	if keep == 0 {
		ctx.selfFrameChunks = nil
		ctx.selfFrameChunkCount = 0
		return
	}
	frame := ctx.selfFrameChunks
	for i := uintptr(0); i < keep; i++ {
		if frame == nil {
			throw("runtime: invalid stackless coroutine self-frame chunk list")
		}
		header := (*stacklessCoroFusedFrameHeader)(frame)
		if header.owner != nil || header.marker != 0 || header.parent == nil {
			throw("runtime: initialized stackless coroutine cached self frame")
		}
		if i+1 == keep {
			header.parent = nil
			break
		}
		frame = header.parent
	}
	ctx.selfFrameChunkCount = uint16(keep)
}

// takeStacklessCoroNativeFusedChunk opportunistically reuses compatible chunks
// without replacing an established cache identity during a running lineage.
// Record repeated misses for phase replacement after the executor is idle;
// alternating deep self and mutual roots preserve the self working set.
func takeStacklessCoroNativeFusedChunk(resume stacklessCoroResume,
	frameSize uintptr, chunkType *_type) unsafe.Pointer {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	if raceenabled || ctx == nil || ctx.executor != gp || resume == nil {
		return nil
	}
	if ctx.selfFrameResume != nil &&
		(ctx.selfFrameResume != stacklessCoroResumeIdentity(resume) ||
			ctx.selfFrameSize != frameSize || ctx.selfFrameChunkType != chunkType) {
		if !ctx.selfFrameMissedRoot {
			identity := stacklessCoroResumeIdentity(resume)
			if ctx.selfFrameMissResume != identity ||
				ctx.selfFrameMissType != chunkType || ctx.selfFrameMissSize != frameSize {
				ctx.selfFrameMissResume = identity
				ctx.selfFrameMissType = chunkType
				ctx.selfFrameMissSize = frameSize
				ctx.selfFrameMissRoots = 0
			}
			ctx.selfFrameMissedRoot = true
		}
		return nil
	}
	return takeStacklessCoroNativeSelfFrame(resume, frameSize, chunkType, true)
}

// takeStacklessCoroNativeSelfFrame reuses homogeneous typed storage on the
// executor that allocated it. A native context is owned by one executor at a
// time, so this cache needs no scheduler lock. Keeping one resume identity
// bounds both lookup cost and retained type diversity.
func takeStacklessCoroNativeSelfFrame(resume stacklessCoroResume,
	frameSize uintptr, chunkType *_type, chunk bool) unsafe.Pointer {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	var chunkSize uintptr
	if chunkType != nil {
		chunkSize = chunkType.Size_
	}
	if raceenabled || ctx == nil || ctx.executor != gp || resume == nil ||
		frameSize == 0 || chunkSize == 0 ||
		chunkSize > stacklessCoroSelfFrameBurstCacheSize {
		return nil
	}
	identity := stacklessCoroResumeIdentity(resume)
	if ctx.selfFrameResume != identity || ctx.selfFrameSize != frameSize ||
		ctx.selfFrameChunkType != chunkType {
		ctx.selfFrames = nil
		ctx.selfFrameChunks = nil
		ctx.selfFrameResume = identity
		ctx.selfFrameChunkType = chunkType
		ctx.selfFrameSize = frameSize
		// Charge the allocator class, including its malloc header, rather
		// than payload bytes. Packing a frame must not expand this budget.
		ctx.selfFrameChunkSize = roundupsize(chunkSize, false)
		if chunkSize > gc.MinSizeForMallocHeader {
			ctx.selfFrameChunkSize += gc.MallocHeaderSize
		}
		ctx.selfFrameChunkCount = 0
		ctx.selfFrameCount = 0
		if frameSize < stacklessCoroSelfFrameBurstMinSize {
			releaseStacklessCoroNativeBurst(ctx)
		}
		return nil
	}
	link := &ctx.selfFrames
	count := &ctx.selfFrameCount
	if chunk {
		link = &ctx.selfFrameChunks
	}
	frame := *link
	if frame == nil {
		if (chunk && ctx.selfFrameChunkCount != 0) ||
			(!chunk && *count != 0) {
			throw("runtime: invalid stackless coroutine self-frame cache")
		}
		return nil
	}
	ctx.selfFrameCacheUsed = true
	header := (*stacklessCoroFusedFrameHeader)(frame)
	*link = header.parent
	header.parent = nil
	if chunk {
		if ctx.selfFrameChunkCount == 0 {
			throw("runtime: invalid stackless coroutine self-frame chunk count")
		}
		ctx.selfFrameChunkCount--
	} else {
		if *count == 0 {
			throw("runtime: invalid stackless coroutine self-frame count")
		}
		*count--
	}
	return frame
}

// cacheStacklessCoroNativeSelfFrame retains only storage whose allocation key
// still matches the active executor cache. Interleaved recursive identities
// may replace that key; the older lineage then remains ordinary GC work.
func cacheStacklessCoroNativeSelfFrame(resume stacklessCoroResume,
	frame unsafe.Pointer, allocation uint8) {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	if raceenabled || ctx == nil || ctx.executor != gp || resume == nil ||
		frame == nil ||
		ctx.selfFrameResume != stacklessCoroResumeIdentity(resume) ||
		ctx.selfFrameSize == 0 || ctx.selfFrameChunkSize == 0 {
		return
	}
	header := (*stacklessCoroFusedFrameHeader)(frame)
	if header.parent != nil || header.owner != nil || header.marker != 0 {
		throw("runtime: initialized stackless coroutine cached self frame")
	}
	switch {
	case allocation >= stacklessCoroFusedFrameDirectFirst &&
		allocation <= stacklessCoroFusedFrameDirectLast:
		if ctx.selfFrameCount >= stacklessCoroFrameChunkDirectCount {
			return
		}
		header.parent = ctx.selfFrames
		ctx.selfFrames = frame
		ctx.selfFrameCount++
	case allocation == stacklessCoroFusedFrameChunkFirst:
		// Reserve extra capacity only when completed chunks actually exceed
		// the ordinary bound. Shallow live lineages must not hold a quota.
		if ctx.selfFrameSize >= stacklessCoroSelfFrameBurstMinSize &&
			!ctx.selfFrameBurst && uintptr(ctx.selfFrameChunkCount) >=
			stacklessCoroSelfFrameChunkCacheSize/ctx.selfFrameChunkSize {
			takeStacklessCoroNativeBurst(ctx)
		}
		if uintptr(ctx.selfFrameChunkCount) >=
			stacklessCoroSelfFrameChunkBudget(ctx)/ctx.selfFrameChunkSize {
			return
		}
		header.parent = ctx.selfFrameChunks
		ctx.selfFrameChunks = frame
		ctx.selfFrameChunkCount++
	}
}

func newStacklessCoroNativeContext() *stacklessCoroNativeContext {
	ctx := new(stacklessCoroNativeContext)
	ctx.gState.native = unsafe.Pointer(ctx)
	ctx.schedulerG = acquireStacklessCoroNativeSchedulerStack()

	executor := malg(-1)
	casgstatus(executor, _Gidle, _Gdead)
	executor.startpc = abi.FuncPCABIInternal(coroNativeMain)
	executor.goid = sched.goidgen.Add(1)
	executor.sched.g = guintptr(unsafe.Pointer(executor))
	executor.trace.reset()
	allgadd(executor)
	sched.ngsys.Add(1)
	ctx.executor = executor
	return ctx
}

func acquireStacklessCoroNativeSchedulerStack() *g {
	for i := range stacklessCoroNativeSchedulerStacks {
		slot := &stacklessCoroNativeSchedulerStacks[i]
		gp := slot.Load()
		if gp != nil && slot.CompareAndSwap(gp, nil) {
			return gp
		}
	}
	return newStacklessCoroNativeSchedulerStack()
}

// Keep empty-descriptor acquisition and stack initialization off the shared-stack fast path.
func newStacklessCoroNativeSchedulerStack() *g {
	var schedulerG *g
	for {
		free := stacklessCoroNativeSchedulerDescriptors.free.Load()
		if free == 0 {
			break
		}
		i := sys.TrailingZeros64(free)
		bit := uint64(1) << uint(i)
		if stacklessCoroNativeSchedulerDescriptors.free.CompareAndSwap(free, free&^bit) {
			schedulerG = &stacklessCoroNativeSchedulerDescriptors.slots[i]
			break
		}
	}
	if schedulerG == nil {
		schedulerG = new(g)
	}
	// This stack runs g0 runtime work; the executor keeps the OS thread stack.
	// Match malg's stack allocation and instrumentation for either a new or
	// recycled descriptor, and allocm's Go-allocated scheduler-stack size.
	stacksize := round2(stackSystem + 16384*sys.StackGuardMultiplier)
	systemstack(func() {
		schedulerG.stack = stackalloc(uint32(stacksize))
		if valgrindenabled {
			schedulerG.valgrindStackID = valgrindRegisterStack(unsafe.Pointer(schedulerG.stack.lo), unsafe.Pointer(schedulerG.stack.hi))
		}
	})
	schedulerG.stackguard0 = schedulerG.stack.lo + stackGuard
	schedulerG.stackguard1 = schedulerG.stackguard0
	*(*uintptr)(unsafe.Pointer(schedulerG.stack.lo)) = 0
	schedulerG.sched.sp = schedulerG.stack.hi - 4*goarch.PtrSize
	schedulerG.sched.g = guintptr(unsafe.Pointer(schedulerG))
	return schedulerG
}

func releaseStacklessCoroNativeSchedulerStack(ctx *stacklessCoroNativeContext) {
	gp := ctx.schedulerG
	if gp == nil {
		return
	}
	if gp.m != nil {
		throw("runtime: releasing active native scheduler stack")
	}
	ctx.schedulerG = nil
	for i := range stacklessCoroNativeSchedulerStacks {
		if stacklessCoroNativeSchedulerStacks[i].CompareAndSwap(nil, gp) {
			return
		}
	}
	freeStacklessCoroNativeSchedulerStack(gp)
}

func freeStacklessCoroNativeSchedulerStack(gp *g) {
	if gp.m != nil {
		throw("runtime: freeing active native scheduler stack")
	}
	// No running M or suspended native activation can refer to this G. The
	// stack allocator requires the current M's system stack to free storage.
	systemstack(func() {
		stackfree(gp.stack)
		if valgrindenabled {
			valgrindDeregisterStack(gp.valgrindStackID)
			gp.valgrindStackID = 0
		}
		gp.stack = stack{}
		gp.stackguard0 = 0
		gp.stackguard1 = 0
	})
	// Keep no saved registers or heap funcval from the finished activation.
	// The next borrower allocates a stack before installing this G as m.g0.
	gp.sched = gobuf{}
	base := uintptr(unsafe.Pointer(&stacklessCoroNativeSchedulerDescriptors.slots[0]))
	addr := uintptr(unsafe.Pointer(gp))
	if addr >= base && addr < base+unsafe.Sizeof(stacklessCoroNativeSchedulerDescriptors.slots) {
		i := (addr - base) / unsafe.Sizeof(g{})
		// Exclusive ownership keeps this bit zero until release, so addition
		// sets only this bit without carry, including the wrapping high bit.
		stacklessCoroNativeSchedulerDescriptors.free.Add(int64(uint64(1) << uint(i)))
	}

}

// coroNativeStart is the mcall continuation that replaces the native g0
// identity with an executor G. It never returns.
func coroNativeStart(caller *g) {
	ctx := (*stacklessCoroNativeContext)(caller.param)
	if ctx == nil || ctx.caller != caller {
		throw("runtime: invalid stackless coroutine native start")
	}

	nativeG0 := getg()
	mp := nativeG0.m
	if nativeG0 != mp.g0 || mp.curg != caller {
		throw("runtime: invalid stackless coroutine native bootstrap")
	}

	// sys.GetCallerSP identifies the mcall frame above this function. Build a
	// fresh synthetic goroutine root below both bootstrap frames so precise
	// stack scanning never encounters mcall in the executor's call chain.
	hi := alignDown(sys.GetCallerSP()-stacklessCoroBootstrapReserve, sys.StackAlign)
	if hi <= nativeG0.stack.lo+stackGuard+stackMin {
		throw("runtime: native stack is too small for stackless coroutine executor")
	}

	executor := ctx.executor
	schedulerG := ctx.schedulerG
	ctx.nativeG0 = nativeG0
	ctx.lockedG = mp.lockedg
	ctx.lockedInt = mp.lockedInt
	ctx.g0Accurate = mp.g0StackAccurate

	executor.stack = stack{lo: nativeG0.stack.lo, hi: hi}
	executor.stackguard0 = executor.stack.lo + stackGuard
	executor.stackguard1 = ^uintptr(0)
	executor.m = mp
	executor.stacklessCoro = &ctx.gState

	totalSize := uintptr(4*goarch.PtrSize + sys.MinFrameSize)
	totalSize = alignUp(totalSize, sys.StackAlign)
	sp := hi - totalSize
	if usesLR {
		*(*uintptr)(unsafe.Pointer(sp)) = 0
		prepGoExitFrame(sp)
	}
	if GOARCH == "arm64" {
		*(*uintptr)(unsafe.Pointer(sp - goarch.PtrSize)) = 0
	}
	memclrNoHeapPointers(unsafe.Pointer(&executor.sched), unsafe.Sizeof(executor.sched))
	executor.sched.sp = sp
	executor.stktopsp = sp
	executor.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
	executor.sched.g = guintptr(unsafe.Pointer(executor))
	gostartcall(&executor.sched, unsafe.Pointer(abi.FuncPCABIInternal(coroNativeMain)), nil)

	schedulerG.m = mp
	memclrNoHeapPointers(unsafe.Pointer(&schedulerG.sched), unsafe.Sizeof(schedulerG.sched))
	schedulerG.sched.sp = schedulerG.stack.hi - 4*goarch.PtrSize
	schedulerG.sched.g = guintptr(unsafe.Pointer(schedulerG))

	executor.trace.reset()
	trace := traceAcquire()
	tracing := trace.ok()
	canCASCaller := !tracing && caller.bubble == nil && !goroutineProfile.active
	canCASExecutor := !tracing && executor.bubble == nil &&
		!goroutineProfile.active && !casgstatusAlwaysTrack && !executor.tracking
	gcController.addScannableStack(mp.p.ptr(), int64(executor.stack.hi-executor.stack.lo))
	if !canCASExecutor ||
		!executor.atomicstatus.CompareAndSwap(_Gdead, _Grunnable) {
		// Preserve scheduler latency tracking and wait for a concurrent stack
		// scan when the executor is observable through the ordinary G path.
		casgstatus(executor, _Gdead, _Grunnable)
	}
	if tracing {
		trace.GoCreate(executor, executor.startpc, false)
		trace.GoPark(traceBlockGeneric, 0)
	}
	caller.waitreason = waitReasonCoroutine
	if !canCASCaller ||
		!caller.atomicstatus.CompareAndSwap(_Grunning, _Gwaiting) {
		// Preserve the ordinary transition when a concurrent stack scan owns
		// the status or a runtime observer needs the full path.
		casgstatus(caller, _Grunning, _Gwaiting)
	}
	caller.m = nil

	mp.curg = executor
	mp.lockedInt++
	if mp.lockedInt == 0 {
		throw("runtime: stackless coroutine locked thread count overflow")
	}
	mp.lockedg.set(executor)
	executor.lockedm.set(mp)
	if !canCASExecutor ||
		!executor.atomicstatus.CompareAndSwap(_Grunnable, _Grunning) {
		casgstatus(executor, _Grunnable, _Grunning)
	}
	if tracing {
		trace.GoStart()
		traceRelease(trace)
	}

	// Keep nativeG0 installed as m.g0 until every runtime call above has
	// completed. gogo performs the signal-safe G and stack transition; the
	// executor replaces m.g0 only after it is running on its target stack.
	gogo(&executor.sched)
}

func coroNativeMain() {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	if ctx == nil || ctx.executor != gp || gp.m.g0 != ctx.nativeG0 || ctx.schedulerG.m != gp.m {
		throw("runtime: invalid stackless coroutine executor context")
	}
	gp.m.g0 = ctx.schedulerG
	gp.m.g0StackAccurate = true
	initial := ctx.initialRoot
	ctx.initialRoot = false
	ctx.scheduler.runTasks(true, false, initial, ctx.public)
	mcall(coroNativeFinish)
	throw("runtime: stackless coroutine native finish returned")
}

// coroNativeFinish restores the original g0 and caller. It runs on the
// separate scheduler stack and never returns.
func coroNativeFinish(executor *g) {
	ctx := stacklessCoroNativeContextFor(executor)
	if ctx == nil || ctx.executor != executor {
		throw("runtime: invalid stackless coroutine native finish")
	}
	if ctx.gState.deferTask != nil || ctx.gState.blockingScheduler != nil {
		throw("runtime: active stackless coroutine state during native finish")
	}
	schedulerG := getg()
	mp := schedulerG.m
	caller := ctx.caller
	if schedulerG != ctx.schedulerG || mp.g0 != schedulerG || mp.curg != executor {
		throw("runtime: invalid stackless coroutine native teardown")
	}

	trace := traceAcquire()
	tracing := trace.ok()
	canCASCaller := !tracing && caller.bubble == nil && !goroutineProfile.active
	canCASExecutor := !tracing && executor.bubble == nil &&
		!goroutineProfile.active && !casgstatusAlwaysTrack && !executor.tracking
	if tracing {
		trace.GoEnd()
	}
	if !canCASExecutor ||
		!executor.atomicstatus.CompareAndSwap(_Grunning, _Gdead) {
		casgstatus(executor, _Grunning, _Gdead)
	}
	gcController.addScannableStack(mp.p.ptr(), -int64(executor.stack.hi-executor.stack.lo))
	resetStacklessCoroExecutor(executor)

	executor.m = nil
	executor.lockedm = 0
	executor.stacklessCoro = nil
	executor.stack = stack{}
	executor.stackguard0 = 0
	executor.stackguard1 = 0
	executor.stktopsp = 0
	memclrNoHeapPointers(unsafe.Pointer(&executor.sched), unsafe.Sizeof(executor.sched))

	mp.curg = caller
	mp.lockedg = ctx.lockedG
	mp.lockedInt = ctx.lockedInt
	caller.m = mp

	if !canCASCaller ||
		!caller.atomicstatus.CompareAndSwap(_Gwaiting, _Grunning) {
		// Preserve the ordinary runnable transition when a concurrent stack
		// scan owns the status or a runtime observer needs the full path.
		casgstatus(caller, _Gwaiting, _Grunnable)
		if tracing {
			trace.GoUnpark(caller, 0)
		}
		casgstatus(caller, _Grunnable, _Grunning)
	}
	if tracing {
		trace.GoStart()
		traceRelease(trace)
	}

	// Keep schedulerG installed as m.g0 through gogo's signal-safe G and stack
	// transition. The caller restores nativeG0 after it is running again.
	gogo(&caller.sched)
}

//go:nosplit
func stacklessCoroNativeContextFor(gp *g) *stacklessCoroNativeContext {
	state := gp.stacklessCoro
	if state == nil {
		return nil
	}
	return (*stacklessCoroNativeContext)(state.native)
}

//go:nosplit
func stacklessCoroNativeSchedulerFor(gp *g) *stacklessCoroScheduler {
	ctx := stacklessCoroNativeContextFor(gp)
	if ctx == nil {
		return nil
	}
	if ctx.scheduler == nil {
		throw("runtime: stackless coroutine blocking call has no scheduler")
	}
	return ctx.scheduler
}

func stacklessCoroDeferSleep(s *stacklessCoroScheduler,
	op *stacklessCoroOperation, when int64) bool {
	gp := getg()
	ctx := stacklessCoroNativeContextFor(gp)
	if ctx == nil || ctx.executor != gp || ctx.scheduler != s ||
		ctx.deferredSleep != nil {
		return false
	}
	lock(&s.lock)
	deferSleep := s.executorState.Load() == stacklessCoroExecutorStateOff &&
		s.head == nil && s.tail == nil && s.runnableState.Load() == 0
	if deferSleep {
		ctx.deferredSleep = op
		ctx.deferredSleepWhen = when
	}
	unlock(&s.lock)
	return deferSleep
}

// resetStacklessCoroExecutor clears state that execute and gdestroy normally
// reset for a reused G. In particular, a concurrent stack scan may request a
// synchronous preemption immediately before the executor becomes dead. That
// request has no owner after suspendG observes the dead G and must not survive
// into the next use of this synthetic executor.
func resetStacklessCoroExecutor(executor *g) {
	executor.preempt = false
	executor.preemptStop = false
	executor.preemptShrink = false
	executor.syncSafePoint = false
	executor.asyncSafePoint = false
	executor.throwsplit = false
	executor.waitreason = waitReasonZero
	executor.waitsince = 0
}
