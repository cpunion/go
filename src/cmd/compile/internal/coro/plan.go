// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package coro implements stackless coroutine lowering for GOEXPERIMENT=coro.
//
// The pass is structured in four stages:
//  1. Plan (plan.go): package eligibility, call-graph analysis, and suspension
//     classification.
//  2. CFG (cfg.go): statement/expression normalization into a flat basic-block
//     control-flow graph with explicit suspension terminators.
//  3. Frame (frame.go): backward liveness analysis and combined
//     struct{ coroTask; frame } layout generation.
//  4. Lower (lower.go, callable.go): emitting F.coro.func1 state machines,
//     F.coro frame constructors, public F wrappers, and dynamic dispatch init.
package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"fmt"
	"internal/buildcfg"
	"os"
	"slices"
	"strings"
)

// ioKind identifies a compiler-lowered direct stdlib I/O operation.
type ioKind uint8

const (
	ioNone ioKind = iota
	ioFileRead
	ioFileWrite
	ioConnRead
	ioConnWrite
)

// Plan summarizes coroutine analysis results for a package.
type Plan struct {
	Pkg        *ir.Package
	Funcs      []*ir.Func
	Lowered    map[*ir.Func]*FuncInfo
	SkipReason map[*ir.Func]string
	HasSuspend map[*ir.Func]bool
}

// FuncInfo holds lowering metadata for one lowered function.
type FuncInfo struct {
	Orig       *ir.Func
	FrameType  *types.Type
	FactoryFn  *ir.Func // F.coro
	ResumeFn   *ir.Func // F.coro.func1
	IfaceAdpFn *ir.Func // method adapter for coroLookupInterface
	FuncAdpFn  *ir.Func // func adapter for coroLookupFunction

	// Capture metadata for closures (indexed matching Orig.ClosureVars).
	CapByRef []bool

	// Frame field indices (populated by frame.go).
	StateField  *types.Field
	ParamFields []*types.Field // matches receiver (if any) + regular params
	CapFields   []*types.Field // matches Orig.ClosureVars
	ResFields   []*types.Field // matches results
	VarFields   map[*ir.Name]*types.Field
}

// shouldCompilePackage reports whether the current package is eligible for
// coroutine analysis/lowering. Low-level runtime/stdlib packages are skipped
// so even -gcflags=all=-d=coro=4 remains safe and non-recursive.
func shouldCompilePackage() bool {
	if !buildcfg.Experiment.Coro {
		return false
	}
	if base.Flag.CompilingRuntime {
		return false
	}
	p := types.LocalPkg.Path
	switch p {
	case "", "runtime", "syscall", "sync", "sync/atomic", "reflect", "os", "net", "time", "testing", "unsafe":
		return false
	}
	if strings.HasPrefix(p, "runtime/") || strings.HasPrefix(p, "internal/") {
		return false
	}
	return true
}

// AnalyzePreserve runs provisional analysis before inlining and returns a
// PreserveCall predicate that prevents inlining away coroutine call boundaries.
func AnalyzePreserve(pkg *ir.Package) func(*ir.Func) bool {
	if !shouldCompilePackage() {
		return nil
	}
	plan := analyze(pkg)
	if len(plan.Lowered) == 0 {
		return nil
	}
	return func(fn *ir.Func) bool {
		if fn == nil {
			return false
		}
		if _, ok := plan.Lowered[fn]; ok {
			return true
		}
		return isSuspendingStdlibFunc(fn)
	}
}

func isSuspendingStdlibFunc(fn *ir.Func) bool {
	if fn == nil || fn.Sym() == nil || fn.Sym().Pkg == nil {
		return false
	}
	pkg := fn.Sym().Pkg.Path
	name := fn.Sym().Name
	switch pkg {
	case "runtime":
		return name == "Gosched" || name == "GC" || name == "Goexit"
	case "time":
		return name == "Sleep"
	case "os":
		return name == "(*File).Read" || name == "(*File).Write"
	case "net":
		return strings.HasSuffix(name, ").Read") || strings.HasSuffix(name, ").Write")
	}
	return false
}

// Compile runs final analysis and (when -d=coro>=4) lowers all eligible
// coroutine functions in pkg.
func Compile(pkg *ir.Package) {
	if !shouldCompilePackage() {
		return
	}
	plan := analyze(pkg)
	if base.Debug.Coro >= 1 {
		fmt.Fprintln(os.Stderr, "coro: phase=final")
		if base.Debug.Coro >= 2 {
			plan.dump(os.Stderr)
		}
	}
	if base.Debug.Coro < 4 {
		return
	}
	loweredCount, skippedCount, diags := plan.lowerAll()
	fmt.Fprintf(os.Stderr, "coro: phase=lower lowered=%d skipped=%d\n", loweredCount, skippedCount)
	for _, d := range diags {
		fmt.Fprintf(os.Stderr, "coro: skip %s\n", d)
	}
}

func analyze(pkg *ir.Package) *Plan {
	p := &Plan{
		Pkg:        pkg,
		Lowered:    make(map[*ir.Func]*FuncInfo),
		SkipReason: make(map[*ir.Func]string),
		HasSuspend: make(map[*ir.Func]bool),
	}

	inPkg := make(map[*ir.Func]bool, len(pkg.Funcs))
	for _, fn := range pkg.Funcs {
		if fn == nil {
			continue
		}
		p.Funcs = append(p.Funcs, fn)
		inPkg[fn] = true
	}

	syncCalls := make(map[*ir.Func][]*ir.Func)
	for _, fn := range p.Funcs {
		if reason := basicIneligibleReason(fn); reason != "" {
			p.SkipReason[fn] = reason
			continue
		}
		var calls []*ir.Func
		ir.VisitList(fn.Body, func(n ir.Node) {
			if p.SkipReason[fn] != "" {
				return
			}
			switch n.Op() {
			case ir.OSEND, ir.ORECV, ir.OAS2RECV, ir.OSELECT, ir.OGO:
				p.HasSuspend[fn] = true
			case ir.ORANGE:
				rn := n.(*ir.RangeStmt)
				if rn.X != nil && rn.X.Type() != nil && rn.X.Type().IsChan() {
					p.HasSuspend[fn] = true
				}
			case ir.OCALLFUNC, ir.OCALLMETH:
				call := n.(*ir.CallExpr)
				if call.GoDefer {
					return
				}
				if isSuspendingBuiltinCall(call) || matchStdlibIO(call) != ioNone {
					p.HasSuspend[fn] = true
					return
				}
				if reason := disqualifyingCallReason(call); reason != "" {
					p.SkipReason[fn] = reason
					return
				}
				if callee := staticLocalCallee(call, inPkg); callee != nil {
					calls = append(calls, callee)
				}
			case ir.OCALLINTER:
				call := n.(*ir.CallExpr)
				if reason := disqualifyingInterfaceCall(call); reason != "" {
					p.SkipReason[fn] = reason
				}
			}
		})
		if len(calls) > 0 {
			syncCalls[fn] = calls
		}
	}

	// Fixed-point 1: propagate real disqualifiers (cgo, testing harness, blocking
	// setup/sync, runtime host ops) through synchronous intra-package calls.
	for changed := true; changed; {
		changed = false
		for fn, callees := range syncCalls {
			if p.SkipReason[fn] != "" {
				continue
			}
			for _, callee := range callees {
				if r := p.SkipReason[callee]; isPropagatingDisqualifier(r) {
					p.SkipReason[fn] = "calls " + shortFuncName(callee) + " (" + r + ")"
					changed = true
					break
				}
			}
		}
	}

	// Fixed-point 2: propagate suspension effect through eligible synchronous calls.
	for changed := true; changed; {
		changed = false
		for fn, callees := range syncCalls {
			if p.SkipReason[fn] != "" || p.HasSuspend[fn] {
				continue
			}
			for _, callee := range callees {
				if p.SkipReason[callee] == "" && p.HasSuspend[callee] {
					p.HasSuspend[fn] = true
					changed = true
					break
				}
			}
		}
	}

	// Select eligible suspending functions for lowering.
	for _, fn := range p.Funcs {
		if p.SkipReason[fn] == "" && p.HasSuspend[fn] {
			p.Lowered[fn] = &FuncInfo{Orig: fn}
		}
	}

	// Determine by-ref vs by-val capture mode for lowered closures.
	for _, fn := range p.Funcs {
		info := p.Lowered[fn]
		if info == nil || len(fn.ClosureVars) == 0 {
			continue
		}
		info.CapByRef = computeCaptureByRef(fn)
	}

	return p
}

func basicIneligibleReason(fn *ir.Func) string {
	if len(fn.Body) == 0 {
		return "empty body"
	}
	if fn.Wrapper() || fn.ABIWrapper() {
		return "wrapper"
	}
	if fn.IsPackageInit() {
		return "package init"
	}
	if fn.Pragma&(ir.Nosplit|ir.Nowritebarrier|ir.Nowritebarrierrec|ir.Systemstack|ir.CgoUnsafeArgs) != 0 {
		return "runtime pragma"
	}
	if fn.Sym() != nil {
		name := fn.Sym().Name
		if strings.HasPrefix(name, "_Cfunc_") || strings.HasPrefix(name, "_cgo_") || strings.HasPrefix(name, "init.") {
			return "cgo/init helper"
		}
	}
	if fn.Type() != nil && fn.Type().HasShape() {
		return "generic shape"
	}
	return ""
}

func isPropagatingDisqualifier(reason string) bool {
	switch reason {
	case "", "empty body", "wrapper", "package init", "generic shape", "cgo/init helper":
		return false
	}
	return true
}

func isSuspendingBuiltinCall(call *ir.CallExpr) bool {
	pkgPath, name := callTargetPkgName(call)
	switch pkgPath {
	case "runtime":
		return name == "Gosched" || name == "GC"
	case "time":
		return name == "Sleep"
	}
	return false
}

func matchStdlibIO(call *ir.CallExpr) ioKind {
	if buildcfg.GOOS != "darwin" && buildcfg.GOOS != "linux" {
		return ioNone
	}
	sel, ok := call.Fun.(*ir.SelectorExpr)
	if !ok || sel.Sel == nil {
		return ioNone
	}
	mname := sel.Sel.Name
	if mname != "Read" && mname != "Write" {
		return ioNone
	}
	// Resolve the concrete receiver type of the method call.
	var recvTyp *types.Type
	if sel.Selection != nil && sel.Selection.Type != nil && sel.Selection.Type.Recv() != nil {
		recvTyp = sel.Selection.Type.Recv().Type
	} else if sel.X != nil {
		recvTyp = sel.X.Type()
	}
	if recvTyp == nil || !recvTyp.IsPtr() {
		return ioNone
	}
	elem := recvTyp.Elem()
	if elem == nil || elem.Sym() == nil || elem.Sym().Pkg == nil {
		return ioNone
	}
	pkgPath := elem.Sym().Pkg.Path
	typeName := elem.Sym().Name
	if pkgPath == "os" && typeName == "File" {
		if mname == "Read" {
			return ioFileRead
		}
		return ioFileWrite
	}
	if pkgPath == "net" && (typeName == "conn" || typeName == "TCPConn" || typeName == "UnixConn" || typeName == "UDPConn" || typeName == "IPConn") {
		if mname == "Read" {
			return ioConnRead
		}
		return ioConnWrite
	}
	return ioNone
}

func disqualifyingCallReason(call *ir.CallExpr) string {
	pkgPath, name := callTargetPkgName(call)
	if strings.HasPrefix(name, "_Cfunc_") || strings.HasPrefix(name, "_cgo_") {
		return "calls cgo"
	}
	switch pkgPath {
	case "runtime":
		switch name {
		case "cgocall", "asmcgocall":
			return "calls cgo"
		case "LockOSThread", "UnlockOSThread", "ReadMemStats", "SetFinalizer":
			return "calls runtime host op"
		}
	case "runtime/debug":
		if name == "FreeOSMemory" {
			return "calls runtime host op"
		}
	case "testing":
		return "calls testing harness"
	case "sync":
		switch {
		case strings.HasSuffix(name, ".Lock"),
			strings.HasSuffix(name, ".RLock"),
			strings.HasSuffix(name, ".Wait"),
			strings.HasSuffix(name, ".Do"):
			return "calls blocking sync"
		}
	case "net":
		if strings.HasPrefix(name, "Listen") || strings.HasPrefix(name, "Dial") ||
			strings.Contains(name, ".Accept") {
			return "calls blocking setup"
		}
	case "os":
		switch name {
		case "Open", "OpenFile", "Create", "Pipe":
			return "calls blocking setup"
		}
	}
	return ""
}

func disqualifyingInterfaceCall(call *ir.CallExpr) string {
	sel, ok := call.Fun.(*ir.SelectorExpr)
	if !ok || sel.X == nil || sel.X.Type() == nil {
		return ""
	}
	t := sel.X.Type()
	if t.Sym() != nil && t.Sym().Pkg != nil && t.Sym().Pkg.Path == "testing" {
		return "calls testing harness"
	}
	return ""
}

func callTargetPkgName(call *ir.CallExpr) (string, string) {
	switch fn := call.Fun.(type) {
	case *ir.Name:
		if fn.Sym() != nil {
			path := ""
			if fn.Sym().Pkg != nil {
				path = fn.Sym().Pkg.Path
			}
			return path, fn.Sym().Name
		}
	case *ir.SelectorExpr:
		if fn.Sel != nil {
			if fn.Op() == ir.ODOTMETH || fn.Op() == ir.OMETHEXPR {
				var recvTyp *types.Type
				if fn.Selection != nil && fn.Selection.Type != nil && fn.Selection.Type.Recv() != nil {
					recvTyp = fn.Selection.Type.Recv().Type
				} else if fn.X != nil {
					recvTyp = fn.X.Type()
				}
				if recvTyp != nil {
					baseT := recvTyp
					if baseT.IsPtr() {
						baseT = baseT.Elem()
					}
					if baseT.Sym() != nil && baseT.Sym().Pkg != nil {
						return baseT.Sym().Pkg.Path, baseT.Sym().Name + "." + fn.Sel.Name
					}
				}
			}
			if fn.Sel.Pkg != nil {
				return fn.Sel.Pkg.Path, fn.Sel.Name
			}
		}
	}
	return "", ""
}

func staticLocalCallee(call *ir.CallExpr, inPkg map[*ir.Func]bool) *ir.Func {
	if call == nil {
		return nil
	}
	switch fn := call.Fun.(type) {
	case *ir.Name:
		if fn.Func != nil && inPkg[fn.Func] {
			return fn.Func
		}
	case *ir.ClosureExpr:
		if fn.Func != nil && inPkg[fn.Func] {
			return fn.Func
		}
	case *ir.SelectorExpr:
		if fn.Op() == ir.ODOTMETH || fn.Op() == ir.OMETHEXPR {
			if mn := ir.MethodExprName(fn); mn != nil && mn.Func != nil && inPkg[mn.Func] {
				return mn.Func
			}
		}
	}
	return nil
}

func computeCaptureByRef(clo *ir.Func) []bool {
	res := make([]bool, len(clo.ClosureVars))
	writtenOrAddr := make(map[*ir.Name]bool)
	scanMutations := func(body ir.Nodes) {
		ir.VisitList(body, func(n ir.Node) {
			switch n.Op() {
			case ir.OADDR:
				if nm, ok := n.(*ir.AddrExpr).X.(*ir.Name); ok {
					writtenOrAddr[nm.Canonical()] = true
				}
			case ir.OAS:
				if nm, ok := n.(*ir.AssignStmt).X.(*ir.Name); ok {
					writtenOrAddr[nm.Canonical()] = true
				}
			case ir.OASOP:
				if nm, ok := n.(*ir.AssignOpStmt).X.(*ir.Name); ok {
					writtenOrAddr[nm.Canonical()] = true
				}
			case ir.OAS2, ir.OAS2DOTTYPE, ir.OAS2FUNC, ir.OAS2MAPR, ir.OAS2RECV:
				for _, lhs := range n.(*ir.AssignListStmt).Lhs {
					if nm, ok := lhs.(*ir.Name); ok {
						writtenOrAddr[nm.Canonical()] = true
					}
				}
			}
		})
	}
	scanMutations(clo.Body)
	// Also scan enclosing function for mutations/address-taking of captured outer vars.
	outerWritten := make(map[*ir.Name]bool)
	if clo.ClosureParent != nil {
		var check func(body ir.Nodes)
		check = func(body ir.Nodes) {
			ir.VisitList(body, func(n ir.Node) {
				switch n.Op() {
				case ir.OADDR:
					if nm, ok := n.(*ir.AddrExpr).X.(*ir.Name); ok {
						outerWritten[nm.Canonical()] = true
					}
				case ir.OAS:
					as := n.(*ir.AssignStmt)
					// First assignment (declaration init) is fine; track count > 1 or non-Def.
					if nm, ok := as.X.(*ir.Name); ok && !as.Def {
						outerWritten[nm.Canonical()] = true
					}
				case ir.OASOP:
					if nm, ok := n.(*ir.AssignOpStmt).X.(*ir.Name); ok {
						outerWritten[nm.Canonical()] = true
					}
				case ir.OAS2, ir.OAS2DOTTYPE, ir.OAS2FUNC, ir.OAS2MAPR, ir.OAS2RECV:
					al := n.(*ir.AssignListStmt)
					if !al.Def {
						for _, lhs := range al.Lhs {
							if nm, ok := lhs.(*ir.Name); ok {
								outerWritten[nm.Canonical()] = true
							}
						}
					}
				}
			})
		}
		check(clo.ClosureParent.Body)
	}
	for i, cv := range clo.ClosureVars {
		canon := cv.Canonical()
		outer := cv.Outer
		byRef := cv.Addrtaken() || canon.Addrtaken() ||
			writtenOrAddr[canon] ||
			(outer != nil && (outer.Addrtaken() || outerWritten[outer.Canonical()])) ||
			cv.Type().Size() > 128
		res[i] = byRef
		if byRef && outer != nil {
			outer.Canonical().SetAddrtaken(true)
		}
	}
	return res
}

func shortFuncName(fn *ir.Func) string {
	if fn == nil || fn.Sym() == nil {
		return "<nil>"
	}
	return fn.Sym().Name
}

func (p *Plan) dump(w *os.File) {
	fns := slices.Clone(p.Funcs)
	slices.SortFunc(fns, func(a, b *ir.Func) int {
		return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b))
	})
	for _, fn := range fns {
		eff := "nosuspend"
		if p.HasSuspend[fn] {
			eff = "may-suspend"
		}
		status := "plain"
		if p.Lowered[fn] != nil {
			status = "lower"
		} else if r := p.SkipReason[fn]; r != "" {
			status = "skip(" + r + ")"
		}
		fmt.Fprintf(w, "coro: func=%s effect=%s status=%s\n", ir.PkgFuncName(fn), eff, status)
	}
}
