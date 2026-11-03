// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

type boundMethodValue struct {
	receiver *ir.Name
	argument ir.Node
	target   *ir.Func
}

// The frontend records the actual wrapper relationship before inlining can
// erase it. Names and wrapper flags alone never establish this capability.
var boundMethodValues = make(map[*ir.Func]*boundMethodValue)
var boundInterfaceMethods = make(map[*ir.Func]*ir.SelectorExpr)

type interfaceMethodWrapper struct {
	*boundMethodValue
	nilCheck bool
}

var interfaceMethodWrappers = make(map[*ir.Func]*interfaceMethodWrapper)

// Record only frontend-generated receiver forwarding. The ordinary wrapper
// remains intact, including panicwrap's caller-sensitive panic message.
func RecordInterfaceMethodWrapper(wrapper *ir.Func, receiver ir.Node, call *ir.CallExpr, nilCheck bool) {
	name, ok := receiver.(*ir.Name)
	if !ok || wrapper.Needctxt() || wrapper.Type().HasShape() ||
		wrapper.Type().Recv() == nil || wrapper.Type().Recv().Nname != name ||
		call.Op() != ir.OCALLFUNC || len(call.Init()) != 0 {
		return
	}
	target := ir.StaticCalleeName(call.Fun)
	if target == nil || target.Func == nil || target.Func.Type().HasShape() ||
		target.Func.Type().Recv() == nil || len(call.Args) != wrapper.Type().NumParams()+1 {
		return
	}
	interfaceMethodWrappers[wrapper] = &interfaceMethodWrapper{
		boundMethodValue: &boundMethodValue{receiver: name, argument: ir.DeepCopy(src.NoXPos, call.Args[0]), target: target.Func},
		nilCheck:         nilCheck,
	}
}

func RecordBoundMethodValue(wrapper *ir.Func, receiver ir.Node, call *ir.CallExpr) {
	name, ok := receiver.(*ir.Name)
	if !ok || wrapper.OClosure != nil || len(wrapper.ClosureVars) != 1 ||
		wrapper.ClosureVars[0] != name || !name.Byval() || wrapper.Type().HasShape() ||
		(call.Op() != ir.OCALLFUNC && call.Op() != ir.OCALLINTER) || len(call.Init()) != 0 {
		return
	}
	if call.Op() == ir.OCALLINTER {
		dot := call.Fun.(*ir.SelectorExpr)
		if dot.X == name {
			boundInterfaceMethods[wrapper] = dot
		}
		return
	}
	target := ir.StaticCalleeName(call.Fun)
	if target == nil || target.Func == nil || target.Func.Type().HasShape() ||
		target.Func.Type().Recv() == nil || len(call.Args) != wrapper.Type().NumParams()+1 {
		return
	}
	boundMethodValues[wrapper] = &boundMethodValue{
		receiver: name, argument: ir.DeepCopy(src.NoXPos, call.Args[0]), target: target.Func,
	}
}

func boundMethodFactory(bound *boundMethodValue, candidates map[*ir.Func]*lowerCandidate,
	plain map[*ir.Func]*ir.Func) (*ir.Func, FactoryABI, bool) {
	if candidate := candidates[bound.target]; candidate != nil {
		factory := candidate.factory
		if candidate.rootArray != nil {
			factory = candidate.rootArray.public
		}
		return factory, candidate.factoryABI, true
	}
	if factory := plain[bound.target]; factory != nil {
		return factory, FactoryABI1, false
	}
	if factory, ok := importedResumeFactory(bound.target); ok {
		summary, _ := Summary(bound.target)
		return factory, summary.Factory, summary.Primary() == CoroPrimary || summary.Factory == FactoryABI3
	}
	return nil, NoFactory, false
}

// A method value stores its receiver by value after the code word. Preserve
// that layout and the frontend's receiver adjustment (including promoted
// fields and implicit dereferences). The companion starts the actual method's
// activation directly, so a deferred method retains its direct recover right.
func boundMethodReceiver(pos src.XPos, bound *boundMethodValue, environment ir.Node) ir.Node {
	fields := []*types.Field{
		types.NewField(pos, typecheck.Lookup("F"), types.Types[types.TUINTPTR]),
		types.NewField(pos, typecheck.Lookup("R"), bound.receiver.Type()),
	}
	layout := types.NewStruct(fields)
	layout.SetNoalg(true)
	types.CalcSize(layout)
	value := explicitFrameField(pos, typecheck.ConvNop(environment, types.NewPtr(layout)), fields[1])
	return adjustedMethodReceiver(pos, bound, value)
}

func adjustedMethodReceiver(pos src.XPos, bound *boundMethodValue, value ir.Node) ir.Node {
	var edit func(ir.Node) ir.Node
	edit = func(n ir.Node) ir.Node {
		if n == bound.receiver {
			return value
		}
		ir.EditChildren(n, edit)
		return n
	}
	return edit(ir.DeepCopy(pos, bound.argument))
}
