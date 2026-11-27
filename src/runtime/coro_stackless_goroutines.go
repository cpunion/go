// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/cpu"
	"internal/profilerecord"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// Independent source go statements have an identity before they need an
// ordinary stack. Keep this cold metadata outside the fixed task and native
// executor layouts. Typed links root the suspended source until completion;
// completed public records are cleared before bounded reuse.
type stacklessCoroGoroutine struct {
	next             *stacklessCoroGoroutine
	publicNext       *stacklessCoroGoroutine
	root, leaf       *stacklessCoroTask
	scheduler        *stacklessCoroScheduler
	owner            atomic.Pointer[g]
	publicG          *g
	publicState      *stacklessCoroPublicGState
	hostG            *g
	goid, parentGoid uint64
	gopc             uintptr
	labels           unsafe.Pointer
	ancestors        *[]ancestorInfo
	profileCaller    []uintptr
	profileBoundary  *stacklessCoroTask
	profileTrace     *stacklessCoroPanicTrace
	profileBefore    bool
	public, hosted   bool
	inRegistry       bool
	// shard is 0 for records in the legacy global hash table, and 1..N for
	// records stored in stacklessCoroGoroutineShards[shard-1] (which reuse
	// publicNext as the doubly-linked prev pointer for O(1) unlink).
	shard uint8
}

const (
	stacklessCoroInlineBuckets          = 256
	stacklessCoroGoroutineBurstCacheCap = 1024
	stacklessCoroGoroutineShardCount    = 16
	stacklessCoroGoroutineShardFreeCap  = 128
	stacklessCoroPooledGShard           = 0xFF
	stacklessCoroPooledGTransit         = 0xFE
	stacklessCoroPooledGCap             = 2048
	stacklessCoroPooledGChunkSlots      = 64
	stacklessCoroPooledGChunkSize       = 64
)

// stacklessCoroPooledG holds up to 2048 reusable multi-P goroutine records
// whose registration and completion are 100% lock-free (no runtime.lock on
// spawn, complete, refill, or flush). STW visitors scan slots[:count] and
// skip records with !r.inRegistry. Idle records expire after two GC cycles.
var stacklessCoroPooledG struct {
	lock       mutex
	count      atomic.Uint32
	slots      [stacklessCoroPooledGCap]*stacklessCoroGoroutine
	idleAge    [stacklessCoroPooledGCap]uint8
	freeChunks [stacklessCoroPooledGChunkSlots]atomic.Pointer[stacklessCoroGoroutine]
	freeHint   atomic.Uint32
}

type stacklessCoroGoroutineShard struct {
	lock      mutex
	head      *stacklessCoroGoroutine
	free      [2]*stacklessCoroGoroutine
	freeCount [2]uint16
	freeYoung uint8
	unhosted  atomic.Int64
	records   atomic.Int64
	_         cpu.CacheLinePad
}

var stacklessCoroGoroutineShards [stacklessCoroGoroutineShardCount]stacklessCoroGoroutineShard

type stacklessCoroSpawnSite struct {
	returnPC, sourcePC uintptr
}

var stacklessCoroGoroutines struct {
	lock            mutex
	inlineBuckets   [stacklessCoroInlineBuckets]*stacklessCoroGoroutine
	largeBuckets    []*stacklessCoroGoroutine
	publicBuckets   [256]*stacklessCoroGoroutine
	publicFree      *stacklessCoroGoroutine
	publicFreeCount int
	taskFree        *stacklessCoroGoroutine
	taskFreeCount   int
	taskBurst       [2]*stacklessCoroGoroutine
	taskBurstCount  [2]int
	taskBurstYoung  uint8
	pending         *stacklessCoroHost
	spawnSites      [128]stacklessCoroSpawnSite
	unhosted        atomic.Int64
	records         atomic.Int64
}

func init() {
	lockInit(&stacklessCoroGoroutines.lock, lockRankStacklessCoroCache)
	lockInit(&stacklessCoroPooledG.lock, lockRankStacklessCoroCache)
	for i := range stacklessCoroGoroutineShards {
		lockInit(&stacklessCoroGoroutineShards[i].lock, lockRankStacklessCoroCache)
	}
}

func pushCoroPooledGChunk(head *stacklessCoroGoroutine) {
	if head == nil {
		return
	}
	hint := stacklessCoroPooledG.freeHint.Add(1)
	for i := uint32(0); i < stacklessCoroPooledGChunkSlots; i++ {
		idx := (hint + i) & (stacklessCoroPooledGChunkSlots - 1)
		if stacklessCoroPooledG.freeChunks[idx].Load() == nil &&
			stacklessCoroPooledG.freeChunks[idx].CompareAndSwap(nil, head) {
			return
		}
	}
	// If all chunk slots are full, records remain tracked in stacklessCoroPooledG.slots
	// and will be reclaimed/rebuilt on GC.
}

func popCoroPooledGChunk() *stacklessCoroGoroutine {
	mp := acquirem()
	hint := stacklessCoroPooledG.freeHint.Load()
	for i := uint32(0); i < stacklessCoroPooledGChunkSlots; i++ {
		idx := (hint - i) & (stacklessCoroPooledGChunkSlots - 1)
		if head := stacklessCoroPooledG.freeChunks[idx].Load(); head != nil &&
			stacklessCoroPooledG.freeChunks[idx].CompareAndSwap(head, nil) {
			for cur := head; cur != nil; cur = cur.next {
				cur.shard = stacklessCoroPooledGTransit
			}
			releasem(mp)
			return head
		}
	}
	releasem(mp)
	return nil
}

// allocCoroPooledGBatch allocates up to stacklessCoroPooledGChunkSize new
// pooled goroutine records into stacklessCoroPooledG.slots under cold lock.
// Every newly installed record starts with stacklessCoroPooledGTransit so a
// concurrent STW GC triggered by new() cannot reclaim it before publication.
func allocCoroPooledGBatch() *stacklessCoroGoroutine {
	lock(&stacklessCoroPooledG.lock)
	var head *stacklessCoroGoroutine
	n := 0
	cur := stacklessCoroPooledG.count.Load()
	for i := uint32(0); i < cur && n < stacklessCoroPooledGChunkSize; i++ {
		if stacklessCoroPooledG.slots[i] == nil {
			r := new(stacklessCoroGoroutine)
			r.shard = stacklessCoroPooledGTransit
			stacklessCoroPooledG.slots[i] = r
			stacklessCoroPooledG.idleAge[i] = 0
			r.next = head
			head = r
			n++
		}
	}
	for cur < stacklessCoroPooledGCap && n < stacklessCoroPooledGChunkSize {
		r := new(stacklessCoroGoroutine)
		r.shard = stacklessCoroPooledGTransit
		stacklessCoroPooledG.slots[cur] = r
		stacklessCoroPooledG.idleAge[cur] = 0
		cur++
		r.next = head
		head = r
		n++
	}
	if cur > stacklessCoroPooledG.count.Load() {
		stacklessCoroPooledG.count.Store(cur)
	}
	unlock(&stacklessCoroPooledG.lock)
	return head
}

func takeStacklessCoroPooledGoroutine() *stacklessCoroGoroutine {
	if r := coroPGoroutineGet(); r != nil {
		return r
	}
	chunk := popCoroPooledGChunk()
	if chunk == nil {
		chunk = allocCoroPooledGBatch()
		if chunk == nil {
			return nil
		}
	}
	first := chunk
	rest := first.next
	first.next = nil
	if rest != nil {
		coroPGoroutinePutBatch(rest)
	}
	return first
}

func stacklessCoroGoroutineBucket(root *stacklessCoroTask) **stacklessCoroGoroutine {
	buckets := stacklessCoroGoroutineBuckets()
	return &buckets[stacklessCoroGoroutineIndex(root, len(buckets))]
}

func stacklessCoroGoroutineIndex(root *stacklessCoroTask, n int) uintptr {
	x := uint64(uintptr(unsafe.Pointer(root)))
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return uintptr(x^(x>>31)) & uintptr(n-1)
}

// Readers hold the registry lock or run with the world stopped.
func stacklessCoroGoroutineBuckets() []*stacklessCoroGoroutine {
	if b := stacklessCoroGoroutines.largeBuckets; b != nil {
		return b
	}
	return stacklessCoroGoroutines.inlineBuckets[:]
}

// Rehash the typed links under the registry lock. The table grows with the
// live population so tasks of the same allocator size cannot accumulate in a
// few long lists. A completed burst releases its bucket storage.
func stacklessCoroResizeGoroutineBuckets(size int) {
	old := stacklessCoroGoroutineBuckets()
	var buckets []*stacklessCoroGoroutine
	if size == stacklessCoroInlineBuckets {
		buckets = stacklessCoroGoroutines.inlineBuckets[:]
	} else {
		buckets = make([]*stacklessCoroGoroutine, size)
	}
	for i, r := range old {
		old[i] = nil
		for r != nil {
			next := r.next
			slot := &buckets[stacklessCoroGoroutineIndex(r.root, size)]
			r.next, *slot = *slot, r
			r = next
		}
	}
	if size == stacklessCoroInlineBuckets {
		buckets = nil
	}
	stacklessCoroGoroutines.largeBuckets = buckets
}

func stacklessCoroPublicGoroutineBucket(gp *g) **stacklessCoroGoroutine {
	return &stacklessCoroGoroutines.publicBuckets[(uintptr(unsafe.Pointer(gp))>>3)%256]
}

// Register before the first native episode, including roots that never spawn
// an independent task. The original ordinary G owns the identity and count.
// Hand-written runtime continuations without source metadata need no record
// until they spawn a child. Reuse only completely cleared public records.
func (s *stacklessCoroScheduler) registerPublicGoroutine(callerPC uintptr, callerSP unsafe.Pointer) {
	gp := getg()
	resumePC := stacklessCoroResumeIdentity(s.root.resume).fn
	// Fast path: keep an unnested public root private to its ordinary G so
	// short-lived roots avoid global registry locking, hashing, and counters.
	if pub := acquireStacklessCoroPublicState(gp); pub != nil && pub.publicRecord.Load() == nil {
		r := pub.publicFree
		if r != nil {
			pub.publicFree = nil
		} else {
			r = &stacklessCoroGoroutine{profileCaller: make([]uintptr, 0, maxCPUProfStack)}
		}
		buf := r.profileCaller[:cap(r.profileCaller)]
		n := 0
		if gp.stktopsp-uintptr(callerSP) > stacklessCoroGoStartProfileDepth {
			if cached, ok := stacklessCoroCachedPublicProfileCaller(gp, resumePC, callerPC, callerSP, buf); ok {
				n = cached
			}
		}
		if n == 0 {
			source := s.publicSourceFunc()
			if !source.valid() {
				if pub.publicFree == nil {
					pub.publicFree = r
				}
				return
			}
			if gp.stktopsp-uintptr(callerSP) <= stacklessCoroGoStartProfileDepth {
				buf[0] = stacklessCoroProfileGoStart(gp, source, callerPC, callerSP)
			} else {
				buf[0] = 0
			}
			n = 1
			if buf[0] == 0 {
				n = stacklessCoroCachedProfileCaller(gp, resumePC, source.entry(), callerPC, callerSP, buf)
			}
		}
		r.root = &s.root
		r.leaf = &s.root
		r.scheduler = s
		r.publicG = gp
		r.publicState = pub
		r.goid = gp.goid
		r.parentGoid = gp.parentGoid
		r.gopc = gp.gopc
		if gp.labels != nil {
			r.labels = gp.labels
		}
		if gp.ancestors != nil {
			r.ancestors = gp.ancestors
		}
		r.profileCaller = buf[:n]
		r.public = true
		s.root.setSource(r)
		pub.publicRecord.Store(r)
		return
	}
	var pcs [maxCPUProfStack]uintptr
	n := 0
	if gp.stktopsp-uintptr(callerSP) > stacklessCoroGoStartProfileDepth {
		if cached, ok := stacklessCoroCachedPublicProfileCaller(gp, resumePC, callerPC, callerSP, pcs[:]); ok {
			n = cached
		}
	}
	if n == 0 {
		source := s.publicSourceFunc()
		if !source.valid() {
			return
		}
		// The caller still owns its stack. Walk it before locking the registry.
		// Most starts fit in two small Go frames. Skip the speculative walk
		// when the current stack already extends beyond them.
		if gp.stktopsp-uintptr(callerSP) <= stacklessCoroGoStartProfileDepth {
			pcs[0] = stacklessCoroProfileGoStart(gp, source, callerPC, callerSP)
		}
		n = 1
		if pcs[0] == 0 {
			n = stacklessCoroCachedProfileCaller(gp, resumePC, source.entry(), callerPC, callerSP, pcs[:])
		}
	}
	lock(&stacklessCoroGoroutines.lock)
	s.registerPublicGoroutineLocked(gp, &s.root, pcs[:n])
	unlock(&stacklessCoroGoroutines.lock)
}

func (s *stacklessCoroScheduler) publicSourceFunc() funcInfo {
	f := findfunc(stacklessCoroResumeIdentity(s.root.resume).fn)
	info := stacklessCoroSourceFrame(f)
	if info == nil || info.flags&abi.CoroSourceResume == 0 {
		return funcInfo{}
	}
	return stacklessCoroSourceFunc(f, info)
}

// A nil pcs indicates lazy registration without a saved public caller.
func (s *stacklessCoroScheduler) registerPublicGoroutineLocked(gp *g, leaf *stacklessCoroTask, pcs []uintptr) *stacklessCoroGoroutine {
	r := stacklessCoroGoroutines.publicFree
	if r != nil {
		stacklessCoroGoroutines.publicFree = r.next
		stacklessCoroGoroutines.publicFreeCount--
	} else {
		r = new(stacklessCoroGoroutine)
	}
	callerPCs := r.profileCaller
	*r = stacklessCoroGoroutine{root: &s.root, leaf: leaf, scheduler: s,
		goid: gp.goid, parentGoid: gp.parentGoid, gopc: gp.gopc,
		labels: gp.labels, ancestors: gp.ancestors, publicG: gp,
		profileCaller: callerPCs[:0], public: true}
	if pcs == nil && gp == getg() {
		if source := s.publicSourceFunc(); source.valid() {
			r.captureProfileCaller(gp, source.entry())
		}
	} else {
		r.setProfileCaller(pcs)
	}
	stacklessCoroInsertGoroutine(r)
	p := stacklessCoroPublicGoroutineBucket(gp)
	r.publicNext, *p = *p, r
	return r
}

//go:noinline
func (r *stacklessCoroGoroutine) captureProfileCaller(gp *g, rootPC uintptr) {
	var pcs [maxCPUProfStack]uintptr
	n := stacklessCoroCaptureProfileCaller(gp, rootPC, 0, nil, pcs[:], nil)
	r.setProfileCaller(pcs[:n])
}

func (r *stacklessCoroGoroutine) setProfileCaller(pcs []uintptr) {
	if len(pcs) != 0 && cap(r.profileCaller) == 0 {
		r.profileCaller = make([]uintptr, 0, maxCPUProfStack)
	}
	r.profileCaller = append(r.profileCaller[:0], pcs...)
}

func stacklessCoroLogicalRoot(task *stacklessCoroTask) *stacklessCoroTask {
	for p := task.parent(); p != nil; p = task.parent() {
		task = p
	}
	return task
}

// Requires the registry lock, or a stopped world. No registry operation
// acquires a scheduler lock; the opposite order is used by completion.
func stacklessCoroGoroutineFor(root *stacklessCoroTask) *stacklessCoroGoroutine {
	if r := root.source(); r != nil && r.root == root && r.inRegistry {
		return r
	}
	for r := *stacklessCoroGoroutineBucket(root); r != nil; r = r.next {
		if r.root == root {
			return r
		}
	}
	return nil
}

func stacklessCoroInsertGoroutine(r *stacklessCoroGoroutine) {
	if r.root.parent() != nil {
		throw("runtime: stackless coroutine record attached to non-root task")
	}
	size := len(stacklessCoroGoroutineBuckets())
	if stacklessCoroGoroutines.records.Load() >= int64(size)*2 {
		stacklessCoroResizeGoroutineBuckets(size * 2)
	}
	p := stacklessCoroGoroutineBucket(r.root)
	r.next, *p = *p, r
	r.inRegistry = true
	r.root.setSource(r)
	stacklessCoroGoroutines.records.Add(1)
}

// Called with s.lock before publishing a newly spawned task. Source roots
// already remember their public caller; hand-written roots do so lazily here.
// The caller can leave before independent children create further goroutines.
func (s *stacklessCoroScheduler) registerGoroutineLocked(task, parent *stacklessCoroTask, callerpc uintptr, callersp unsafe.Pointer) {
	if s.multiPSpawnFastPath() && debug.tracebackancestors == 0 {
		if s.registerGoroutineMultiP(task, parent, callerpc, callersp) {
			return
		}
	}
	gp := getg()
	caller := stacklessCoroPublicCaller(gp)
	lock(&stacklessCoroGoroutines.lock)
	root := stacklessCoroLogicalRoot(parent)
	creator := root.source()
	if creator == nil {
		if root != &s.root {
			throw("runtime: missing coroutine creator identity")
		}
		creator = s.registerPublicGoroutineLocked(caller, parent, nil)
	}
	callerpc = stacklessCoroSpawnSourcePC(gp, callerpc, callersp)
	if h := currentStacklessCoroHost(); h != nil && h.root == root {
		creator.labels = gp.labels
	}
	r := stacklessCoroGoroutines.taskFree
	if r != nil {
		stacklessCoroGoroutines.taskFree = r.next
		stacklessCoroGoroutines.taskFreeCount--
	} else if r = takeStacklessCoroGoroutineBurstLocked(); r == nil {
		r = new(stacklessCoroGoroutine)
	}
	*r = stacklessCoroGoroutine{root: task, leaf: task, scheduler: s,
		goid: sched.goidgen.Add(1), parentGoid: creator.goid, gopc: callerpc,
		labels: creator.labels}
	if debug.tracebackancestors > 0 {
		var previous []ancestorInfo
		if creator.ancestors != nil {
			previous = *creator.ancestors
		}
		n := min(len(previous)+1, int(debug.tracebackancestors))
		ancestors := make([]ancestorInfo, n)
		copy(ancestors[1:], previous)
		pcs := make([]uintptr, tracebackInnerFrames)
		var u unwinder
		skip, count := 0, 0
		rootPC := appendStacklessCoroSourceFrames(&u, parent, &skip, pcs, &count)
		if count != 0 {
			pcs[0] = callerpc - sys.PCQuantum + 1
		}
		if creator.publicG != nil && count < len(pcs) {
			systemstack(func() {
				var physical unwinder
				found := false
				for physical.init(creator.publicG, unwindSilentErrors); physical.valid() && count < len(pcs); physical.next() {
					f := physical.frame.fn
					info := stacklessCoroSourceFrame(f)
					if !found {
						found = f.entry() == rootPC
						continue
					}
					pc := physical.symPC()
					if info != nil && info.flags&abi.CoroSourceCopy != 0 {
						source := stacklessCoroSourceFunc(f, info)
						file, line := funcline1(f, pc, false)
						f, pc = source, stacklessCoroSourcePC(source, file, line)
					}
					appendStacklessCoroSourcePC(&u, f, pc, &skip, pcs, &count)
				}
			})
		}
		ancestors[0] = ancestorInfo{pcs: pcs[:count], goid: creator.goid, gopc: creator.gopc}
		r.ancestors = new([]ancestorInfo)
		*r.ancestors = ancestors
	}
	stacklessCoroInsertGoroutine(r)
	stacklessCoroGoroutines.unhosted.Add(1)
	sched.goroutinesCreated.Add(1)
	unlock(&stacklessCoroGoroutines.lock)
}

func coroAllocGoroutineID() uint64 {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp != nil {
		if pp.goidcache == pp.goidcacheend {
			pp.goidcache = sched.goidgen.Add(_GoidCacheBatch)
			pp.goidcache -= _GoidCacheBatch - 1
			pp.goidcacheend = pp.goidcache + _GoidCacheBatch
		}
		id := pp.goidcache
		pp.goidcache++
		releasem(mp)
		return id
	}
	releasem(mp)
	return sched.goidgen.Add(1)
}

func takeStacklessCoroGoroutineRecordMultiP() *stacklessCoroGoroutine {
	lock(&stacklessCoroGoroutines.lock)
	r := stacklessCoroGoroutines.taskFree
	if r != nil {
		stacklessCoroGoroutines.taskFree = r.next
		stacklessCoroGoroutines.taskFreeCount--
	} else {
		r = takeStacklessCoroGoroutineBurstLocked()
	}
	unlock(&stacklessCoroGoroutines.lock)
	if r != nil {
		r.next = nil
		return r
	}
	return new(stacklessCoroGoroutine)
}

func (s *stacklessCoroScheduler) registerGoroutineMultiP(task, parent *stacklessCoroTask,
	callerpc uintptr, callersp unsafe.Pointer) bool {
	if task.parent() != nil {
		throw("runtime: stackless coroutine record attached to non-root task")
	}
	root := stacklessCoroLogicalRoot(parent)
	creator := root.source()
	if creator == nil {
		return false
	}
	gp := getg()
	sourcePC := coroPSpawnSiteLookup(callerpc)
	if sourcePC == 0 {
		lock(&stacklessCoroGoroutines.lock)
		sourcePC = stacklessCoroSpawnSourcePC(gp, callerpc, callersp)
		unlock(&stacklessCoroGoroutines.lock)
		if sourcePC != 0 {
			coroPSpawnSiteStore(callerpc, sourcePC)
		}
	}
	labels := creator.labels
	if h := currentStacklessCoroHost(); h != nil && h.root == root {
		creator.labels = gp.labels
		labels = gp.labels
	}
	if r := takeStacklessCoroPooledGoroutine(); r != nil {
		goid := coroAllocGoroutineID()
		mp := acquirem()
		var ps *stacklessCoroPState
		mp, ps = ensureCoroPState(mp)
		r.next = nil
		r.publicNext = nil
		r.root = task
		r.leaf = task
		r.scheduler = s
		r.goid = goid
		r.parentGoid = creator.goid
		r.gopc = sourcePC
		r.labels = labels
		r.hosted = false
		r.hostG = nil
		r.public = false
		r.publicG = nil
		r.publicState = nil
		r.ancestors = nil
		r.profileBoundary = nil
		r.profileTrace = nil
		r.profileBefore = false
		r.shard = stacklessCoroPooledGShard
		task.setSource(r)
		r.inRegistry = true
		if ps != nil {
			ps.gUnhosted.Add(1)
		} else {
			stacklessCoroGoroutines.unhosted.Add(1)
		}
		releasem(mp)
		sched.goroutinesCreated.Add(1)
		return true
	}
	shardIdx := stacklessCoroGoroutineIndex(task, stacklessCoroGoroutineShardCount)
	sh := &stacklessCoroGoroutineShards[shardIdx]
	var r *stacklessCoroGoroutine
	// Try refilling from this shard's local free list before touching the global cache.
	lock(&sh.lock)
	young := sh.freeYoung
	if sh.free[young] != nil {
		r = sh.free[young]
		sh.free[young] = r.next
		sh.freeCount[young]--
	} else if old := young ^ 1; sh.free[old] != nil {
		r = sh.free[old]
		sh.free[old] = r.next
		sh.freeCount[old]--
	}
	unlock(&sh.lock)
	if r == nil {
		r = takeStacklessCoroGoroutineRecordMultiP()
	}
	*r = stacklessCoroGoroutine{
		root:       task,
		leaf:       task,
		scheduler:  s,
		goid:       coroAllocGoroutineID(),
		parentGoid: creator.goid,
		gopc:       sourcePC,
		labels:     labels,
		inRegistry: true,
		shard:      uint8(shardIdx + 1),
	}
	lock(&sh.lock)
	head := sh.head
	r.next = head
	if head != nil {
		head.publicNext = r
	}
	sh.head = r
	task.setSource(r)
	sh.unhosted.Add(1)
	sh.records.Add(1)
	unlock(&sh.lock)
	sched.goroutinesCreated.Add(1)
	return true
}

// Requires the registry lock. A direct call from a resume has a stable
// relationship between its machine return PC and its original go statement.
// An adapter can have many source callers and cannot share that cache key.
func stacklessCoroSpawnSourcePC(gp *g, callerPC uintptr, callerSP unsafe.Pointer) uintptr {
	sites := &stacklessCoroGoroutines.spawnSites
	site := &sites[(callerPC/sys.PCQuantum)%uintptr(len(sites))]
	if site.returnPC == callerPC && site.sourcePC != 0 {
		return site.sourcePC
	}
	f := findfunc(callerPC)
	if info := stacklessCoroSourceFrame(f); info != nil && info.flags&abi.CoroSourceResume != 0 {
		source := stacklessCoroSourceFunc(f, info)
		if source.valid() {
			pc := callerPC
			if pc > f.entry() {
				pc--
			}
			file, line := funcline1(f, pc, false)
			sourcePC := stacklessCoroSourcePC(source, file, line) + sys.PCQuantum
			*site = stacklessCoroSpawnSite{returnPC: callerPC, sourcePC: sourcePC}
			return sourcePC
		}
	}
	// Walk through adapters on g0 with a typed stack pointer.
	systemstack(func() {
		var u unwinder
		for u.initAt(callerPC, uintptr(callerSP), 0, gp, unwindSilentErrors); u.valid(); u.next() {
			f := u.frame.fn
			if info := stacklessCoroSourceFrame(f); info != nil && info.flags&abi.CoroSourceResume != 0 {
				source := stacklessCoroSourceFunc(f, info)
				if source.valid() {
					file, line := funcline1(f, u.symPC(), false)
					callerPC = stacklessCoroSourcePC(source, file, line) + sys.PCQuantum
					break
				}
			}
		}
	})
	return callerPC
}

func stacklessCoroSetLogicalLeaf(task *stacklessCoroTask, owner *g) {
	r := stacklessCoroLogicalRoot(task).source()
	if r == nil {
		return
	}
	if owner == nil && r.leaf == task && r.owner.Load() == nil && r.profileBoundary == nil && r.profileTrace == nil && !r.profileBefore {
		return
	}
	// Snapshot terminal metadata outside the signal handler only when this
	// scheduler actually holds cold terminal/defer/host entries.
	var boundary *stacklessCoroTask
	var trace *stacklessCoroPanicTrace
	var before bool
	if owner != nil {
		if s := task.context.scheduler; s != nil && s.terminalValues.Load() != nil {
			lock(&s.lock)
			for at := task; at != nil; at = at.parent() {
				if trace = s.terminalSourceTrace(at); trace != nil {
					boundary, before = at, true
					break
				}
				if trace = s.deferTrace(at); trace != nil {
					boundary = at
					break
				}
			}
			unlock(&s.lock)
		}
		if state := owner.stacklessCoro; state != nil {
			state.sourceTransition = true
		}
	}
	r.leaf = task
	r.profileBoundary, r.profileTrace, r.profileBefore = boundary, trace, before
	r.owner.Store(owner)
	// Runnable tasks have no owner. Only the executing G publishes a
	// signal-visible source record; queue publication must not touch a G.
	if owner != nil {
		if state := owner.stacklessCoro; state != nil {
			state.activeSource = r
			state.sourceTransition = false
		}
	}
}

func stacklessCoroClearLogicalOwner(task *stacklessCoroTask) {
	gp := getg()
	// Clear the signal-visible owner before any frame or record can be recycled.
	if state := gp.stacklessCoro; state != nil {
		state.activeSource = nil
		state.sourceTransition = false
	}
	r := stacklessCoroLogicalRoot(task).source()
	if r != nil && r.owner.Load() == gp {
		r.owner.Store(nil)
		r.profileBoundary, r.profileTrace, r.profileBefore = nil, nil, false
	}
}

// Complete a structured activation before clearing its frame or parent link.
// Its logical root remains visible at the caller until that root completes.
func stacklessCoroCompleteLogicalFrame(task *stacklessCoroTask) {
	if parent := task.parent(); parent != nil {
		if r := stacklessCoroLogicalRoot(parent).source(); r != nil {
			r.leaf = parent
			r.owner.Store(nil)
		}
		return
	}
	r := task.source()
	if r == nil {
		return
	}
	if r.shard != 0 {
		stacklessCoroCompleteShardedLogicalFrame(task, r)
		return
	}
	if !r.inRegistry {
		task.setSource(nil)
		pub := r.publicState
		if !r.public || pub == nil || pub.publicRecord.Load() != r {
			throw("runtime: missing private public coroutine identity")
		}
		gp := r.publicG
		pub.publicRecord.Store(nil)
		if (gp == getg() || gp == stacklessCoroPublicCaller(getg())) && pub.publicFree == nil {
			r.root = nil
			r.leaf = nil
			r.scheduler = nil
			r.publicG = nil
			r.publicState = nil
			if r.labels != nil {
				r.labels = nil
			}
			if r.ancestors != nil {
				r.ancestors = nil
			}
			if r.profileBoundary != nil {
				r.profileBoundary = nil
			}
			if r.profileTrace != nil {
				r.profileTrace = nil
			}
			r.profileBefore = false
			r.public = false
			r.profileCaller = r.profileCaller[:0]
			if r.owner.Load() != nil {
				r.owner.Store(nil)
			}
			pub.publicFree = r
		} else {
			callerPCs := r.profileCaller
			*r = stacklessCoroGoroutine{}
			clear(callerPCs)
			r.profileCaller = callerPCs[:0]
		}
		return
	}
	lock(&stacklessCoroGoroutines.lock)
	p := stacklessCoroGoroutineBucket(task)
	for *p != nil && *p != r {
		p = &(*p).next
	}
	if *p == r {
		*p = r.next
		task.setSource(nil)
		if !r.public && !r.hosted {
			stacklessCoroGoroutines.unhosted.Add(-1)
		}
		count := stacklessCoroGoroutines.records.Add(-1)
		public := r.public
		reusable := !public && !r.hosted && r.owner.Load() == nil
		if public {
			p := stacklessCoroPublicGoroutineBucket(r.publicG)
			for *p != r {
				if *p == nil {
					throw("runtime: missing public coroutine identity")
				}
				p = &(*p).publicNext
			}
			*p = r.publicNext
		}
		callerPCs := r.profileCaller
		*r = stacklessCoroGoroutine{}
		if public && stacklessCoroGoroutines.publicFreeCount < stacklessCoroWarmExecutorCount {
			// Retain only reusable storage for code addresses. No
			// stack pointer or caller object survives registration.
			clear(callerPCs)
			r.profileCaller = callerPCs[:0]
			r.next = stacklessCoroGoroutines.publicFree
			stacklessCoroGoroutines.publicFree = r
			stacklessCoroGoroutines.publicFreeCount++
		} else if reusable && stacklessCoroGoroutines.taskFreeCount < stacklessCoroTaskCacheSize {
			// Completion has detached the root and cleared the signal-
			// visible owner. Retain only the empty record for another go.
			r.next = stacklessCoroGoroutines.taskFree
			stacklessCoroGoroutines.taskFree = r
			stacklessCoroGoroutines.taskFreeCount++
		} else if reusable &&
			stacklessCoroGoroutines.taskBurstCount[0]+stacklessCoroGoroutines.taskBurstCount[1] < stacklessCoroGoroutineBurstCacheCap {
			young := stacklessCoroGoroutines.taskBurstYoung
			r.next = stacklessCoroGoroutines.taskBurst[young]
			stacklessCoroGoroutines.taskBurst[young] = r
			stacklessCoroGoroutines.taskBurstCount[young]++
		}
		if size := len(stacklessCoroGoroutines.largeBuckets); size != 0 && count <= int64(size/4) {
			stacklessCoroResizeGoroutineBuckets(size / 2)
		}
	}
	unlock(&stacklessCoroGoroutines.lock)
}

func stacklessCoroCompleteShardedLogicalFrame(task *stacklessCoroTask, r *stacklessCoroGoroutine) {
	if r.shard == stacklessCoroPooledGShard {
		mp := acquirem()
		var ps *stacklessCoroPState
		mp, ps = ensureCoroPState(mp)
		task.setSource(nil)
		r.inRegistry = false
		hosted := r.hosted
		reusable := !hosted && r.owner.Load() == nil
		r.root = nil
		r.leaf = nil
		r.scheduler = nil
		r.labels = nil
		r.hostG = nil
		r.hosted = false
		r.profileBoundary = nil
		r.profileTrace = nil
		r.profileBefore = false
		r.owner.Store(nil)
		if !hosted {
			if ps != nil {
				ps.gUnhosted.Add(-1)
			} else {
				stacklessCoroGoroutines.unhosted.Add(-1)
			}
		}
		if reusable {
			if ps != nil && ps.goroutineFreeCount < stacklessCoroPGoroutineCacheSize {
				r.next = ps.goroutineFree
				ps.goroutineFree = r
				ps.goroutineFreeCount++
			} else {
				var flush *stacklessCoroGoroutine
				if ps != nil {
					for i := 0; i < stacklessCoroPooledGChunkSize-1 && ps.goroutineFree != nil; i++ {
						item := ps.goroutineFree
						ps.goroutineFree = item.next
						ps.goroutineFreeCount--
						item.next = flush
						flush = item
					}
				}
				r.next = flush
				flush = r
				pushCoroPooledGChunk(flush)
			}
		}
		releasem(mp)
		return
	}
	sh := &stacklessCoroGoroutineShards[r.shard-1]
	lock(&sh.lock)
	if r.publicNext != nil {
		r.publicNext.next = r.next
	} else if sh.head == r {
		sh.head = r.next
	}
	if r.next != nil {
		r.next.publicNext = r.publicNext
	}
	task.setSource(nil)
	if !r.hosted {
		sh.unhosted.Add(-1)
	}
	sh.records.Add(-1)
	reusable := !r.hosted && r.owner.Load() == nil
	*r = stacklessCoroGoroutine{}
	if reusable && sh.freeCount[0]+sh.freeCount[1] < stacklessCoroGoroutineShardFreeCap {
		young := sh.freeYoung
		r.next = sh.free[young]
		sh.free[young] = r
		sh.freeCount[young]++
	}
	unlock(&sh.lock)
}

func stacklessCoroUnhostedGoroutines() int64 {
	total := stacklessCoroGoroutines.unhosted.Load()
	for i := range stacklessCoroGoroutineShards {
		total += stacklessCoroGoroutineShards[i].unhosted.Load()
	}
	mp := acquirem()
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		if ps := pp.coroP.Load(); ps != nil {
			total += ps.gUnhosted.Load()
		}
	}
	releasem(mp)
	if total < 0 {
		return 0
	}
	return total
}

func clearStacklessCoroPooledGCache(age bool) {
	assertWorldStopped()
	for i := range stacklessCoroPooledG.freeChunks {
		stacklessCoroPooledG.freeChunks[i].Store(nil)
	}
	n := stacklessCoroPooledG.count.Load()
	var newCount uint32
	var chunkHead *stacklessCoroGoroutine
	chunkN := 0
	chunkSlot := 0
	for i := uint32(0); i < n; i++ {
		r := stacklessCoroPooledG.slots[i]
		if r == nil {
			continue
		}
		if r.inRegistry || r.shard == stacklessCoroPooledGTransit {
			stacklessCoroPooledG.idleAge[i] = 0
			newCount = i + 1
			continue
		}
		if age {
			if stacklessCoroPooledG.idleAge[i] >= 1 {
				stacklessCoroPooledG.slots[i] = nil
				stacklessCoroPooledG.idleAge[i] = 0
				continue
			}
			stacklessCoroPooledG.idleAge[i] = 1
		}
		newCount = i + 1
		if chunkSlot < stacklessCoroPooledGChunkSlots {
			r.next = chunkHead
			chunkHead = r
			chunkN++
			if chunkN >= stacklessCoroPooledGChunkSize {
				stacklessCoroPooledG.freeChunks[chunkSlot].Store(chunkHead)
				chunkSlot++
				chunkHead = nil
				chunkN = 0
			}
		}
	}
	if chunkHead != nil && chunkSlot < stacklessCoroPooledGChunkSlots {
		stacklessCoroPooledG.freeChunks[chunkSlot].Store(chunkHead)
	}
	stacklessCoroPooledG.count.Store(newCount)
}

func takeStacklessCoroGoroutineBurstLocked() *stacklessCoroGoroutine {
	for age := uint8(0); age < 2; age++ {
		gen := stacklessCoroGoroutines.taskBurstYoung ^ age
		if r := stacklessCoroGoroutines.taskBurst[gen]; r != nil {
			stacklessCoroGoroutines.taskBurst[gen] = r.next
			stacklessCoroGoroutines.taskBurstCount[gen]--
			return r
		}
	}
	return nil
}

func clearStacklessCoroGoroutineBurstCache() {
	lock(&stacklessCoroGoroutines.lock)
	old := stacklessCoroGoroutines.taskBurstYoung ^ 1
	stacklessCoroGoroutines.taskBurst[old] = nil
	stacklessCoroGoroutines.taskBurstCount[old] = 0
	stacklessCoroGoroutines.taskBurstYoung = old
	unlock(&stacklessCoroGoroutines.lock)
	for i := range stacklessCoroGoroutineShards {
		sh := &stacklessCoroGoroutineShards[i]
		lock(&sh.lock)
		old := sh.freeYoung ^ 1
		sh.free[old] = nil
		sh.freeCount[old] = 0
		sh.freeYoung = old
		unlock(&sh.lock)
	}
}

func resetStacklessCoroGoroutineBurstCacheForTest() {
	lock(&stacklessCoroGoroutines.lock)
	stacklessCoroGoroutines.taskBurst = [2]*stacklessCoroGoroutine{}
	stacklessCoroGoroutines.taskBurstCount = [2]int{}
	stacklessCoroGoroutines.taskBurstYoung = 0
	unlock(&stacklessCoroGoroutines.lock)
	for i := range stacklessCoroGoroutineShards {
		sh := &stacklessCoroGoroutineShards[i]
		lock(&sh.lock)
		sh.free = [2]*stacklessCoroGoroutine{}
		sh.freeCount = [2]uint16{}
		sh.freeYoung = 0
		unlock(&sh.lock)
	}
	world := stopTheWorld(stwForTestResetDebugLog)
	clearStacklessCoroPooledGCache(true)
	clearStacklessCoroPooledGCache(true)
	startTheWorld(world)
}

func stacklessCoroRegisterHostStart(h *stacklessCoroHost) {
	lock(&stacklessCoroGoroutines.lock)
	h.startG = getg()
	h.startNext = stacklessCoroGoroutines.pending
	stacklessCoroGoroutines.pending = h
	unlock(&stacklessCoroGoroutines.lock)
}

// newproc1 calls this with preemption disabled, before publishing the G or
// emitting GoCreate. The logical identity is transferred exactly once; STW
// cannot observe both an unhosted record and its newly published ordinary G.
func stacklessCoroInitHostG(fn *funcval, caller, gp *g) uint64 {
	hasRecords := stacklessCoroGoroutines.records.Load() != 0 || stacklessCoroPooledG.count.Load() != 0
	if !hasRecords {
		for i := range stacklessCoroGoroutineShards {
			if stacklessCoroGoroutineShards[i].records.Load() != 0 {
				hasRecords = true
				break
			}
		}
	}
	if !hasRecords {
		return 0
	}
	if funcname(findfunc(fn.fn)) != "runtime.stacklessCoroHostStart.func1" {
		return 0
	}
	lock(&stacklessCoroGoroutines.lock)
	var matched *stacklessCoroHost
	for p := &stacklessCoroGoroutines.pending; *p != nil; p = &(*p).startNext {
		h := *p
		// newproc1 is synchronous with this creator's go statement. The
		// stable entry and creator identify the pending transfer without
		// depending on the compiler's private closure environment layout.
		if h.startG != caller {
			continue
		}
		*p = h.startNext
		h.startNext, h.startG = nil, nil
		matched = h
		break
	}
	if matched == nil {
		unlock(&stacklessCoroGoroutines.lock)
		return 0
	}
	r := matched.root.source()
	if r != nil && r.shard == stacklessCoroPooledGShard {
		if r.public || r.hosted {
			unlock(&stacklessCoroGoroutines.lock)
			throw("runtime: invalid coroutine host identity transfer")
		}
		r.hosted, r.hostG = true, gp
		stacklessCoroGoroutines.unhosted.Add(-1)
		gp.parentGoid, gp.gopc = r.parentGoid, r.gopc
		gp.labels, gp.ancestors = r.labels, r.ancestors
		goid := r.goid
		unlock(&stacklessCoroGoroutines.lock)
		return goid
	}
	if r != nil && r.shard != 0 {
		shardIdx := r.shard - 1
		unlock(&stacklessCoroGoroutines.lock)
		sh := &stacklessCoroGoroutineShards[shardIdx]
		lock(&sh.lock)
		if r.public || r.hosted {
			unlock(&sh.lock)
			throw("runtime: invalid coroutine host identity transfer")
		}
		r.hosted, r.hostG = true, gp
		sh.unhosted.Add(-1)
		gp.parentGoid, gp.gopc = r.parentGoid, r.gopc
		gp.labels, gp.ancestors = r.labels, r.ancestors
		goid := r.goid
		unlock(&sh.lock)
		return goid
	}
	r = stacklessCoroGoroutineFor(matched.root)
	if r == nil || r.public || r.hosted {
		unlock(&stacklessCoroGoroutines.lock)
		throw("runtime: invalid coroutine host identity transfer")
	}
	r.hosted, r.hostG = true, gp
	stacklessCoroGoroutines.unhosted.Add(-1)
	gp.parentGoid, gp.gopc = r.parentGoid, r.gopc
	gp.labels, gp.ancestors = r.labels, r.ancestors
	goid := r.goid
	unlock(&stacklessCoroGoroutines.lock)
	return goid
}

// The world is stopped. Owners cannot resume/recycle frames, and all registry
// mutations require a P with preemption disabled by a runtime mutex. Foreign
// calls retain their source frame until returning to Go with a P.
func stacklessCoroLogicalTrace(r *stacklessCoroGoroutine) *stacklessCoroPanicTrace {
	trace := new(stacklessCoroPanicTrace)
	trace.appendLogicalGoroutine(r)
	pcs := make([]uintptr, trace.pcCount)
	*trace = stacklessCoroPanicTrace{pcs: pcs}
	trace.appendLogicalGoroutine(r)
	return trace
}

// The world is stopped. A caller with nil pcs needs only the bounded text
// window; collecting it does not allocate a complete return-PC payload.
func (trace *stacklessCoroPanicTrace) appendLogicalGoroutine(r *stacklessCoroGoroutine) {
	var leafEntry, leafPC uintptr
	if owner := r.owner.Load(); owner != nil {
		var u unwinder
		for u.init(owner, unwindSilentErrors); u.valid(); u.next() {
			f := u.frame.fn
			if info := stacklessCoroSourceFrame(f); info != nil && info.flags&abi.CoroSourceResume != 0 {
				source := stacklessCoroSourceFunc(f, info)
				if !source.valid() {
					continue
				}
				file, line := funcline1(f, u.symPC(), false)
				leafEntry, leafPC = source.entry(), stacklessCoroSourcePC(source, file, line)
				break
			}
		}
	}
	first := true
	for task := r.leaf; task != nil; task = task.parent() {
		if previous := r.scheduler.terminalSourceTrace(task); previous != nil {
			trace.appendTrace(previous)
			break
		}
		pc, _ := walkStacklessCoroSourceTaskFrames(task, func(f funcInfo, pc uintptr) bool {
			if first && f.entry() == leafEntry {
				pc = leafPC
			}
			first = false
			trace.append(f, pc)
			return true
		})
		if pc != 0 {
			trace.rootPC = pc
		}
		if previous := r.scheduler.deferTrace(task); previous != nil {
			trace.appendTrace(previous)
			break
		}
	}
	if trace.pcCount == 0 && r.leaf.resume != nil {
		// Internal hand-written continuations have no compiler source
		// metadata. They still represent live logical goroutines and
		// must contribute a valid resume position to the profile.
		f := findfunc(stacklessCoroResumeIdentity(r.leaf.resume).fn)
		if f.valid() {
			trace.rootPC = f.entry()
			trace.append(f, f.entry())
		}
	}
	if r.public {
		trace.appendPublicCaller(r.publicG)
	}
}

// The world is stopped. Append the original ordinary caller after the
// logical source root exactly once. The public entry and optional coroRun
// adapter are implementation frames between that root and its caller.
func (trace *stacklessCoroPanicTrace) appendPublicCaller(gp *g) {
	var u unwinder
	bridges, afterDriver := true, false
	for u.init(gp, unwindSilentErrors); u.valid(); u.next() {
		f, pc := u.frame.fn, u.symPC()
		info := stacklessCoroSourceFrame(f)
		source := stacklessCoroSourceFunc(f, info)
		if bridges {
			if source.valid() && source.entry() == trace.rootPC {
				bridges = false
				continue
			}
			if funcname(f) == "runtime.coroRunFrame" {
				bridges, afterDriver = false, true
			}
			continue
		}
		if afterDriver {
			if funcname(f) == "runtime.coroRun" {
				continue
			}
			afterDriver = false
			if source.valid() && source.entry() == trace.rootPC {
				continue
			}
		}
		if info != nil && info.flags&abi.CoroSourceCopy != 0 && source.valid() {
			file, line := funcline1(f, pc, false)
			f, pc = source, stacklessCoroSourcePC(source, file, line)
		}
		trace.append(f, pc)
	}
}

// Requires STW. Capture heap activations while the frames and registry are
// stable. Ordinary callbacks retain their physical stack and bridge projection.
// Public entries use their original G even before allocating an ordinary host.
func stacklessCoroSnapshotRecord(gp *g) *stacklessCoroGoroutine {
	assertWorldStopped()
	state := stacklessCoroHostState(gp)
	if state != nil {
		if state.ordinaryDepth != 0 {
			return nil
		}
		if state.host != nil {
			r := stacklessCoroGoroutineFor(state.host.root)
			if r != nil && !r.public && r.hostG == gp {
				return r
			}
		}
	}
	for r := *stacklessCoroPublicGoroutineBucket(gp); r != nil; r = r.publicNext {
		if r.publicG != gp {
			continue
		}
		f := findfunc(stacklessCoroResumeIdentity(r.root.resume).fn)
		info := stacklessCoroSourceFrame(f)
		if info != nil && info.flags&abi.CoroSourceResume != 0 && stacklessCoroSourceFunc(f, info).valid() {
			return r
		}
		return nil
	}
	if pub := stacklessCoroPublicState(gp); pub != nil {
		if r := pub.publicRecord.Load(); r != nil && r.publicG == gp {
			f := findfunc(stacklessCoroResumeIdentity(r.root.resume).fn)
			info := stacklessCoroSourceFrame(f)
			if info != nil && info.flags&abi.CoroSourceResume != 0 && stacklessCoroSourceFunc(f, info).valid() {
				return r
			}
			return nil
		}
	}
	return nil
}

func stacklessCoroSnapshotGoroutine(gp *g) bool {
	return stacklessCoroSnapshotRecord(gp) != nil
}

// Requires STW. Visit both globally registered logical goroutines and private
// unnested public roots owned directly by ordinary Gs.
func visitStacklessCoroGoroutines(visit func(*stacklessCoroGoroutine)) {
	assertWorldStopped()
	for _, bucket := range stacklessCoroGoroutineBuckets() {
		for r := bucket; r != nil; r = r.next {
			visit(r)
		}
	}
	for i := range stacklessCoroGoroutineShards {
		for r := stacklessCoroGoroutineShards[i].head; r != nil; r = r.next {
			visit(r)
		}
	}
	nPooled := stacklessCoroPooledG.count.Load()
	for i := uint32(0); i < nPooled; i++ {
		if r := stacklessCoroPooledG.slots[i]; r != nil && r.inRegistry {
			visit(r)
		}
	}
	forEachGRace(func(gp *g) {
		if pub := stacklessCoroPublicState(gp); pub != nil {
			if r := pub.publicRecord.Load(); r != nil {
				visit(r)
			}
		}
	})
}

func stacklessCoroHasActiveGoroutines() bool {
	if stacklessCoroGoroutines.records.Load() != 0 {
		return true
	}
	for i := range stacklessCoroGoroutineShards {
		if stacklessCoroGoroutineShards[i].records.Load() != 0 {
			return true
		}
	}
	if stacklessCoroPooledG.count.Load() != 0 && stacklessCoroUnhostedGoroutines() != 0 {
		return true
	}
	found := false
	forEachGRace(func(gp *g) {
		if found {
			return
		}
		if pub := stacklessCoroPublicState(gp); pub != nil && pub.publicRecord.Load() != nil {
			found = true
		}
	})
	return found
}

// Requires STW. A host created after this snapshot is already satisfied by
// the unhosted source record. Existing hosts captured here are marked satisfied
// as well, so promotion or resumption cannot produce a duplicate profile entry.
func stacklessCoroGoroutineProfile(p []profilerecord.StackRecord, labels []unsafe.Pointer, concurrent bool) int {
	assertWorldStopped()
	n := 0
	visitStacklessCoroGoroutines(func(r *stacklessCoroGoroutine) {
		gp := r.hostG
		if r.public {
			gp = r.publicG
		}
		if gp != nil && stacklessCoroSnapshotRecord(gp) != r {
			return
		}
		trace := stacklessCoroLogicalTrace(r)
		depth := len(trace.pcs)
		if depth > int(debug.profstackdepth) {
			depth = int(debug.profstackdepth)
		}
		p[n].Stack = trace.pcs[:depth]
		if labels != nil {
			labels[n] = r.labels
			if gp != nil {
				labels[n] = gp.labels
			}
		}
		if gp != nil && concurrent {
			gp.goroutineProfiled.Store(goroutineProfileSatisfied)
		}
		n++
	})
	return n
}

// Only runtime.Stack(all=true) invokes this while the world is stopped.
func stacklessCoroTracebackGoroutines() {
	stacklessCoroPrintGoroutines(0)
}

// A regular user panic may stop the world before entering the fatal printer.
// A signal or throw must never infer this permission from global freeze state.
// The caller is committed to fatalpanic, even if another P fails to stop.
func stacklessCoroPrepareFatalTraceback() bool {
	level, all, crash := gotraceback()
	if level != 1 || !all || crash || debug.dontfreezetheworld != 0 ||
		panicking.Load() != 0 || !stacklessCoroHasActiveGoroutines() {
		return false
	}
	// Fatal printing must still work when another P cannot be preempted.
	// Avoid parking behind worldsema and bound the stop attempt itself.
	deadline := nanotime() + 100*1000*1000 // 100 ms
	for !cansemacquire(&worldsema) {
		if nanotime() >= deadline || panicking.Load() != 0 {
			return false
		}
		Gosched()
	}
	gp := getg()
	gp.m.preemptoff = stwAllGoroutinesStack.String()
	stopped := false
	systemstack(func() {
		stopped = stopTheWorldWithSemaUntil(stwAllGoroutinesStack, deadline).finishedStopping != 0
	})
	return stopped
}

// The caller completed a real STW for this panic. Preserve the usual physical
// callback stacks and print each suspended logical goroutine exactly once.
func stacklessCoroFatalTracebackOthers(me *g) {
	assertWorldStopped()
	tracebacksomeothers(me, func(other *g) bool {
		return !stacklessCoroSnapshotGoroutine(other)
	})
	stacklessCoroPrintGoroutines(me.goid)
}

// Use a bounded stack scratch trace, including when malloc is forbidden by
// fatalpanic. No mutable registry link is read without a completed real STW.
func stacklessCoroPrintGoroutines(skipID uint64) {
	assertWorldStopped()
	visitStacklessCoroGoroutines(func(r *stacklessCoroGoroutine) {
		if r.goid == skipID {
			return
		}
		gp := r.hostG
		if r.public {
			gp = r.publicG
		}
		if gp != nil && stacklessCoroSnapshotRecord(gp) != r {
			return
		}
		status := "waiting"
		if r.leaf.state == stacklessCoroTaskRunnable || r.leaf.state == stacklessCoroTaskNew {
			status = "runnable"
		} else if r.owner.Load() != nil {
			status = "running"
		}
		print("\ngoroutine ", r.goid, " [", status)
		if gp != nil && gp.lockedm != 0 {
			print(", locked to thread")
		}
		print("]")
		labels := r.labels
		if gp != nil {
			labels = gp.labels
		}
		printGoroutineLabels(labels)
		print(":\n")
		var trace stacklessCoroPanicTrace
		trace.appendLogicalGoroutine(r)
		for i := 0; i < trace.count; i++ {
			uf, ok := trace.at(i)
			if !ok {
				if i == tracebackInnerFrames {
					print("...", trace.count-tracebackInnerFrames-tracebackOuterFrames, " frames elided...\n")
				}
				continue
			}
			f := findfunc(uf.pc)
			iu, _ := newInlineUnwinder(f, uf.pc)
			sf := iu.srcFunc(uf)
			file, line := iu.fileLine(uf)
			name := sf.name()
			if !iu.isInlined(uf) {
				if source := stacklessCoroSourceName(f); source != "" {
					name = source
				}
			}
			printFuncName(name)
			print("(...)\n\t", file, ":", line, "\n")
		}
		if f := findfunc(r.gopc); f.valid() && r.goid != 1 {
			printcreatedby1(f, r.gopc, r.parentGoid)
		}
		if r.ancestors != nil {
			for _, ancestor := range *r.ancestors {
				printAncestorTraceback(ancestor)
			}
		}
	})
}
