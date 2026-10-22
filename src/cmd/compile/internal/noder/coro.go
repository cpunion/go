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
	pri.funcBody(body)
	readBodies(typecheck.Target, false, nil)
	return body
}
