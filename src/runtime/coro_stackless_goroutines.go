// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
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
	root, leaf       *stacklessCoroTask
	scheduler        *stacklessCoroScheduler
	owner            atomic.Pointer[g]
	physicalG        *g // public caller or promoted ordinary host
	goid, parentGoid uint64
	gopc             uintptr
	labels           unsafe.Pointer
	ancestors        *[]ancestorInfo
	profileBoundary  *stacklessCoroTask
	profileTrace     *stacklessCoroPanicTrace
	profileBefore    bool
	public           bool
}

// Only public entries need caller PCs or a link in the public-G index.
// Independent go tasks keep just the common source record.
type stacklessCoroPublicGoroutine struct {
	stacklessCoroGoroutine
	nextPublic    *stacklessCoroPublicGoroutine
	profileCaller []uintptr
}

// The caller has identified r as a public record.
func (r *stacklessCoroGoroutine) publicRecord() *stacklessCoroPublicGoroutine {
	return (*stacklessCoroPublicGoroutine)(unsafe.Pointer(r))
}

const (
	stacklessCoroInlineBuckets      = 256
	stacklessCoroMaxRetainedBuckets = 1 << 16
)

type stacklessCoroSpawnSite struct {
	returnPC, sourcePC uintptr
}

var stacklessCoroGoroutines struct {
	lock            mutex
	inlineBuckets   [stacklessCoroInlineBuckets]*stacklessCoroGoroutine
	largeBuckets    []*stacklessCoroGoroutine
	bucketsIdle     bool
	publicBuckets   [256]*stacklessCoroPublicGoroutine
	publicFree      *stacklessCoroPublicGoroutine
	publicFreeCount int
	taskFree        *stacklessCoroGoroutine
	taskFreeCount   int
	pending         *stacklessCoroHost
	spawnSites      [128]stacklessCoroSpawnSite
	unhosted        atomic.Int64
	records         atomic.Int64
}

func init() {
	lockInit(&stacklessCoroGoroutines.lock, lockRankStacklessCoroCache)
}

func stacklessCoroGoroutineBucket(root *stacklessCoroTask) **stacklessCoroGoroutine {
	buckets := stacklessCoroGoroutineBuckets()
	return &buckets[stacklessCoroGoroutineIndex(root, len(buckets))]
}

func stacklessCoroGoroutineIndex(root *stacklessCoroTask, n int) uintptr {
	addr := uintptr(unsafe.Pointer(root))
	// Keep the cheap address index for small populations. Larger tables
	// need to mix the address so allocator size classes use all buckets.
	if n == stacklessCoroInlineBuckets {
		return (addr >> 3) & (stacklessCoroInlineBuckets - 1)
	}
	return memhash(unsafe.Pointer(&addr), 0, unsafe.Sizeof(addr)) & uintptr(n-1)
}

// Readers hold the registry lock or run with the world stopped.
func stacklessCoroGoroutineBuckets() []*stacklessCoroGoroutine {
	if b := stacklessCoroGoroutines.largeBuckets; len(b) != 0 {
		return b
	}
	return stacklessCoroGoroutines.inlineBuckets[:]
}

// Rehash the typed links under the registry lock. The table grows with the
// live population so tasks of the same allocator size cannot accumulate in a
// few long lists. Keep a bounded backing array for successive bursts.
func stacklessCoroResizeGoroutineBuckets(size int) {
	old := stacklessCoroGoroutineBuckets()
	large := stacklessCoroGoroutines.largeBuckets
	var buckets []*stacklessCoroGoroutine
	switch {
	case size == stacklessCoroInlineBuckets:
		buckets = stacklessCoroGoroutines.inlineBuckets[:]
	case size <= cap(large) && (size >= len(large) || cap(large) <= stacklessCoroMaxRetainedBuckets):
		buckets = large[:size]
		if len(large) != 0 {
			stacklessCoroResizeLargeBuckets(large, buckets)
			stacklessCoroGoroutines.largeBuckets = buckets
			return
		}
	default:
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
		buckets = large[:0]
		if cap(buckets) > stacklessCoroMaxRetainedBuckets {
			buckets = nil
		}
		stacklessCoroGoroutines.bucketsIdle = false
	}
	stacklessCoroGoroutines.largeBuckets = buckets
}

// In-place resizing uses the same hash at either power of two. Growing
// moves only links whose extra index bit is set. Shrinking joins the two
// buckets with the same low bits, leaving the unused part of the array nil.
func stacklessCoroResizeLargeBuckets(old, buckets []*stacklessCoroGoroutine) {
	if len(buckets) > len(old) {
		for i := range old {
			for link := &old[i]; *link != nil; {
				r := *link
				at := stacklessCoroGoroutineIndex(r.root, len(buckets))
				if at == uintptr(i) {
					link = &r.next
				} else {
					*link = r.next
					r.next, buckets[at] = buckets[at], r
				}
			}
		}
		return
	}
	for i := len(buckets); i < len(old); i++ {
		head := old[i]
		old[i] = nil
		if head == nil {
			continue
		}
		tail := head
		for tail.next != nil {
			tail = tail.next
		}
		at := i & (len(buckets) - 1)
		tail.next, buckets[at] = buckets[at], head
	}
}

func stacklessCoroPublicGoroutineBucket(gp *g) **stacklessCoroPublicGoroutine {
	return &stacklessCoroGoroutines.publicBuckets[(uintptr(unsafe.Pointer(gp))>>3)%256]
}

// Register before the first native episode, including roots that never spawn
// an independent task. The original ordinary G owns the identity and count.
// Hand-written runtime continuations without source metadata need no record
// until they spawn a child. Reuse only completely cleared public records.
func (s *stacklessCoroScheduler) registerPublicGoroutine() {
	f := findfunc(stacklessCoroResumeIdentity(s.root.resume).fn)
	info := stacklessCoroSourceFrame(f)
	if info == nil || info.flags&abi.CoroSourceResume == 0 || !stacklessCoroSourceFunc(f, info).valid() {
		return
	}
	lock(&stacklessCoroGoroutines.lock)
	s.registerPublicGoroutineLocked(getg(), &s.root)
	unlock(&stacklessCoroGoroutines.lock)
}

func (s *stacklessCoroScheduler) registerPublicGoroutineLocked(gp *g, leaf *stacklessCoroTask) *stacklessCoroGoroutine {
	public := stacklessCoroGoroutines.publicFree
	if public != nil {
		stacklessCoroGoroutines.publicFree = public.nextPublic
		stacklessCoroGoroutines.publicFreeCount--
	} else {
		public = new(stacklessCoroPublicGoroutine)
	}
	r := &public.stacklessCoroGoroutine
	*r = stacklessCoroGoroutine{root: &s.root, leaf: leaf, scheduler: s,
		goid: gp.goid, parentGoid: gp.parentGoid, gopc: gp.gopc,
		labels: gp.labels, ancestors: gp.ancestors, physicalG: gp, public: true}
	if gp == getg() {
		f := findfunc(stacklessCoroResumeIdentity(s.root.resume).fn)
		if info := stacklessCoroSourceFrame(f); info != nil && info.flags&abi.CoroSourceResume != 0 {
			public.profileCaller = stacklessCoroCaptureProfileCaller(gp, s.root.resume, public.profileCaller)
		}
	}
	stacklessCoroInsertGoroutine(r)
	p := stacklessCoroPublicGoroutineBucket(gp)
	public.nextPublic, *p = *p, public
	return r
}

func stacklessCoroLogicalRoot(task *stacklessCoroTask) *stacklessCoroTask {
	for task.parent() != nil {
		task = task.parent()
	}
	return task
}

// Requires the registry lock, or a stopped world. No registry operation
// acquires a scheduler lock; the opposite order is used by completion.
func stacklessCoroGoroutineFor(root *stacklessCoroTask) *stacklessCoroGoroutine {
	for r := *stacklessCoroGoroutineBucket(root); r != nil; r = r.next {
		if r.root == root {
			return r
		}
	}
	return nil
}

func stacklessCoroInsertGoroutine(r *stacklessCoroGoroutine) {
	size := len(stacklessCoroGoroutineBuckets())
	if stacklessCoroGoroutines.records.Load() >= int64(size)*2 {
		stacklessCoroResizeGoroutineBuckets(size * 2)
	}
	p := stacklessCoroGoroutineBucket(r.root)
	r.next, *p = *p, r
	stacklessCoroGoroutines.records.Add(1)
}

// Called with s.lock before publishing a newly spawned task. Source roots
// already remember their public caller; hand-written roots do so lazily here.
// The caller can leave before independent children create further goroutines.
func (s *stacklessCoroScheduler) registerGoroutineLocked(task, parent *stacklessCoroTask, callerpc uintptr, callersp unsafe.Pointer) {
	tasks := [1]*stacklessCoroTask{task}
	s.registerGoroutinesLocked(tasks[:], parent, callerpc, callersp)
}

func (s *stacklessCoroScheduler) registerGoroutinesLocked(tasks []*stacklessCoroTask, parent *stacklessCoroTask, callerpc uintptr, callersp unsafe.Pointer) {
	gp := getg()
	caller := stacklessCoroPublicCaller(gp)
	lock(&stacklessCoroGoroutines.lock)
	root := stacklessCoroLogicalRoot(parent)
	creator := stacklessCoroGoroutineFor(root)
	if creator == nil {
		if root != &s.root {
			throw("runtime: missing coroutine creator identity")
		}
		creator = s.registerPublicGoroutineLocked(caller, parent)
	}
	callerpc = stacklessCoroSpawnSourcePC(gp, callerpc, callersp)
	if h := currentStacklessCoroHost(); h != nil && h.root == root {
		creator.labels = gp.labels
	}
	for _, task := range tasks {
		r := stacklessCoroGoroutines.taskFree
		if r == nil {
			r = takeStacklessCoroCachedRecordLocked()
			if r == nil {
				r = new(stacklessCoroGoroutine)
			}
		} else {
			stacklessCoroGoroutines.taskFree = r.next
			stacklessCoroGoroutines.taskFreeCount--
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
			if creator.public && count < len(pcs) {
				systemstack(func() {
					var physical unwinder
					found := false
					for physical.init(creator.physicalG, unwindSilentErrors); physical.valid() && count < len(pcs); physical.next() {
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
		task.setSource(r)
		stacklessCoroInsertGoroutine(r)
	}
	stacklessCoroGoroutines.unhosted.Add(int64(len(tasks)))
	sched.goroutinesCreated.Add(int64(len(tasks)))
	unlock(&stacklessCoroGoroutines.lock)
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
	if !raceenabled && owner != nil && owner.stackIsFixed() {
		if record := task.source(); record != nil && !task.context.scheduler.hasTerminalRecords() {
			// Registration and structured completion already made the
			// independent task the leaf. It keeps the record alive until
			// this executor clears ownership and completes the task.
			record.owner.Store(owner)
			state := owner.stacklessCoro
			state.activeSource = record
			state.activeTerminalSource = false
			state.sourceTransition = false
			return
		}
	}
	if stacklessCoroGoroutines.records.Load() == 0 {
		return
	}
	// Snapshot terminal metadata outside the signal handler. The scheduler
	// lock protects its cold list; the retained trace is immutable. Queue
	// publication already holds that lock and passes no owner.
	var boundary *stacklessCoroTask
	var trace *stacklessCoroPanicTrace
	var before bool
	if owner != nil {
		s := task.context.scheduler
		if s.hasTerminalRecords() {
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
	}
	lock(&stacklessCoroGoroutines.lock)
	if r := stacklessCoroGoroutineFor(stacklessCoroLogicalRoot(task)); r != nil {
		r.owner.Store(owner)
		r.leaf = task
		r.profileBoundary, r.profileTrace, r.profileBefore = boundary, trace, before
		// Runnable tasks have no owner. Only the executing G publishes a
		// signal-visible source record; queue publication must not touch a G.
		if owner != nil {
			if state := owner.stacklessCoro; state != nil {
				state.activeSource = r
				state.activeTerminalSource = trace != nil
				state.sourceTransition = false
			}
		}
	}
	unlock(&stacklessCoroGoroutines.lock)
}

func stacklessCoroClearLogicalOwner(task *stacklessCoroTask, native bool) {
	gp := getg()
	// Detach the signal-visible reference before a record can be recycled.
	if state := gp.stacklessCoro; state != nil {
		record, terminal := state.activeSource, state.activeTerminalSource
		state.activeSource = nil
		state.activeTerminalSource = false
		state.sourceTransition = false
		if native && record != nil && !terminal {
			// A structured child may have taken ownership while the parent
			// was returning Wait. Only clear our own publication; the
			// parent's active resume keeps this record live until then.
			record.owner.CompareAndSwap(gp, nil)
			return
		}
	}
	if stacklessCoroGoroutines.records.Load() == 0 {
		return
	}
	lock(&stacklessCoroGoroutines.lock)
	if r := stacklessCoroGoroutineFor(stacklessCoroLogicalRoot(task)); r != nil && r.owner.Load() == gp {
		r.owner.Store(nil)
		r.profileBoundary, r.profileTrace, r.profileBefore = nil, nil, false
	}
	unlock(&stacklessCoroGoroutines.lock)
}

// Complete a structured activation before clearing its frame or parent link.
// Its logical root remains visible at the caller until that root completes.
func stacklessCoroCompleteLogicalFrame(task *stacklessCoroTask) {
	if stacklessCoroGoroutines.records.Load() == 0 {
		return
	}
	lock(&stacklessCoroGoroutines.lock)
	root := stacklessCoroLogicalRoot(task)
	p := stacklessCoroGoroutineBucket(root)
	for *p != nil && (*p).root != root {
		p = &(*p).next
	}
	if r := *p; r != nil {
		if task.parent() != nil {
			r.leaf = task.parent()
			r.owner.Store(nil)
		} else {
			*p = r.next
			task.parentOrSource = nil
			if r.physicalG == nil {
				stacklessCoroGoroutines.unhosted.Add(-1)
			}
			count := stacklessCoroGoroutines.records.Add(-1)
			public := r.public
			reusable := r.physicalG == nil && r.owner.Load() == nil
			if public {
				record := r.publicRecord()
				p := stacklessCoroPublicGoroutineBucket(r.physicalG)
				for *p != record {
					if *p == nil {
						throw("runtime: missing public coroutine identity")
					}
					p = &(*p).nextPublic
				}
				*p = record.nextPublic
				record.nextPublic = nil
			}
			*r = stacklessCoroGoroutine{}
			if public && stacklessCoroGoroutines.publicFreeCount < stacklessCoroWarmExecutorCount {
				// Retain only reusable storage for code addresses.
				record := r.publicRecord()
				clear(record.profileCaller)
				record.profileCaller = record.profileCaller[:0]
				record.nextPublic = stacklessCoroGoroutines.publicFree
				stacklessCoroGoroutines.publicFree = record
				stacklessCoroGoroutines.publicFreeCount++
			} else if reusable && stacklessCoroGoroutines.taskFreeCount < stacklessCoroTaskCacheSize {
				// Completion has detached the root and cleared the signal-
				// visible owner. Retain only the empty record for another go.
				r.next = stacklessCoroGoroutines.taskFree
				stacklessCoroGoroutines.taskFree = r
				stacklessCoroGoroutines.taskFreeCount++
			} else if reusable {
				cacheStacklessCoroRecordLocked(r)
			}
			if size := len(stacklessCoroGoroutines.largeBuckets); size != 0 && count <= int64(size/4) {
				stacklessCoroResizeGoroutineBuckets(size / 2)
			}
		}
	}
	unlock(&stacklessCoroGoroutines.lock)
}

func stacklessCoroUnhostedGoroutines() int64 {
	return stacklessCoroGoroutines.unhosted.Load()
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
	if stacklessCoroGoroutines.records.Load() == 0 {
		return 0
	}
	if funcname(findfunc(fn.fn)) != "runtime.stacklessCoroHostStart.func1" {
		return 0
	}
	lock(&stacklessCoroGoroutines.lock)
	defer unlock(&stacklessCoroGoroutines.lock)
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
		r := stacklessCoroGoroutineFor(h.root)
		if r == nil || r.physicalG != nil {
			throw("runtime: invalid coroutine host identity transfer")
		}
		r.physicalG = gp
		stacklessCoroGoroutines.unhosted.Add(-1)
		gp.parentGoid, gp.gopc = r.parentGoid, r.gopc
		gp.labels, gp.ancestors = r.labels, r.ancestors
		return r.goid
	}
	return 0
}

// The world is stopped. Owners cannot resume/recycle frames, and all registry
// mutations require a P with preemption disabled by a runtime mutex. Foreign
// calls retain their source frame until returning to Go with a P.
func stacklessCoroLogicalTrace(r *stacklessCoroGoroutine) *stacklessCoroPanicTrace {
	frames := new(stacklessCoroPanicText)
	trace := &stacklessCoroPanicTrace{frames: frames[:]}
	trace.appendLogicalGoroutine(r)
	pcs := make([]uintptr, trace.pcCount)
	*trace = stacklessCoroPanicTrace{frames: frames[:], pcs: pcs}
	trace.appendLogicalGoroutine(r)
	if trace.pcCount != len(pcs) {
		throw("runtime: incomplete coroutine goroutine PC capture")
	}
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
		trace.appendPublicCaller(r.physicalG)
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
			if r != nil && !r.public && r.physicalG == gp {
				return r
			}
		}
	}
	for record := *stacklessCoroPublicGoroutineBucket(gp); record != nil; record = record.nextPublic {
		r := &record.stacklessCoroGoroutine
		if r.physicalG != gp {
			continue
		}
		f := findfunc(stacklessCoroResumeIdentity(r.root.resume).fn)
		info := stacklessCoroSourceFrame(f)
		if info != nil && info.flags&abi.CoroSourceResume != 0 && stacklessCoroSourceFunc(f, info).valid() {
			return r
		}
		return nil
	}
	return nil
}

func stacklessCoroSnapshotGoroutine(gp *g) bool {
	return stacklessCoroSnapshotRecord(gp) != nil
}

// Requires STW. A host created after this snapshot is already satisfied by
// the unhosted source record. Existing hosts captured here are marked satisfied
// as well, so promotion or resumption cannot produce a duplicate profile entry.
func stacklessCoroGoroutineProfile(p []profilerecord.StackRecord, labels []unsafe.Pointer, concurrent bool) int {
	assertWorldStopped()
	n := 0
	for _, bucket := range stacklessCoroGoroutineBuckets() {
		for r := bucket; r != nil; r = r.next {
			gp := r.physicalG
			if gp != nil && stacklessCoroSnapshotRecord(gp) != r {
				continue
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
		}
	}
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
		panicking.Load() != 0 || stacklessCoroGoroutines.records.Load() == 0 {
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
	for _, bucket := range stacklessCoroGoroutineBuckets() {
		for r := bucket; r != nil; r = r.next {
			if r.goid == skipID {
				continue
			}
			gp := r.physicalG
			if gp != nil && stacklessCoroSnapshotRecord(gp) != r {
				continue
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
			var frames stacklessCoroPanicText
			trace := stacklessCoroPanicTrace{frames: frames[:]}
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
		}
	}
}
