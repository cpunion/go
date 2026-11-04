// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"internal/abi"
	"testing"
)

func TestSourceFrameCopyIdentity(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	pkg := types.NewPkg("example.com/sourceframes", "p")
	types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
	source := newLowerTestFunc(pkg, "source.coro.literal")
	copy := newLowerTestFunc(pkg, ".private.body")
	child := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), copy, typecheck.Target, 0)
	copy.Body = ir.Nodes{child.OClosure}
	RecordSourceCopy(copy, source)
	if got := SourceFrameInfo(copy); got == nil || got.Source != source.Linksym() || got.Flags != abi.CoroSourceCopy {
		t.Fatalf("copy lost its explicit source identity: %v", got)
	}
	want := source.Linksym().Name + child.Sym().Name[len(copy.Sym().Name):]
	if got := SourceFrameInfo(child); got == nil || got.Source.Name != want {
		t.Fatalf("nested copy source = %v, want %s", got, want)
	}
	if got := SourceFrameInfo(child); got.Name == nil || string(got.Name.P) != want || got.NameSize != uint32(len(want)) {
		t.Fatal("nested source identity depends on the original closure having emitted code")
	}
	unrelated := newLowerTestFunc(pkg, "unrelated.coro.func1")
	if SourceFrameInfo(unrelated) != nil {
		t.Fatal("source attribution inferred identity from a helper-like name")
	}
	recordSourceResume(unrelated, copy, nil, nil)
	if got := SourceFrameInfo(unrelated); got.Source != source.Linksym() || got.Flags != abi.CoroSourceResume {
		t.Fatal("resume lost its canonical source identity")
	}
	recordSourceBridge(child)
	if got := SourceFrameInfo(child); got.Source != nil || got.Flags != abi.CoroSourceBridge {
		t.Fatal("ordinary bridge acquired an independent source activation")
	}
}
