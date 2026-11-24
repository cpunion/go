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
	publicNext       *stacklessCoroGoroutine
	root, leaf       *stacklessCoroTask
	scheduler        *stacklessCoroScheduler
	owner            *g
	publicG          *g
	hostG            *g
	goid, parentGoid uint64
	gopc             uintptr
	labels           unsafe.Pointer
	ancestors        *[]ancestorInfo
	public, hosted   bool
}

var stacklessCoroGoroutines struct {
	lock            mutex
	buckets         [256]*stacklessCoroGoroutine
	publicBuckets   [256]*stacklessCoroGoroutine
	publicFree      *stacklessCoroGoroutine
	publicFreeCount int
	pending         *stacklessCoroHost
	unhosted        atomic.Int64
	records         atomic.Int64
}

func init() {
	lockInit(&stacklessCoroGoroutines.lock, lockRankStacklessCoroCache)
}

func stacklessCoroGoroutineBucket(root *stacklessCoroTask) **stacklessCoroGoroutine {
	return &stacklessCoroGoroutines.buckets[(uintptr(unsafe.Pointer(root))>>3)%256]
}

func stacklessCoroPublicGoroutineBucket(gp *g) **stacklessCoroGoroutine {
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
	r := stacklessCoroGoroutines.publicFree
	if r != nil {
		stacklessCoroGoroutines.publicFree = r.next
		stacklessCoroGoroutines.publicFreeCount--
	} else {
		r = new(stacklessCoroGoroutine)
	}
	*r = stacklessCoroGoroutine{root: &s.root, leaf: leaf, scheduler: s,
		goid: gp.goid, parentGoid: gp.parentGoid, gopc: gp.gopc,
		labels: gp.labels, ancestors: gp.ancestors, publicG: gp, public: true}
	stacklessCoroInsertGoroutine(r)
	p := stacklessCoroPublicGoroutineBucket(gp)
	r.publicNext, *p = *p, r
	return r
}

func stacklessCoroLogicalRoot(task *stacklessCoroTask) *stacklessCoroTask {
	for task.parent != nil {
		task = task.parent
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
	p := stacklessCoroGoroutineBucket(r.root)
	r.next, *p = *p, r
	stacklessCoroGoroutines.records.Add(1)
}

// Called with s.lock before publishing a newly spawned task. Source roots
// already remember their public caller; hand-written roots do so lazily here.
// The caller can leave before independent children create further goroutines.
func (s *stacklessCoroScheduler) registerGoroutineLocked(task, parent *stacklessCoroTask, callerpc uintptr, callersp unsafe.Pointer) {
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
	// coroSpawn may add an adapter above the resume. Walk on g0 with a
	// typed stack pointer so stack growth cannot invalidate the cursor.
	systemstack(func() {
		var u unwinder
		for u.initAt(callerpc, uintptr(callersp), 0, gp, unwindSilentErrors); u.valid(); u.next() {
			f := u.frame.fn
			if info := stacklessCoroSourceFrame(f); info != nil && info.flags&abi.CoroSourceResume != 0 {
				source := stacklessCoroSourceFunc(f, info)
				if !source.valid() {
					continue
				}
				file, line := funcline1(f, u.symPC(), false)
				callerpc = stacklessCoroSourcePC(source, file, line) + sys.PCQuantum
				break
			}
		}
	})
	if h := currentStacklessCoroHost(); h != nil && h.root == root {
		creator.labels = gp.labels
	}
	r := &stacklessCoroGoroutine{root: task, leaf: task, scheduler: s,
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

func stacklessCoroSetLogicalLeaf(task *stacklessCoroTask, owner *g) {
	if stacklessCoroGoroutines.records.Load() == 0 {
		return
	}
	lock(&stacklessCoroGoroutines.lock)
	if r := stacklessCoroGoroutineFor(stacklessCoroLogicalRoot(task)); r != nil {
		r.leaf, r.owner = task, owner
	}
	unlock(&stacklessCoroGoroutines.lock)
}

func stacklessCoroClearLogicalOwner(task *stacklessCoroTask) {
	if stacklessCoroGoroutines.records.Load() == 0 {
		return
	}
	lock(&stacklessCoroGoroutines.lock)
	if r := stacklessCoroGoroutineFor(stacklessCoroLogicalRoot(task)); r != nil && r.owner == getg() {
		r.owner = nil
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
		if task.parent != nil {
			r.leaf, r.owner = task.parent, nil
		} else {
			*p = r.next
			if !r.public && !r.hosted {
				stacklessCoroGoroutines.unhosted.Add(-1)
			}
			stacklessCoroGoroutines.records.Add(-1)
			public := r.public
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
			*r = stacklessCoroGoroutine{}
			if public && stacklessCoroGoroutines.publicFreeCount < stacklessCoroWarmExecutorCount {
				r.next = stacklessCoroGoroutines.publicFree
				stacklessCoroGoroutines.publicFree = r
				stacklessCoroGoroutines.publicFreeCount++
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
		if r == nil || r.public || r.hosted {
			throw("runtime: invalid coroutine host identity transfer")
		}
		r.hosted, r.hostG = true, gp
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
	trace := new(stacklessCoroPanicTrace)
	var leafEntry, leafPC uintptr
	if r.owner != nil {
		var u unwinder
		for u.init(r.owner, unwindSilentErrors); u.valid(); u.next() {
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
	for pass := 0; pass < 2; pass++ {
		first := true
		for task := r.leaf; task != nil; task = task.parent {
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
		if pass == 0 {
			pcs := make([]uintptr, trace.pcCount)
			*trace = stacklessCoroPanicTrace{pcs: pcs}
		}
	}
	return trace
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
	for _, bucket := range stacklessCoroGoroutines.buckets {
		for r := bucket; r != nil; r = r.next {
			gp := r.hostG
			if r.public {
				gp = r.publicG
			}
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
// Fatal signal tracebacks must not allocate or inspect mutable registry links.
func stacklessCoroTracebackGoroutines() {
	assertWorldStopped()
	for _, bucket := range stacklessCoroGoroutines.buckets {
		for r := bucket; r != nil; r = r.next {
			gp := r.hostG
			if r.public {
				gp = r.publicG
			}
			if gp != nil && stacklessCoroSnapshotRecord(gp) != r {
				continue
			}
			status := "waiting"
			if r.leaf.state == stacklessCoroTaskRunnable || r.leaf.state == stacklessCoroTaskNew {
				status = "runnable"
			} else if r.owner != nil {
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
			trace := stacklessCoroLogicalTrace(r)
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
