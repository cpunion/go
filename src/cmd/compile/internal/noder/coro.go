// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// ReadCoroBody reads an independent copy of fn's source body for execution
// inside a synctest bubble. The returned function is only an IR container;
// coroutine lowering installs its body in fn without adding a call frame.
// Synthetic functions without serialized bodies must remain ordinary Go.
func ReadCoroBody(fn *ir.Func) *ir.Func {
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
	body := ir.NewFunc(fn.Pos(), fn.Nname.Pos(), fn.Sym().Pkg.Lookup(fn.Sym().Name+".coro.go"), typ)
	body.SetDupok(fn.Dupok())
	body.Nname.Defn = body
	pri.funcBody(body)
	readBodies(typecheck.Target, false, nil)
	return body
}
