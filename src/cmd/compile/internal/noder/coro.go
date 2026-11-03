// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"fmt"
)

var coroBodyGen int

// Use serialized body identity, including its instantiation dictionary. A
// reread has fresh closure symbols and declarations, so source spelling or
// positions cannot identify its environment layout.
type coroCaptureBody struct {
	pr        *pkgReader
	idx       index
	dict      *readerDict
	methodSym *types.Sym
}

var coroPinnedCaptures = make(map[coroCaptureBody]bool)
var coroPinnedCaptureCopies = make(map[*ir.Func]bool)

// Independent body containers have no OClosure and are not source declarations
// in bodyReader. Retain their serialized identity before any later source
// specialization requests a fixed environment layout.
var coroCaptureCopies = make(map[coroCaptureBody]map[*ir.Func]bool)

func coroCaptureKey(pri pkgReaderIndex) coroCaptureBody {
	return coroCaptureBody{pri.pr, pri.idx, pri.dict, pri.methodSym}
}

// PinCoroCaptures fixes the selected source closure's environment layout for
// private go/defer helpers. Every later ordinary body read receives the same
// address-taking before escape analysis. Synthetic operand wrappers cannot
// acquire this capability.
func PinCoroCaptures(fn *ir.Func) bool {
	pri, ok := bodyReader[fn]
	if !ok || pri.synthetic != nil || pri.pr == nil || fn.OClosure == nil || len(fn.ClosureVars) == 0 {
		return false
	}
	key := coroCaptureKey(pri)
	coroPinnedCaptures[key] = true
	// Register source declarations as well as previously materialized body
	// containers; symbol spelling and source positions are not identities.
	for candidate, body := range bodyReader {
		if body.synthetic == nil && coroCaptureKey(body) == key && candidate.OClosure != nil {
			rememberCoroCaptureCopy(candidate, body)
		}
	}
	for copy := range coroCaptureCopies[key] {
		pinCoroCaptureCells(copy)
	}
	return true
}

func rememberCoroCaptureCopy(fn *ir.Func, pri pkgReaderIndex) {
	if pri.synthetic != nil || pri.pr == nil || len(fn.ClosureVars) == 0 {
		return
	}
	key := coroCaptureKey(pri)
	if coroCaptureCopies[key] == nil {
		coroCaptureCopies[key] = make(map[*ir.Func]bool)
	}
	coroCaptureCopies[key][fn] = true
	if coroPinnedCaptures[key] {
		pinCoroCaptureCells(fn)
	}
}

func pinCoroCaptureCells(fn *ir.Func) {
	if coroPinnedCaptureCopies[fn] || len(fn.ClosureVars) == 0 {
		return
	}
	coroPinnedCaptureCopies[fn] = true
	var prefix ir.Nodes
	ir.WithFunc(fn, func() {
		for _, variable := range fn.ClosureVars {
			variable.Canonical().SetAddrtaken(true)
			prefix = append(prefix, typecheck.Stmt(ir.NewAssignStmt(variable.Pos(), ir.BlankNode, typecheck.NodAddr(variable))))
		}
	})
	fn.Body = append(prefix, fn.Body...)
}

func pinCoroCaptureCopies(body *ir.Func, source pkgReaderIndex) {
	seen := make(map[*ir.Func]bool)
	var visit func(*ir.Func, pkgReaderIndex)
	visit = func(fn *ir.Func, pri pkgReaderIndex) {
		if seen[fn] {
			return
		}
		seen[fn] = true
		rememberCoroCaptureCopy(fn, pri)
		ir.Visit(fn, func(node ir.Node) {
			if closure, ok := node.(*ir.ClosureExpr); ok {
				if child, found := bodyReader[closure.Func]; found {
					visit(closure.Func, child)
				}
			}
		})
	}
	visit(body, source)
}

// HaveCoroBody reports whether fn has a serialized source body without
// constructing any local declarations or closures.
func HaveCoroBody(fn *ir.Func) bool {
	_, ok := bodyReader[fn]
	return ok
}

// ReadCoroBody reads an independent copy of fn's source body for ordinary
// fallback. The returned function is only an IR container; coroutine lowering
// installs its body in fn without adding a call frame. Each read owns distinct
// closure symbols, including repeated reads of one source function.
// Synthetic functions without serialized bodies must remain ordinary Go.
func ReadCoroBody(fn *ir.Func) *ir.Func {
	coroBodyGen++
	return ReadCoroBodyAs(fn, fn.Sym().Pkg.Lookup(fmt.Sprintf("%s.coro.go%d", fn.Sym().Name, coroBodyGen)))
}

// ReadCoroBodyAs materializes a source body under an independently owned
// symbol. Its closures are named beneath that symbol before they are read.
func ReadCoroBodyAs(fn *ir.Func, sym *types.Sym) *ir.Func {
	pri, ok := bodyReader[fn]
	if !ok {
		return nil
	}
	copyFields := func(fields []*types.Field) []*types.Field {
		copies := make([]*types.Field, len(fields))
		for i, field := range fields {
			copies[i] = field.Copy()
			copies[i].Nname = nil
		}
		return copies
	}
	sig := fn.Type()
	var recv *types.Field
	if sig.Recv() != nil {
		recv = sig.Recv().Copy()
		recv.Nname = nil
	}
	typ := types.NewSignature(recv, copyFields(sig.Params()), copyFields(sig.Results()))
	body := ir.NewFunc(fn.Pos(), fn.Nname.Pos(), sym, typ)
	body.SetDupok(fn.Dupok())
	body.Pragma = fn.Pragma
	body.Nname.Defn = body
	for _, variable := range fn.ClosureVars {
		ir.NewClosureVar(variable.Pos(), body, variable.Outer)
	}
	pri.funcBody(body)
	readBodies(typecheck.Target, false, nil)
	pinCoroCaptureCopies(body, pri)
	return body
}
