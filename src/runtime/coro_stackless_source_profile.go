// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/sys"
	"unsafe"
)

const (
	stacklessCoroGoStartProfileDepth = 2 * (abi.StackSmall + goarch.PtrSize)
	stacklessCoroProfileCacheDepth   = 1<<16 - 1 // offsets fit in uint16
)

// A freshly launched goroutine may enter its source root directly or through
// an elided go wrapper. Verify both the return PCs and the actual stack top
// before recording goexit as its entire ordinary ancestry. A recursive call
// may have the same PCs as the initial call but has a different stack depth.
func stacklessCoroProfileGoStart(gp *g, source funcInfo, callerPC uintptr, callerSP unsafe.Pointer) uintptr {
	if GOARCH != "amd64" && GOARCH != "arm64" {
		return 0
	}
	if findfunc(callerPC) != source || source.funcID != abi.FuncIDNormal {
		return 0
	}
	goexitPC := abi.FuncPCABI0(goexit) + sys.PCQuantum
	pc, atTop, sp := stacklessCoroProfileCallerLink(gp, source, callerPC, callerSP)
	if atTop && pc == goexitPC {
		return pc
	}
	f := findfunc(pc)
	if !f.valid() || f.funcID != abi.FuncIDWrapper || f.entry() != gp.startpc {
		return 0
	}
	// The ordinary profile path translates source copies before eliding
	// wrappers. An inlined caller must still appear in the profile.
	profileFunc := f
	if info := stacklessCoroSourceFrame(f); info != nil && info.flags&abi.CoroSourceCopy != 0 {
		if source := stacklessCoroSourceFunc(f, info); source.valid() {
			profileFunc = source
		}
	}
	if profileFunc.funcID != abi.FuncIDWrapper || funcdata(profileFunc, abi.FUNCDATA_InlTree) != nil {
		return 0
	}
	pc, atTop, _ = stacklessCoroProfileCallerLink(gp, f, pc, sp)
	if atTop && pc == goexitPC {
		return pc
	}
	return 0
}

// Read the link of a regular Go call frame using the same saved PC and SP
// rules as the unwinder. Keep the SP typed across the pcsp lookup and read
// stack bounds and the link together while the running G cannot be preempted.
func stacklessCoroProfileCallerLink(gp *g, f funcInfo, pc uintptr, sp unsafe.Pointer) (uintptr, bool, unsafe.Pointer) {
	if f.pcsp == 0 || f.flag&(abi.FuncFlagTopFrame|abi.FuncFlagSPWrite|abi.FuncFlagAsm) != 0 {
		return 0, false, nil
	}
	delta := funcspdelta(f, pc)
	if delta <= 0 {
		return 0, false, nil
	}
	size := uintptr(delta)
	if !usesLR {
		size += goarch.PtrSize
	}
	mp := acquirem()
	addr, top := uintptr(sp), gp.stktopsp
	if addr < gp.stack.lo || addr >= top || size > top-addr || top >= gp.stack.hi {
		releasem(mp)
		return 0, false, nil
	}
	callerSP := unsafe.Add(sp, size)
	if !usesLR {
		sp = unsafe.Add(sp, size-goarch.PtrSize)
	}
	returnPC := *(*uintptr)(sp)
	atTop := uintptr(callerSP) == top
	releasem(mp)
	return returnPC, atTop, callerSP
}

// A complete ordinary Go stack can be reidentified by its entry PC, stack
// depth and the saved return PC at every frame. Cached offsets are relative to
// the starting SP, so stack moves do not invalidate them. The cache is owned
// by its P and contains only integers. The profiler reads the published
// public record as before.
type stacklessCoroCallerProfile struct {
	rootPC, callerPC uintptr
	returnPCs        [12]uintptr
	pcs              [16]uintptr
	offsets          [12]uint16
	depth            uint16
	nReturn, nPC     uint8
}

func stacklessCoroCachedProfileCaller(gp *g, rootPC, callerPC uintptr, sp unsafe.Pointer, pcs []uintptr) int {
	if !framepointer_enabled || callerPC == 0 || gp.stktopsp-uintptr(sp) > stacklessCoroProfileCacheDepth {
		return stacklessCoroCaptureProfileCaller(gp, rootPC, callerPC, sp, pcs, nil)
	}
	mp := acquirem()
	if pp := mp.p.ptr(); pp != nil && mp.curg == gp && uintptr(sp) >= gp.stack.lo && gp.stktopsp < gp.stack.hi {
		depth := gp.stktopsp - uintptr(sp)
		for i := range pp.callerProfiles {
			if n := pp.callerProfiles[i].load(rootPC, callerPC, depth, sp, pcs); n > 0 {
				pp.profileMisses = 0
				releasem(mp)
				return n
			}
		}
		// Keep active entries through occasional misses. Once both slots
		// have been used, look for consecutive misses before replacing one.
		if pp.callerProfiles[len(pp.callerProfiles)-1].nPC != 0 && pp.profileMisses < uint8(len(pp.callerProfiles)) {
			pp.profileMisses++
			releasem(mp)
			return stacklessCoroCaptureProfileCaller(gp, rootPC, callerPC, sp, pcs, nil)
		}
		pp.profileMisses = 0
	}
	releasem(mp)
	return stacklessCoroRememberProfileCaller(gp, rootPC, callerPC, sp, pcs)
}

//go:nosplit
func (c *stacklessCoroCallerProfile) load(rootPC, callerPC, depth uintptr, sp unsafe.Pointer, pcs []uintptr) int {
	if c.callerPC != callerPC || c.rootPC != rootPC || uintptr(c.depth) != depth {
		return 0
	}
	for i, offset := range c.offsets[:c.nReturn] {
		if *(*uintptr)(unsafe.Add(sp, uintptr(offset))) != c.returnPCs[i] {
			return 0
		}
	}
	return copy(pcs, c.pcs[:c.nPC])
}

//go:noinline
func stacklessCoroRememberProfileCaller(gp *g, rootPC, callerPC uintptr, sp unsafe.Pointer, pcs []uintptr) int {
	c := stacklessCoroCallerProfile{rootPC: rootPC, callerPC: callerPC}
	n := stacklessCoroCaptureProfileCaller(gp, rootPC, callerPC, sp, pcs, &c)
	if c.depth != 0 && n != 0 && n <= len(c.pcs) {
		c.nPC = uint8(copy(c.pcs[:], pcs[:n]))
		mp := acquirem()
		if pp := mp.p.ptr(); pp != nil {
			pp.callerProfiles[pp.nextProfile] = c
			pp.nextProfile = (pp.nextProfile + 1) % uint8(len(pp.callerProfiles))
		}
		releasem(mp)
	}
	return n
}

// Record a link only when unwinder.next takes its ordinary Go call path.
// A recipe is complete only at the real goexit frame of this G.
func (c *stacklessCoroCallerProfile) recordLink(u *unwinder, start, top uintptr) bool {
	f := &u.frame
	if f.sp == top && f.pc == abi.FuncPCABI0(goexit)+sys.PCQuantum && f.lr == 0 {
		c.depth = uint16(top - start)
		return false
	}
	if f.lr == 0 || f.fp <= f.sp || f.fp > top || int(c.nReturn) == len(c.offsets) ||
		f.fn.flag&(abi.FuncFlagAsm|abi.FuncFlagSPWrite|abi.FuncFlagTopFrame) != 0 ||
		f.fn.funcID == abi.FuncID_cgocallback || isInjectedCall(f.fn.funcID) {
		return false
	}
	addr := f.sp
	if !usesLR {
		addr = f.fp - goarch.PtrSize
	}
	if addr < start || addr > top-goarch.PtrSize || *(*uintptr)(unsafe.Pointer(addr)) != f.lr {
		return false
	}
	c.offsets[c.nReturn] = uint16(addr - start)
	c.returnPCs[c.nReturn] = f.lr
	c.nReturn++
	return true
}

// Capture the public caller while it owns its ordinary stack. A later CPU
// signal must not unwind that waiting G: concurrent GC may be moving its stack.
// PCs are immutable code addresses and need no stack pointer or registry lookup.
// CPU profiles have a bounded PC buffer, so retaining this many caller PCs is
// sufficient even when the logical source chain itself is shallow.
func stacklessCoroCaptureProfileCaller(gp *g, rootPC, callerPC uintptr, callerSP unsafe.Pointer, pcs []uintptr, cached *stacklessCoroCallerProfile) int {
	n, skip := 0, 0
	systemstack(func() {
		var u unwinder
		bridges := callerPC == 0
		afterDriver := !bridges
		start, top := uintptr(callerSP), gp.stktopsp
		recording := cached != nil && start >= gp.stack.lo && start < top &&
			top < gp.stack.hi && top-start <= stacklessCoroProfileCacheDepth
		if bridges {
			u.init(gp, unwindSilentErrors)
		} else {
			// callerSP remains a pointer until we are on g0, so a stack
			// move between the entry and this call can adjust it.
			u.initAt(callerPC, uintptr(callerSP), 0, gp, unwindSilentErrors)
		}
		for ; u.valid() && n < len(pcs); u.next() {
			if recording {
				recording = cached.recordLink(&u, start, top)
			}
			f, pc := u.frame.fn, u.symPC()
			info := stacklessCoroSourceFrame(f)
			source := stacklessCoroSourceFunc(f, info)
			if bridges {
				if source.valid() && source.entry() == rootPC {
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
				if source.valid() && source.entry() == rootPC {
					continue
				}
			}
			if info != nil && info.flags&abi.CoroSourceCopy != 0 && source.valid() {
				file, line := funcline1(f, pc, false)
				f, pc = source, stacklessCoroSourcePC(source, file, line)
			}
			appendStacklessCoroSourcePC(&u, f, pc, &skip, pcs, &n)
		}
	})
	return n
}

// The executing G publishes its record before calling a source resume and
// clears it before completing or recycling any activation. The signal handler
// can read this owner directly; the shared registry still requires a lock/STW.
func stacklessCoroActiveSource(gp *g) *stacklessCoroGoroutine {
	// Some platforms sample a different M without stopping it. This source
	// walk is valid only for the interrupted/current M's own running G.
	if gp == nil || gp.m != getg().m || gp != getg().m.curg || gp.stacklessCoro == nil {
		return nil
	}
	r := gp.stacklessCoro.activeSource
	if r == nil || r.owner.Load() != gp || gp.stacklessCoro.sourceTransition {
		return nil
	}
	return r
}

func stacklessCoroProfileTransition(gp *g, mp *m, sampled funcInfo) bool {
	if mp == nil || mp != getg().m {
		return false
	}
	if gp == mp.g0 {
		gp = mp.curg
	}
	if gp == nil || gp != mp.curg || gp.stacklessCoro == nil {
		return false
	}
	state := gp.stacklessCoro
	if state.activeSource == nil {
		return false
	}
	if mp.locks != 0 || state.sourceTransition || state.activeSource.owner.Load() != gp {
		return true
	}
	if !sampled.valid() {
		return false
	}
	// Signals can arrive in the helper's prologue before the transition
	// marker is set, or after clearing it on return to the generated resume.
	// These PCs still belong to the transition.
	switch sampled.entry() {
	case abi.FuncPCABIInternal(coroAwaitSelfFrame),
		abi.FuncPCABIInternal(coroAdvanceSelfChunk),
		abi.FuncPCABIInternal(coroPopSelfChunk),
		abi.FuncPCABIInternal(coroCompleteSelfFrame),
		abi.FuncPCABIInternal(coroAdvanceFusedChunk),
		abi.FuncPCABIInternal(coroPopFusedChunk),
		abi.FuncPCABIInternal(coroAwaitFusedFrame),
		abi.FuncPCABIInternal(coroCompleteFusedFrame),
		abi.FuncPCABIInternal(restoreStacklessCoroFusedParent),
		abi.FuncPCABIInternal(coroAwaitFrame),
		abi.FuncPCABIInternal(coroDeferAwaitFrame),
		abi.FuncPCABIInternal(stacklessCoroBeginProfileTransition),
		abi.FuncPCABIInternal(coroProfileResume):
		return true
	}
	return false
}

// A helper can finish publishing a different activation before returning to
// the generated resume. Keep this per-owner marker through that interval:
// matching source function entries cannot distinguish recursive activations.
//
//go:nosplit
func stacklessCoroBeginProfileTransition() {
	if state := getg().stacklessCoro; state != nil && state.activeSource != nil {
		state.sourceTransition = true
	}
}

// Fresh resume invocations reset this marker in SetLogicalLeaf. A compiler
// local self-switch instead reloads the typed frame within one invocation,
// and calls this helper only after that reload has selected the new frame.
//
//go:nosplit
func coroProfileResume() {
	if state := getg().stacklessCoro; state != nil {
		state.sourceTransition = false
	}
}

func stacklessCoroProfileLabels(gp *g) *unsafe.Pointer {
	if r := stacklessCoroActiveSource(gp); r != nil {
		if r.public || gp == r.physicalG {
			return &r.physicalG.labels
		}
		return &r.labels
	}
	return &gp.labels
}

func stacklessCoroActiveSourcePCs(u *unwinder, source funcInfo, skip *int, buf []uintptr, n *int) bool {
	gp := u.g.ptr()
	mp := getg().m
	if mp.curg != gp || mp.locks != 0 {
		return false
	}
	r := stacklessCoroActiveSource(gp)
	if r == nil {
		return false
	}
	file, line := funcline1(u.frame.fn, u.symPC(), false)
	leafPC := stacklessCoroSourcePC(source, file, line)
	contains := false
	walkStacklessCoroProfileFrames(r, func(f funcInfo, pc uintptr) bool {
		contains = f.entry() == source.entry()
		return !contains
	})
	started := !contains
	if !contains {
		// Completion may already have restored the parent's context while
		// the child's resume is executing its return. Its physical PC is
		// still valid, but its old frame storage has been released. Keep
		// that source PC and then walk only the newly published parent.
		appendStacklessCoroSourcePC(u, source, leafPC, skip, buf, n)
	}
	walkStacklessCoroProfileFrames(r, func(f funcInfo, pc uintptr) bool {
		if !started {
			// A resume can publish its next heterogeneous child immediately
			// before returning Switch. Begin at the source activation that
			// actually owns the interrupted PC, not the not-yet-called child.
			if f.entry() != source.entry() {
				return true
			}
			started = true
			pc = leafPC
		}
		appendStacklessCoroSourcePC(u, f, pc, skip, buf, n)
		return *n < len(buf)
	})
	if trace := r.profileTrace; trace != nil {
		boundary := stacklessCoroPanicPC
		if trace.goexit {
			boundary = stacklessCoroGoexitPC
		}
		appendStacklessCoroSourcePC(u, findfunc(boundary), boundary, skip, buf, n)
		appendStacklessCoroPanicPCs(u, trace, skip, buf, n)
	}
	if r.public {
		for _, pc := range r.publicRecord().profileCaller {
			if *n == len(buf) {
				break
			}
			if *skip != 0 {
				*skip--
			} else {
				buf[*n] = pc
				*n++
			}
		}
	}
	return true
}

// The signal handler walks only its owner's live source chain. The boundary
// and immutable terminal PCs were published before this resume started; no
// scheduler list lookup or lock is permitted here.
func walkStacklessCoroProfileFrames(r *stacklessCoroGoroutine, visit func(funcInfo, uintptr) bool) {
	for task := r.leaf; task != nil; task = task.parent() {
		if task == r.profileBoundary && r.profileBefore {
			return
		}
		if _, complete := walkStacklessCoroSourceTaskFrames(task, visit); !complete {
			return
		}
		if task == r.profileBoundary {
			return
		}
	}
}
