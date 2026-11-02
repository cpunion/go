// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
	"cmd/internal/sys"
	"testing"
)

func TestReadCoroBodyUnavailable(t *testing.T) {
	pkg := types.NewPkg("example.com/coro/unavailable", "unavailable")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("synthetic"),
		types.NewSignature(nil, nil, nil))
	if HaveCoroBody(fn) {
		t.Fatal("synthetic function has a serialized source body")
	}
	if body := ReadCoroBody(fn); body != nil {
		t.Fatal("synthetic function acquired an ordinary source body")
	}
	if PinCoroCaptures(fn) {
		t.Fatal("synthetic function acquired a serialized capture-layout capability")
	}
}

// A prior ReadCoroBodyAs result is an IR container, not an OClosure. Its
// captures must still agree with a later selected-value helper's source layout.
func TestPinCoroCaptureCopies(t *testing.T) {
	if types.Types[types.TINT] == nil {
		types.PtrSize, types.RegSize, types.MaxWidth = 8, 8, 1<<50
		base.Ctxt = &obj.Link{Arch: &obj.LinkArch{Arch: &sys.Arch{Alignment: 1}}}
		typecheck.InitUniverse()
	}
	savedBodies, savedPins, savedCopies, savedFixed := bodyReader, coroPinnedCaptures, coroCaptureCopies, coroPinnedCaptureCopies
	bodyReader = make(map[*ir.Func]pkgReaderIndex)
	coroPinnedCaptures = make(map[coroCaptureBody]bool)
	coroCaptureCopies = make(map[coroCaptureBody]map[*ir.Func]bool)
	coroPinnedCaptureCopies = make(map[*ir.Func]bool)
	defer func() {
		bodyReader, coroPinnedCaptures, coroCaptureCopies, coroPinnedCaptureCopies = savedBodies, savedPins, savedCopies, savedFixed
	}()
	pkg := types.NewPkg("example.com/coro/capturecopies", "copies")
	newCopy := func(name string, closure bool) *ir.Func {
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, nil, nil))
		outer := ir.NewNameAt(src.NoXPos, pkg.Lookup(name+".cell"), types.Types[types.TINT])
		outer.Class = ir.PAUTO
		outer.SetTypecheck(1)
		capture := ir.NewClosureVar(src.NoXPos, fn, outer)
		capture.SetTypecheck(1)
		if closure {
			fn.OClosure = &ir.ClosureExpr{Func: fn}
		}
		return fn
	}
	pri := pkgReaderIndex{pr: new(pkgReader), idx: 7, dict: new(readerDict), methodSym: pkg.Lookup("method")}
	source := newCopy("source", true)
	bodyReader[source] = pri
	prior := newCopy("prior", false)
	rememberCoroCaptureCopy(prior, pri)
	if prior.ClosureVars[0].Canonical().Addrtaken() || len(prior.Body) != 0 {
		t.Fatal("recording a copy changed capture semantics before pinning")
	}
	var unrelated []*ir.Func
	for _, field := range []string{"reader", "index", "dictionary", "method", "synthetic"} {
		other := pri
		switch field {
		case "reader":
			other.pr = new(pkgReader)
		case "index":
			other.idx++
		case "dictionary":
			other.dict = new(readerDict)
		case "method":
			other.methodSym = pkg.Lookup("otherMethod")
		case "synthetic":
			other.synthetic = func(src.XPos, *reader) {}
		}
		copy := newCopy("other."+field, false)
		rememberCoroCaptureCopy(copy, other)
		unrelated = append(unrelated, copy)
	}
	if !PinCoroCaptures(source) {
		t.Fatal("serialized captured declaration lost layout capability")
	}
	future := newCopy("future", false)
	rememberCoroCaptureCopy(future, pri)
	for _, copy := range []*ir.Func{source, prior, future} {
		if !copy.ClosureVars[0].Canonical().Addrtaken() || len(copy.Body) != 1 {
			t.Fatalf("%s: independent capture cell was not pinned", copy.Sym().Name)
		}
	}
	for _, copy := range unrelated {
		if copy.ClosureVars[0].Canonical().Addrtaken() || len(copy.Body) != 0 {
			t.Fatalf("%s: unrelated serialized body acquired the source layout", copy.Sym().Name)
		}
	}
	if PinCoroCaptures(prior) || !PinCoroCaptures(source) || len(prior.Body) != 1 {
		t.Fatal("body container acquired source authority or repeat pinning duplicated address-taking")
	}
}
