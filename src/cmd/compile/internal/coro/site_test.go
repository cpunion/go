// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"bytes"
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
	"cmd/internal/sys"
	"strings"
	"testing"
)

func init() {
	if base.Ctxt == nil {
		base.Ctxt = &obj.Link{Arch: &obj.LinkArch{Arch: &sys.Arch{Alignment: 1}}}
	}
	if types.Types[types.TINT] == nil {
		types.PtrSize = 8
		types.RegSize = 8
		types.MaxWidth = 1 << 50
		typecheck.InitUniverse()
	}
}

func newSiteTestFunc(pkg *types.Pkg, name string, pos src.XPos) *ir.Func {
	fn := ir.NewFunc(pos, pos, pkg.Lookup(name), types.NewSignature(nil, nil, nil))
	fn.ABI = obj.ABIInternal
	fn.Nname.Defn = fn
	return fn
}

func TestFreezeSitePlans(t *testing.T) {
	var positions src.PosTable
	file := src.NewFileBase("p.go", "/p.go")
	pos := func(line uint) src.XPos {
		return positions.XPos(src.MakePos(file, line, 2))
	}

	pkg := types.NewPkg("example.com/p", "p")
	suspending := newSiteTestFunc(pkg, "suspending", pos(10))
	plain := newSiteTestFunc(pkg, "plain", pos(20))
	caller := newSiteTestFunc(pkg, "caller", pos(30))

	callerInfo := &funcInfo{
		fn:     caller,
		effect: MaySuspend,
		calls: []callEdge{
			{kind: directCall, callee: suspending, calleeName: "example.com/p.suspending"},
			{kind: directCall, callee: plain, calleeName: "example.com/p.plain"},
			{kind: directCall, calleeName: "<dynamic>", unknown: true},
			{kind: goCall, callee: suspending, calleeName: "example.com/p.suspending"},
			{kind: directCall, calleeName: "example.com/imported.suspending", imported: MaySuspend},
		},
		sites: []siteCandidate{
			{kind: operationCandidate, ordinal: 0, pos: pos(31), reason: channelSend},
			{kind: operationCandidate, ordinal: 1, pos: pos(32), reason: channelReceive},
			{kind: operationCandidate, ordinal: 2, pos: pos(33), reason: channelSelect},
			{kind: operationCandidate, ordinal: 3, pos: pos(34), reason: channelRange},
			{kind: callCandidate, ordinal: 4, pos: pos(35), callIndex: 0},
			{kind: callCandidate, ordinal: 5, pos: pos(36), callIndex: 1},
			{kind: callCandidate, ordinal: 6, pos: pos(37), callIndex: 2},
			{kind: callCandidate, ordinal: 7, pos: pos(38), callIndex: 3},
			{kind: callCandidate, ordinal: 8, pos: pos(39), callIndex: 4},
		},
	}
	analysis := &Analysis{funcs: map[*ir.Func]*funcInfo{
		suspending: {fn: suspending, effect: MaySuspend},
		plain:      {fn: plain, effect: NoSuspend},
		caller:     callerInfo,
	}}

	plan, err := analysis.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := plan.Lookup(caller)
	if !ok {
		t.Fatal("caller has no function plan")
	}
	if fp.ID != "example.com/p.caller,1" {
		t.Fatalf("caller ID = %q", fp.ID)
	}
	if fp.Effect != MaySuspend {
		t.Fatalf("caller effect = %v", fp.Effect)
	}

	want := []struct {
		ordinal   uint32
		kind      SiteKind
		operation OperationKind
		callee    FuncID
	}{
		{0, SitePark, OperationChannelSend, ""},
		{1, SitePark, OperationChannelReceive, ""},
		{2, SitePark, OperationChannelSelect, ""},
		{3, SitePark, OperationChannelRange, ""},
		{4, SiteAwait, OperationNone, "example.com/p.suspending,1"},
		{6, SiteDispatch, OperationNone, ""},
		{7, SiteSpawn, OperationNone, "example.com/p.suspending,1"},
		{8, SiteAwait, OperationNone, "example.com/imported.suspending"},
	}
	if len(fp.Sites) != len(want) {
		t.Fatalf("caller has %d sites, want %d", len(fp.Sites), len(want))
	}
	for i, expected := range want {
		site := fp.Sites[i]
		if site.ID.Ordinal != expected.ordinal || site.Kind != expected.kind ||
			site.Operation != expected.operation || site.Callee != expected.callee {
			t.Errorf("site %d = %+v, want ordinal=%d kind=%v operation=%v callee=%q",
				i, site, expected.ordinal, expected.kind, expected.operation, expected.callee)
		}
	}

	funcs := plan.Funcs()
	if len(funcs) != 3 {
		t.Fatalf("plan has %d functions, want 3", len(funcs))
	}
	funcs[0] = nil
	if plan.Funcs()[0] == nil {
		t.Fatal("Funcs returned the plan's mutable slice")
	}

	var dump bytes.Buffer
	plan.Dump(&dump)
	for _, text := range []string{
		"site=example.com/p.caller,1:4 kind=await",
		"operation=channel-send",
		"kind=dispatch",
		"kind=spawn",
	} {
		if !strings.Contains(dump.String(), text) {
			t.Errorf("plan dump does not contain %q\n%s", text, dump.String())
		}
	}

	previous := CurrentPlan()
	t.Cleanup(func() { SetCurrentPlan(previous) })
	SetCurrentPlan(plan)
	if CurrentPlan() != plan {
		t.Fatal("CurrentPlan did not return the installed plan")
	}
}

func TestEmptyPlanLookups(t *testing.T) {
	var plan *Plan
	if fp, ok := plan.Lookup(nil); ok || fp != nil {
		t.Fatalf("nil plan lookup = %v, %t", fp, ok)
	}
	if funcs := plan.Funcs(); funcs != nil {
		t.Fatalf("nil plan funcs = %v", funcs)
	}
}

func TestFreezeRejectsInvalidAnalysis(t *testing.T) {
	if _, err := (*Analysis)(nil).Freeze(); err == nil {
		t.Fatal("nil analysis did not fail")
	}

	fn := new(ir.Func)
	analysis := &Analysis{funcs: map[*ir.Func]*funcInfo{
		fn: {fn: fn, effect: MaySuspend},
	}}
	if _, err := analysis.Freeze(); err == nil || !strings.Contains(err.Error(), "no stable identity") {
		t.Fatalf("invalid identity error = %v", err)
	}

	var positions src.PosTable
	pos := positions.XPos(src.MakePos(src.NewFileBase("duplicate.go", "/duplicate.go"), 1, 1))
	pkg := types.NewPkg("example.com/duplicate", "duplicate")
	first := newSiteTestFunc(pkg, "f", pos)
	second := newSiteTestFunc(pkg, "f", pos)
	analysis = &Analysis{funcs: map[*ir.Func]*funcInfo{
		first:  {fn: first, effect: MaySuspend},
		second: {fn: second, effect: MaySuspend},
	}}
	if _, err := analysis.Freeze(); err == nil || !strings.Contains(err.Error(), "duplicate coroutine function identity") {
		t.Fatalf("duplicate identity error = %v", err)
	}
}

func TestPlanValueStrings(t *testing.T) {
	if got := FuncID("p.f").String(); got != "p.f" {
		t.Fatalf("FuncID.String() = %q", got)
	}
	if got := (SiteID{Func: "p.f", Ordinal: 3}).String(); got != "p.f:3" {
		t.Fatalf("SiteID.String() = %q", got)
	}
	for _, test := range []struct {
		got  string
		want string
	}{
		{SiteAwait.String(), "await"},
		{SitePark.String(), "park"},
		{SiteSpawn.String(), "spawn"},
		{SiteDispatch.String(), "dispatch"},
		{SiteKind(0).String(), "SiteKind(0)"},
		{OperationNone.String(), "-"},
		{OperationChannelSend.String(), "channel-send"},
		{OperationChannelReceive.String(), "channel-receive"},
		{OperationChannelSelect.String(), "channel-select"},
		{OperationChannelRange.String(), "channel-range"},
		{OperationKind(255).String(), "OperationKind(255)"},
	} {
		if test.got != test.want {
			t.Errorf("String() = %q, want %q", test.got, test.want)
		}
	}
}

func TestFreezeRejectsInvalidCandidate(t *testing.T) {
	var positions src.PosTable
	pos := positions.XPos(src.MakePos(src.NewFileBase("p.go", "/p.go"), 1, 1))
	pkg := types.NewPkg("example.com/invalid", "invalid")
	fn := newSiteTestFunc(pkg, "f", pos)

	tests := []struct {
		name      string
		candidate siteCandidate
	}{
		{"candidate kind", siteCandidate{kind: siteCandidateKind(255)}},
		{"call index", siteCandidate{kind: callCandidate, callIndex: 1}},
		{"operation", siteCandidate{kind: operationCandidate, reason: unknownCall}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			analysis := &Analysis{funcs: map[*ir.Func]*funcInfo{
				fn: {
					fn:     fn,
					effect: MaySuspend,
					sites:  []siteCandidate{test.candidate},
				},
			}}
			defer func() {
				if recover() == nil {
					t.Fatal("Freeze did not panic")
				}
			}()
			_, _ = analysis.Freeze()
		})
	}
}

func TestFuncIDWrapperKinds(t *testing.T) {
	var positions src.PosTable
	pos := positions.XPos(src.MakePos(src.NewFileBase("wrapper.go", "/wrapper.go"), 1, 1))
	pkg := types.NewPkg("example.com/wrapper", "wrapper")

	abiWrapper := newSiteTestFunc(pkg, "abiWrapper", pos)
	abiWrapper.SetABIWrapper(true)
	if got := funcID(abiWrapper); got != "example.com/wrapper.abiWrapper,1/abi-wrapper" {
		t.Errorf("ABI wrapper ID = %q", got)
	}

	wrapper := newSiteTestFunc(pkg, "wrapper", pos)
	wrapper.SetWrapper(true)
	if got := funcID(wrapper); got != "example.com/wrapper.wrapper,1/wrapper" {
		t.Errorf("wrapper ID = %q", got)
	}
}
