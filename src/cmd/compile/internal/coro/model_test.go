// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"strings"
	"testing"
)

func testFunc(name string) *ir.Func {
	pkg := types.NewPkg("example.com/coro/modeltest", "modeltest")
	return ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name),
		types.NewSignature(nil, nil, nil))
}

func TestFuncSummaryPrimary(t *testing.T) {
	tests := []struct {
		summary FuncSummary
		want    PrimaryKind
	}{
		{FuncSummary{}, PlainPrimary},
		{FuncSummary{Effect: MaySuspend}, CoroPrimary},
		{FuncSummary{Exec: NeedsPreempt}, CoroPrimary},
		{FuncSummary{Exec: NeedsSystemABI}, PlainPrimary},
	}
	for _, test := range tests {
		if got := test.summary.Primary(); got != test.want {
			t.Errorf("FuncSummary%+v.Primary() = %v, want %v",
				test.summary, got, test.want)
		}
	}
}

func TestExecFlagsString(t *testing.T) {
	if got, want := ExecFlags(0).String(), "-"; got != want {
		t.Errorf("ExecFlags(0).String() = %q, want %q", got, want)
	}
	flags := NeedsPreempt | NeedsSystemABI | ExecFlags(1<<20)
	if got, want := flags.String(), "preempt,system-abi,ExecFlags(0x100000)"; got != want {
		t.Errorf("ExecFlags.String() = %q, want %q", got, want)
	}
}

func TestVerifyPlan(t *testing.T) {
	fn := testFunc("valid")
	node := ir.NewReturnStmt(src.NoXPos, nil)
	function := &Function{
		Func:    fn,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{{
			ID:   1,
			Kind: SiteYield,
			Node: node,
		}},
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
	if err := plan.Verify(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	function.Primary = PlainPrimary
	if err := plan.Verify(); err == nil ||
		!strings.Contains(err.Error(), "does not match effect") {
		t.Fatalf("mismatched primary error = %v", err)
	}
	function.Primary = CoroPrimary

	function.Sites = append(function.Sites, function.Sites[0])
	if err := plan.Verify(); err == nil ||
		!strings.Contains(err.Error(), "duplicate site ID") {
		t.Fatalf("duplicate site error = %v", err)
	}
}

func TestVerifyForeignSite(t *testing.T) {
	fn := testFunc("foreign")
	node := ir.NewReturnStmt(src.NoXPos, nil)
	function := &Function{
		Func:    fn,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{{
			ID:      1,
			Kind:    SiteForeign,
			Node:    node,
			Foreign: AsyncOperation,
		}},
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
	if err := plan.Verify(); err == nil ||
		!strings.Contains(err.Error(), "lacks System ABI") {
		t.Fatalf("foreign site error = %v", err)
	}

	function.Exec = NeedsSystemABI
	if err := plan.Verify(); err != nil {
		t.Fatalf("valid foreign plan rejected: %v", err)
	}
}
