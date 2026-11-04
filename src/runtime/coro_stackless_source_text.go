// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "internal/abi"

// This state belongs only to text projection. The physical unwinder used by
// GC and panic recovery remains unchanged. Like unwinder, this is pointer-free
// so copying a print/count cursor never requires a write barrier.
type stacklessCoroTracebackState struct {
	rootPC        uintptr
	bridges       bool
	afterDriver   bool
	terminal      bool
	panicStarted  bool
	panicBoundary bool
}

func stacklessCoroTracebackText(u *unwinder, state *stacklessCoroTracebackState, showRuntime, first bool, commit func() (bool, bool)) (handled, stop bool) {
	f := u.frame.fn
	info := stacklessCoroSourceFrame(f)
	source := stacklessCoroSourceFunc(f, info)
	if state.bridges {
		if state.rootPC != 0 && source.valid() && source.entry() == state.rootPC {
			state.bridges = false
			state.terminal = false
			return true, false
		}
		switch funcname(f) {
		case "runtime.coroRunFrame":
			if state.terminal {
				return true, false
			}
			state.bridges, state.afterDriver = false, true
			return true, false
		case "runtime.goexit":
			state.bridges = false
		default:
			return true, false
		}
	}
	if state.afterDriver {
		if funcname(f) == "runtime.coroRun" {
			return true, false
		}
		state.afterDriver = false
		if source.valid() && source.entry() == state.rootPC {
			return true, false
		}
	}
	if f.funcID == abi.FuncID_gopanic || funcname(f) == "runtime.Goexit" {
		for p := u.g.ptr()._panic; p != nil; p = p.link {
			if p.coroTrace != nil && uintptr(p.startSP) == u.frame.sp {
				if !state.panicStarted {
					state.panicStarted = true
					state.panicBoundary = !first || p.goexit
				}
				if state.panicBoundary {
					boundaryFirst := false
					if !printStacklessCoroSourceFrame(u, f, u.symPC(), showRuntime, &boundaryFirst, commit) {
						return true, true
					}
				}
				if !printStacklessCoroPanicTrace(u, p.coroTrace, commit) {
					return true, true
				}
				state.rootPC, state.bridges = p.coroTrace.rootPC, true
				state.terminal = true
				state.panicStarted = false
				return true, false
			}
		}
	}
	if funcname(f) == "runtime.(*stacklessCoroHost).drive" {
		if task := stacklessCoroParkedSourceTask(u.g.ptr()); task != nil {
			rootPC, complete := walkStacklessCoroSourceFrames(task, func(f funcInfo, pc uintptr) bool {
				return printStacklessCoroSourceFrame(u, f, pc, showRuntime, &first, commit)
			})
			if !complete {
				// Count/print clones replay this expansion at the same driver.
				return true, true
			}
			state.rootPC, state.bridges = rootPC, true
			return true, false
		}
	}
	if info == nil {
		return false, false
	}
	if info.flags&abi.CoroSourceBridge != 0 {
		host := stacklessCoroHostState(u.g.ptr())
		if host != nil && host.ordinaryDepth != 0 && host.activeTask != nil {
			rootPC, complete := walkStacklessCoroSourceFrames(host.activeTask, func(f funcInfo, pc uintptr) bool {
				return printStacklessCoroSourceFrame(u, f, pc, showRuntime, &first, commit)
			})
			if !complete {
				// Keep u on the bridge. As with an inlined physical frame,
				// the count/print clones replay this expansion with lastN
				// skipped. Only a complete expansion starts bridge elision.
				return true, true
			}
			state.rootPC, state.bridges = rootPC, true
			return true, false
		}
	}
	if info.flags&abi.CoroSourceCopy != 0 && source.valid() {
		file, line := funcline1(f, u.symPC(), false)
		pc := stacklessCoroSourcePC(source, file, line)
		return true, !printStacklessCoroSourceFrame(u, source, pc, showRuntime, &first, commit)
	}
	return false, false
}

func printStacklessCoroSourceFrame(u *unwinder, f funcInfo, pc uintptr, showRuntime bool, first *bool, commit func() (bool, bool)) bool {
	for iu, uf := newInlineUnwinder(f, pc); uf.valid(); uf = iu.next(uf) {
		sf := iu.srcFunc(uf)
		callee := u.calleeFuncID
		u.calleeFuncID = sf.funcID
		if !(showRuntime || showframe(sf, u.g.ptr(), *first, callee)) {
			continue
		}
		pr, stop := commit()
		if stop {
			return false
		}
		*first = false
		if !pr {
			continue
		}
		file, line := iu.fileLine(uf)
		name := sf.name()
		if !iu.isInlined(uf) {
			if source := stacklessCoroSourceName(f); source != "" {
				name = source
			}
		}
		printFuncName(name)
		// These activations live in typed heap frames, not ordinary ABI
		// argument slots. As for inlined frames, omit arguments and physical
		// stack addresses rather than reading unrelated bridge stack slots.
		print("(...)\n\t", file, ":", line, "\n")
	}
	return true
}
