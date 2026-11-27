// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/cpu"
	"internal/profilerecord"
	"internal/runtime/atomic"
	"unsafe"
)

const (
	coroParkedShardCount = 64
	coroPanicTraceSlots  = 8
	coroMaxTraceFrames   = 32
)

// coroParkedShards tracks currently-parked unhosted stackless coroutines
// (leaf task of each parked logical goroutine) for O(1) insert/remove and
// lock-free STW enumeration in runtime.GoroutineProfile and runtime.Stack.
var coroParkedShards [coroParkedShardCount]struct {
	lock  mutex
	tasks []*coroTask
	_     cpu.CacheLinePad
}

// coroPanicTraces holds the logical coroutine call chain of the most recent
// unhandled coroutine panic per host goroutine ID (lock-free fixed table so
// fatalpanic can read it safely on g0 without acquiring any mutex).
type coroPanicTraceSlot struct {
	goid atomic.Uint64
	n    atomic.Uint32
	pcs  [coroMaxTraceFrames]uintptr
}

var coroPanicTraces [coroPanicTraceSlots]coroPanicTraceSlot
var coroPanicTraceSeq atomic.Uint32

func init() {
	for i := range coroParkedShards {
		lockInit(&coroParkedShards[i].lock, lockRankCoroSched)
	}
}

func coroParkedShardForCurrentP() uint32 {
	mp := acquirem()
	var id uint32
	if pp := mp.p.ptr(); pp != nil {
		id = uint32(pp.id) & (coroParkedShardCount - 1)
	}
	releasem(mp)
	return id
}

// coroTrackParked registers an unhosted task the first time it enters handleWait.
// Tasks remain registered across subsequent park/wake cycles and across per-P
// frame pool reuse (zero hot-path locking in steady state), and are removed either
// when dropped unpooled in coroRecycle or compacted under STW GC in coroClearParkedEmpty.
// Caller must exclusively own task.
func coroTrackParked(task *coroTask) {
	shard := coroParkedShardForCurrentP()
	sh := &coroParkedShards[shard]
	for {
		lock(&sh.lock)
		n := len(sh.tasks)
		if n < cap(sh.tasks) {
			if n >= 1<<24 {
				unlock(&sh.lock)
				return
			}
			sh.tasks = sh.tasks[:n+1]
			sh.tasks[n] = task
			task.result.Store((shard << 24) | uint32(n))
			task.flags |= coroFlagTracked
			unlock(&sh.lock)
			return
		}
		newCap := cap(sh.tasks) * 2
		if newCap < 32 {
			newCap = 32
		}
		unlock(&sh.lock)

		// Allocate replacement backing array outside sh.lock (never mallocgc under a runtime mutex).
		grown := make([]*coroTask, 0, newCap)
		lock(&sh.lock)
		if cap(sh.tasks) < newCap {
			grown = grown[:len(sh.tasks)]
			copy(grown, sh.tasks)
			sh.tasks = grown
		}
		n = len(sh.tasks)
		if n < cap(sh.tasks) {
			if n >= 1<<24 {
				unlock(&sh.lock)
				return
			}
			sh.tasks = sh.tasks[:n+1]
			sh.tasks[n] = task
			task.result.Store((shard << 24) | uint32(n))
			task.flags |= coroFlagTracked
			unlock(&sh.lock)
			return
		}
		unlock(&sh.lock)
	}
}

// coroUntrackParked removes a previously-tracked task via O(1) swap-remove.
// Caller must exclusively own task (called on unpooled coroRecycle paths).
func coroUntrackParked(task *coroTask) {
	shard := task.result.Load() >> 24
	if shard >= coroParkedShardCount {
		task.flags &^= coroFlagTracked
		task.result.Store(0)
		return
	}
	sh := &coroParkedShards[shard]
	lock(&sh.lock)
	idx := int(task.result.Load() & 0xffffff)
	n := len(sh.tasks)
	if idx < n && sh.tasks[idx] == task {
		last := sh.tasks[n-1]
		sh.tasks[idx] = last
		if last != task {
			last.result.Store((shard << 24) | uint32(idx))
		}
		sh.tasks[n-1] = nil
		sh.tasks = sh.tasks[:n-1]
	}
	task.result.Store(0)
	task.flags &^= coroFlagTracked
	unlock(&sh.lock)
}

// coroClearParkedEmpty compacts parked-tracking shards under STW GC, dropping any
// pooled/idle (coroStateNew) or completed (coroStateDone) tasks and releasing
// empty backing slices so idle programs retain 0 B after bursty workloads drain.
func coroClearParkedEmpty() {
	assertWorldStopped()
	for i := range coroParkedShards {
		sh := &coroParkedShards[i]
		tasks := sh.tasks
		if len(tasks) == 0 {
			if cap(tasks) > 0 {
				sh.tasks = nil
			}
			continue
		}
		w := 0
		shardHi := uint32(i) << 24
		for r := 0; r < len(tasks); r++ {
			t := tasks[r]
			if t == nil {
				continue
			}
			s := t.state.Load()
			if s == coroStateNew || s == coroStateDone {
				t.flags &^= coroFlagTracked
				t.result.Store(0)
				continue
			}
			if w != r {
				tasks[w] = t
				t.result.Store(shardHi | uint32(w))
			}
			w++
		}
		for k := w; k < len(tasks); k++ {
			tasks[k] = nil
		}
		if w == 0 {
			sh.tasks = nil
		} else {
			sh.tasks = tasks[:w]
		}
	}
}

func coroResumePC(fn func(unsafe.Pointer) uint8) uintptr {
	if fn == nil {
		return 0
	}
	fv := *(**funcval)(unsafe.Pointer(&fn))
	if fv == nil {
		return 0
	}
	return fv.fn
}

// coroCollectChainPCs appends the logical coroutine call stack [leaf -> parent -> root]
// into dst and returns the number of frames written.
func coroCollectChainPCs(task *coroTask, dst []uintptr) int {
	n := 0
	for cur := task; cur != nil && n < len(dst); cur = cur.parent {
		if pc := coroResumePC(cur.resume); pc != 0 {
			dst[n] = pc
			n++
		}
	}
	return n
}

func coroRecordPanicChain(task *coroTask, pb *coroPanicState) {
	if pb == nil || len(pb.tracePCs) > 0 {
		return
	}
	var buf [coroMaxTraceFrames]uintptr
	n := coroCollectChainPCs(task, buf[:])
	if n == 0 {
		return
	}
	pcs := make([]uintptr, n)
	copy(pcs, buf[:n])
	pb.tracePCs = pcs
}

func coroClearPanicTrace(goid uint64) {
	for i := range coroPanicTraces {
		if coroPanicTraces[i].goid.Load() == goid {
			coroPanicTraces[i].n.Store(0)
			coroPanicTraces[i].goid.Store(0)
		}
	}
}

func coroPublishPanicTrace(goid uint64, pcs []uintptr) {
	if len(pcs) == 0 {
		return
	}
	coroClearPanicTrace(goid)
	slot := &coroPanicTraces[coroPanicTraceSeq.Add(1)&(coroPanicTraceSlots-1)]
	slot.goid.Store(0)
	n := len(pcs)
	if n > coroMaxTraceFrames {
		n = coroMaxTraceFrames
	}
	for i := 0; i < n; i++ {
		slot.pcs[i] = pcs[i]
	}
	slot.n.Store(uint32(n))
	slot.goid.Store(goid)
}

func coroPrintPCFrames(pcs []uintptr) {
	for _, pc := range pcs {
		f := findfunc(pc)
		if !f.valid() {
			print("unknown(", hex(pc), ")\n")
			continue
		}
		print(funcname(f), "(...)\n")
		file, line := funcline(f, pc)
		print("\t", file, ":", line, "\n")
	}
}

// coroPrintPanicTrace prints the logical coroutine call chain during fatalpanic
// when gp is crashing from an unhandled coroutine panic.
func coroPrintPanicTrace(gp *g) {
	if gp == nil {
		return
	}
	goid := gp.goid
	for i := range coroPanicTraces {
		slot := &coroPanicTraces[i]
		if slot.goid.Load() != goid {
			continue
		}
		n := int(slot.n.Load())
		if n <= 0 || n > coroMaxTraceFrames {
			continue
		}
		print("coroutine logical stack:\n")
		coroPrintPCFrames(slot.pcs[:n])
		print("\n")
		return
	}
}

// coroForEachInactiveTaskSTW visits every parked or runnable unhosted stackless
// coroutine without allocating or taking locks. Caller must hold STW or be in a
// fatal crash dump.
func coroForEachInactiveTaskSTW(fn func(task *coroTask, status string) bool) {
	if coroUnhostedCount() == 0 {
		return
	}
	// 1. Parked unhosted coroutines.
	for i := range coroParkedShards {
		tasks := coroParkedShards[i].tasks
		for _, t := range tasks {
			if t == nil || t.flags&(coroFlagUnhosted|coroFlagUnhostedChild) == 0 {
				continue
			}
			if t.state.Load() != coroStateParked {
				continue
			}
			if !fn(t, "coro parked") {
				return
			}
		}
	}
	// 2. Runnable unhosted coroutines on per-P queues.
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		ps := pp.coro.Load()
		if ps == nil {
			continue
		}
		if rn := ps.runnext.Load(); rn != nil && rn.flags&(coroFlagUnhosted|coroFlagUnhostedChild) != 0 && rn.state.Load() == coroStateRunnable {
			if !fn(rn, "coro runnable") {
				return
			}
		}
		h := atomic.LoadAcq(&ps.runqH)
		tail := atomic.LoadAcq(&ps.runqT)
		if tail > h && tail-h <= coroPRunqSize {
			for i := h; i != tail; i++ {
				t := ps.runq[i&(coroPRunqSize-1)]
				if t != nil && t.flags&(coroFlagUnhosted|coroFlagUnhostedChild) != 0 && t.state.Load() == coroStateRunnable {
					if !fn(t, "coro runnable") {
						return
					}
				}
			}
		}
	}
	// 3. Runnable unhosted coroutines on the global overflow queue.
	for t := coroSched.runqHead; t != nil; t = t.next {
		if t.flags&(coroFlagUnhosted|coroFlagUnhostedChild) != 0 && t.state.Load() == coroStateRunnable {
			if !fn(t, "coro runnable") {
				return
			}
		}
	}
}

func coroCountProfileTasksSTW() int {
	if coroUnhostedCount() == 0 {
		return 0
	}
	count := 0
	coroForEachInactiveTaskSTW(func(*coroTask, string) bool {
		count++
		return true
	})
	return count
}

// coroRecordProfileSTW records up to len(dst) inactive unhosted coroutines into
// dst using pre-allocated pcBuf (zero heap allocations under STW).
func coroRecordProfileSTW(dst []profilerecord.StackRecord, labels []unsafe.Pointer, pcBuf []uintptr) int {
	if len(dst) == 0 || len(pcBuf) == 0 || coroUnhostedCount() == 0 {
		return 0
	}
	written := 0
	bufOff := 0
	coroForEachInactiveTaskSTW(func(task *coroTask, _ string) bool {
		if written >= len(dst) || bufOff >= len(pcBuf) {
			return false
		}
		avail := len(pcBuf) - bufOff
		if avail > coroMaxTraceFrames {
			avail = coroMaxTraceFrames
		}
		n := coroCollectChainPCs(task, pcBuf[bufOff:bufOff+avail])
		if n == 0 {
			return true
		}
		dst[written].Stack = pcBuf[bufOff : bufOff+n : bufOff+n]
		if labels != nil && written < len(labels) {
			labels[written] = nil
		}
		bufOff += n
		written++
		return true
	})
	return written
}

// coroTracebackAll appends stackless coroutine logical stacks to runtime.Stack(buf, true)
// and crash/SIGQUIT dumps when unhosted coroutines exist.
func coroTracebackAll() {
	if coroUnhostedCount() == 0 {
		return
	}
	var buf [coroMaxTraceFrames]uintptr
	printed := 0
	const maxDump = 64
	total := 0
	coroForEachInactiveTaskSTW(func(task *coroTask, status string) bool {
		total++
		if printed >= maxDump {
			return true
		}
		n := coroCollectChainPCs(task, buf[:])
		if n == 0 {
			return true
		}
		print("\ncoroutine ", hex(uintptr(unsafe.Pointer(task))), " [", status, "]:\n")
		coroPrintPCFrames(buf[:n])
		printed++
		return true
	})
	if total > printed {
		print("\n... ", total-printed, " more stackless coroutines omitted ...\n")
	}
}
