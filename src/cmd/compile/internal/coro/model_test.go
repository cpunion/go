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
		{FuncSummary{Exec: NeedsSystemABI}, CoroPrimary},
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

func TestDecodeFuncSummary(t *testing.T) {
	read := func(values ...uint64) (func() uint64, *int) {
		index := 0
		return func() uint64 {
			value := values[index]
			index++
			return value
		}, &index
	}

	next, reads := read()
	if summary, ok, err := DecodeFuncSummary(0, next); err != nil || ok ||
		summary != (FuncSummary{}) || *reads != 0 {
		t.Fatalf("zero summary = %+v, %t, %v with %d reads",
			summary, ok, err, *reads)
	}

	next, reads = read(uint64(MaySuspend), uint64(NeedsPreempt))
	summary, ok, err := DecodeFuncSummary(legacySummaryVersion, next)
	want := FuncSummary{Effect: MaySuspend, Exec: NeedsPreempt}
	if err != nil || !ok || summary != want || *reads != 2 {
		t.Fatalf("legacy summary = %+v, %t, %v with %d reads, want %+v",
			summary, ok, err, *reads, want)
	}

	next, reads = read(uint64(NoSuspend), uint64(NeedsSystemABI),
		uint64(FactoryABI1))
	summary, ok, err = DecodeFuncSummary(SummaryVersion, next)
	want = FuncSummary{
		Exec: NeedsSystemABI, Factory: FactoryABI1,
	}
	if err != nil || !ok || summary != want || *reads != 3 {
		t.Fatalf("current summary = %+v, %t, %v with %d reads, want %+v",
			summary, ok, err, *reads, want)
	}

	next, reads = read(uint64(MaySuspend), 0, 2)
	summary, ok, err = DecodeFuncSummary(SummaryVersion, next)
	want = FuncSummary{Effect: MaySuspend, Factory: FactoryABI(2)}
	if err != nil || !ok || summary != want || *reads != 3 {
		t.Fatalf("future factory summary = %+v, %t, %v with %d reads, want %+v",
			summary, ok, err, *reads, want)
	}

	for _, test := range []struct {
		name    string
		version uint64
		values  []uint64
		want    string
	}{
		{
			name: "version", version: SummaryVersion + 1,
			want: "unsupported coroutine summary version",
		},
		{
			name: "effect", version: SummaryVersion,
			values: []uint64{256},
			want:   "invalid coroutine effect",
		},
		{
			name: "exec", version: SummaryVersion,
			values: []uint64{
				uint64(NoSuspend), uint64(1) << 32,
			},
			want: "invalid coroutine execution flags",
		},
		{
			name: "factory", version: SummaryVersion,
			values: []uint64{
				uint64(NoSuspend), 0, 257,
			},
			want: "invalid coroutine factory ABI",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, _ := read(test.values...)
			if _, _, err := DecodeFuncSummary(test.version, next); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeFuncSummary error = %v, want %q",
					err, test.want)
			}
		})
	}
}

func TestPublishSummary(t *testing.T) {
	fn := testFunc("published")
	want := FuncSummary{
		Effect: MaySuspend, Exec: NeedsSystemABI, Factory: FactoryABI1,
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{
		fn: {
			Func: fn, Effect: want.Effect, Exec: want.Exec,
			Factory: want.Factory,
		},
	}}
	plan.PublishSummaries()
	if got, ok := Summary(fn); !ok || got != want {
		t.Fatalf("Summary = %+v, %t, want %+v", got, ok, want)
	}
}

func TestEdgeNeedsCoroEntry(t *testing.T) {
	plan := &Plan{Functions: make(map[*ir.Func]*Function)}
	if !plan.edgeNeedsCoroEntry(Edge{Unknown: true}) {
		t.Fatal("unknown edge does not require a coroutine entry")
	}
	if got := plan.edgeEffect(Edge{Unknown: true}); got != NoSuspend {
		t.Fatalf("unknown edge effect = %v, want nosuspend fallback", got)
	}
	if plan.edgeNeedsCoroEntry(Edge{Imported: FuncSummary{}}) {
		t.Fatal("plain imported edge requires a coroutine entry")
	}
	if !plan.edgeNeedsCoroEntry(Edge{
		Imported: FuncSummary{Exec: NeedsSystemABI},
	}) {
		t.Fatal("System ABI imported edge does not require a coroutine entry")
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
