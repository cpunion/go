// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"bytes"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"strings"
	"testing"
)

func TestAnalyzeAndFreeze(t *testing.T) {
	var positions src.PosTable
	file := src.NewFileBase("analysis.go", "/analysis.go")
	nextLine := uint(1)
	pos := func() src.XPos {
		position := positions.XPos(src.MakePos(file, nextLine, 2))
		nextLine++
		return position
	}

	pkg := types.NewPkg("example.com/analysis", "analysis")
	newFunction := func(name string) *ir.Func {
		return newSiteTestFunc(pkg, name, pos())
	}
	call := func(target *ir.Func) *ir.CallExpr {
		return ir.NewCallExpr(pos(), ir.OCALLFUNC, target.Nname, nil)
	}

	channel := ir.NewNameAt(pos(), pkg.Lookup("ch"), types.NewChan(types.Types[types.TINT], types.Cboth))
	channel.Class = ir.PEXTERN
	leaf := newFunction("leaf")
	leaf.Body = ir.Nodes{ir.NewSendStmt(pos(), channel, channel)}

	receive := newFunction("receive")
	receive.Body = ir.Nodes{ir.NewUnaryExpr(pos(), ir.ORECV, channel)}

	channelRange := newFunction("channelRange")
	channelRange.Body = ir.Nodes{ir.NewRangeStmt(pos(), nil, nil, channel, nil, false)}

	selecting := newFunction("selecting")
	selecting.Body = ir.Nodes{ir.NewSelectStmt(pos(), nil)}

	direct := newFunction("direct")
	direct.Body = ir.Nodes{call(leaf)}

	deferred := newFunction("deferred")
	deferredCall := call(leaf)
	deferred.Body = ir.Nodes{ir.NewGoDeferStmt(pos(), ir.ODEFER, deferredCall)}

	launched := newFunction("launched")
	launchedCall := call(leaf)
	launched.Body = ir.Nodes{ir.NewGoDeferStmt(pos(), ir.OGO, launchedCall)}

	dynamic := newFunction("dynamic")
	functionValue := ir.NewNameAt(pos(), pkg.Lookup("functionValue"), types.NewSignature(nil, nil, nil))
	functionValue.Class = ir.PAUTO
	dynamic.Body = ir.Nodes{ir.NewCallExpr(pos(), ir.OCALLFUNC, functionValue, nil)}

	launchedDynamic := newFunction("launchedDynamic")
	dynamicCall := ir.NewCallExpr(pos(), ir.OCALLFUNC, functionValue, nil)
	launchedDynamic.Body = ir.Nodes{ir.NewGoDeferStmt(pos(), ir.OGO, dynamicCall)}

	imported := newFunction("imported")
	SetSummary(imported, MaySuspend)
	importedCaller := newFunction("importedCaller")
	importedCaller.Body = ir.Nodes{call(imported)}

	interfaceCaller := newFunction("interfaceCaller")
	interfaceCaller.Body = ir.Nodes{ir.NewCallExpr(pos(), ir.OCALLINTER, functionValue, nil)}

	cycleA := newFunction("cycleA")
	cycleB := newFunction("cycleB")
	cycleA.Body = ir.Nodes{call(cycleB)}
	cycleB.Body = ir.Nodes{call(cycleA), ir.NewUnaryExpr(pos(), ir.ORECV, channel)}

	functions := []*ir.Func{
		leaf, receive, channelRange, selecting, direct, deferred, launched,
		dynamic, launchedDynamic, importedCaller, interfaceCaller, cycleA, cycleB,
	}
	analysis := Analyze(functions)

	for _, fn := range []*ir.Func{
		leaf, receive, channelRange, selecting, direct, deferred, dynamic,
		importedCaller, interfaceCaller, cycleA, cycleB,
	} {
		if got := analysis.funcs[fn].effect; got != MaySuspend {
			t.Errorf("%s effect = %v, want %v", ir.PkgFuncName(fn), got, MaySuspend)
		}
	}
	for _, fn := range []*ir.Func{launched, launchedDynamic} {
		if got := analysis.funcs[fn].effect; got != NoSuspend {
			t.Errorf("%s effect = %v, want %v", ir.PkgFuncName(fn), got, NoSuspend)
		}
	}
	if !analysis.funcs[cycleA].recursive || !analysis.funcs[cycleB].recursive {
		t.Error("recursive component was not marked recursive")
	}

	plan, err := analysis.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	checkSite := func(fn *ir.Func, kind SiteKind, operation OperationKind) {
		t.Helper()
		fp, ok := plan.Lookup(fn)
		if !ok {
			t.Fatalf("%s has no function plan", ir.PkgFuncName(fn))
		}
		for _, site := range fp.Sites {
			if site.Kind == kind && site.Operation == operation {
				return
			}
		}
		t.Errorf("%s has no %s/%s site: %+v", ir.PkgFuncName(fn), kind, operation, fp.Sites)
	}
	checkSite(leaf, SitePark, OperationChannelSend)
	checkSite(receive, SitePark, OperationChannelReceive)
	checkSite(channelRange, SitePark, OperationChannelRange)
	checkSite(selecting, SitePark, OperationChannelSelect)
	checkSite(direct, SiteAwait, OperationNone)
	checkSite(deferred, SiteAwait, OperationNone)
	checkSite(launched, SiteSpawn, OperationNone)
	checkSite(dynamic, SiteDispatch, OperationNone)
	checkSite(launchedDynamic, SiteSpawn, OperationNone)
	checkSite(importedCaller, SiteAwait, OperationNone)
	checkSite(interfaceCaller, SiteDispatch, OperationNone)

	analysis.PublishSummaries()
	for _, fn := range functions {
		got, ok := Summary(fn)
		if !ok || got != analysis.funcs[fn].effect {
			t.Errorf("Summary(%s) = %v, %t", ir.PkgFuncName(fn), got, ok)
		}
	}

	var dump bytes.Buffer
	analysis.Dump(&dump)
	for _, text := range []string{
		"func=example.com/analysis.direct effect=may-suspend",
		"edge=direct caller=example.com/analysis.direct callee=example.com/analysis.leaf",
		"edge=go caller=example.com/analysis.launched",
		"callee=<dynamic> unknown=true",
		"caller=example.com/analysis.importedCaller callee=example.com/analysis.imported unknown=false effect=may-suspend",
		"caller=example.com/analysis.interfaceCaller callee=<interface> unknown=true",
	} {
		if !strings.Contains(dump.String(), text) {
			t.Errorf("analysis dump does not contain %q\n%s", text, dump.String())
		}
	}
}

func TestAnalysisHelpers(t *testing.T) {
	var positions src.PosTable
	pos := positions.XPos(src.MakePos(src.NewFileBase("helpers.go", "/helpers.go"), 1, 1))
	localPkg := types.NewPkg("example.com/helpers", "helpers")
	local := newSiteTestFunc(localPkg, "local", pos)
	imported := newSiteTestFunc(localPkg, "imported", pos)
	analysis := &Analysis{funcs: map[*ir.Func]*funcInfo{
		local: {fn: local, effect: NoSuspend},
	}}

	if analysis.callMaySuspend(callEdge{callee: local}) {
		t.Error("NoSuspend local call may suspend")
	}
	if !analysis.callMaySuspend(callEdge{callee: imported, imported: MaySuspend}) {
		t.Error("MaySuspend imported call does not suspend")
	}
	if got := analysis.callEffect(callEdge{imported: MaySuspend}); got != MaySuspend {
		t.Errorf("imported call effect = %v", got)
	}

	if got := symbolName(nil); got != "<dynamic>" {
		t.Errorf("symbolName(nil) = %q", got)
	}
	noPathPkg := types.NewPkg("", "")
	name := ir.NewNameAt(pos, noPathPkg.Lookup("f"), nil)
	if got := symbolName(name); got != "f" {
		t.Errorf("symbolName(no package path) = %q", got)
	}
}

func TestAnalysisValueStrings(t *testing.T) {
	for _, test := range []struct {
		got  string
		want string
	}{
		{NoSuspend.String(), "nosuspend"},
		{MaySuspend.String(), "may-suspend"},
		{Effect(255).String(), "Effect(255)"},
		{directCall.String(), "direct"},
		{deferCall.String(), "defer"},
		{goCall.String(), "go"},
		{callKind(255).String(), "callKind(255)"},
		{channelSend.String(), "channel-send"},
		{channelReceive.String(), "channel-receive"},
		{channelSelect.String(), "channel-select"},
		{channelRange.String(), "channel-range"},
		{unknownCall.String(), "unknown-call"},
		{suspendReason(255).String(), "suspendReason(255)"},
	} {
		if test.got != test.want {
			t.Errorf("String() = %q, want %q", test.got, test.want)
		}
	}
}
