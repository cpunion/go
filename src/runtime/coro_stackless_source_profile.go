// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"unsafe"
)

// Capture the public caller while it owns its ordinary stack. A later CPU
// signal must not unwind that waiting G: concurrent GC may be moving its stack.
// PCs are immutable code addresses and need no stack pointer or registry lookup.
// CPU profiles have a bounded PC buffer, so retaining this many caller PCs is
// sufficient even when the logical source chain itself is shallow.
func stacklessCoroCaptureProfileCaller(gp *g, resume stacklessCoroResume, reuse []uintptr) []uintptr {
	if gp != getg() {
		return reuse[:0]
	}
	f := findfunc(stacklessCoroResumeIdentity(resume).fn)
	info := stacklessCoroSourceFrame(f)
	if info == nil || info.flags&abi.CoroSourceResume == 0 {
		return reuse[:0]
	}
	source := stacklessCoroSourceFunc(f, info)
	if !source.valid() {
		return reuse[:0]
	}
	rootPC := source.entry()
	var pcs [maxCPUProfStack]uintptr
	n, skip := 0, 0
	systemstack(func() {
		var u unwinder
		bridges, afterDriver := true, false
		for u.init(gp, unwindSilentErrors); u.valid() && n < len(pcs); u.next() {
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
			appendStacklessCoroSourcePC(&u, f, pc, &skip, pcs[:], &n)
		}
	})
	if n != 0 && cap(reuse) == 0 {
		reuse = make([]uintptr, 0, maxCPUProfStack)
	}
	return append(reuse[:0], pcs[:n]...)
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

func stacklessCoroProfileTransition(gp *g, mp *m) bool {
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
	return state.activeSource != nil && (mp.locks != 0 ||
		state.sourceTransition || state.activeSource.owner.Load() != gp)
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
	for task := r.leaf; task != nil; task = task.parent {
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
