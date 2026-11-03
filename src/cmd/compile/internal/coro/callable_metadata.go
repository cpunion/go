// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"internal/abi"
	"slices"
	"strings"
)

// Callable companions take an opaque source environment and a typed request.
// Each request retains the original parameter types followed by typed result
// pointers. The signature descriptor proves that both sides use this layout.
// No source function value, interface layout, or factory ABI is changed.
var nativeCallableProviders = make(map[*ir.Func]*ir.Func)

func NativeCallableProvider(fn *ir.Func) *ir.Func { return nativeCallableProviders[fn] }

func callableSignature(fn *ir.Func) *types.Type {
	return callableTypeSignature(fn.Type())
}

func callableTypeSignature(signature *types.Type) *types.Type {
	copyFields := func(fields []*types.Field) []*types.Field {
		result := make([]*types.Field, len(fields))
		for i, field := range fields {
			result[i] = types.NewField(field.Pos, nil, field.Type)
			result[i].SetIsDDD(field.IsDDD())
		}
		return result
	}
	return types.NewSignature(nil, copyFields(signature.RecvParams()), copyFields(signature.Results()))
}

func callableMethodSignature(signature *types.Type) *types.Type {
	return callableTypeSignature(types.NewSignature(nil, signature.Params(), signature.Results()))
}

func callableRequestType(signature *types.Type) *types.Type {
	var fields []*types.Field
	for i, field := range signature.Params() {
		fields = append(fields, types.NewField(field.Pos,
			types.LocalPkg.LookupNum(".coroarg", i), field.Type))
	}
	for i, field := range signature.Results() {
		fields = append(fields, types.NewField(field.Pos,
			types.LocalPkg.LookupNum(".cororesult", i), types.NewPtr(field.Type)))
	}
	typ := types.NewStruct(fields)
	typ.SetNoalg(true)
	types.CalcSize(typ)
	return typ
}

// emitNativeCallableMetadata runs after the ordinary bodies and capture
// layouts have been fixed. Only independently admitted factories receive a
// companion. Missing entries remain distinguishable from native capability.
func emitNativeCallableMetadata(candidates map[*ir.Func]*lowerCandidate, plainFactories map[*ir.Func]*ir.Func) {
	var sources []*ir.Func
	for source := range candidates {
		sources = append(sources, source)
	}
	for source := range boundInterfaceMethods {
		sources = append(sources, source)
	}
	for source := range interfaceMethodWrappers {
		if candidates[source] == nil && plainFactories[source] == nil {
			sources = append(sources, source)
		}
	}
	for source := range boundMethodValues {
		if candidates[source] == nil && plainFactories[source] == nil {
			sources = append(sources, source)
		}
	}
	for source := range plainFactories {
		if candidates[source] == nil {
			sources = append(sources, source)
		}
	}
	slices.SortFunc(sources, func(a, b *ir.Func) int {
		return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b))
	})
	for _, source := range sources {
		if dot := boundInterfaceMethods[source]; dot != nil {
			types.CalcSize(dot.X.Type())
			descriptor := emitCallableRecord(source, callableSignature(source), nil, nil, nil,
				uint64(dot.Offset()/int64(types.PtrSize)+1), 0, ".coro.callable")
			emitCallableProvider(source, descriptor)
			continue
		}
		if wrapper := interfaceMethodWrappers[source]; wrapper != nil {
			factory, abi, deferred := boundMethodFactory(wrapper.boundMethodValue, candidates, plainFactories)
			if factory == nil {
				continue
			}
			call := callableCompanion(source, factory, abi, ".coro.iface.call", true)
			var cleanup *ir.Func
			if deferred {
				cleanup = callableCompanion(source, factory, abi, ".coro.iface.defer", true)
			}
			var nilCheck uint64
			if wrapper.nilCheck {
				nilCheck = 1
			}
			method := emitCallableRecord(source, callableMethodSignature(source.Type()), call, cleanup, nil, 0, nilCheck, ".coro.interface")
			descriptor := emitCallableRecord(source, callableSignature(source), nil, nil, method, 0, 0, ".coro.callable")
			emitCallableProvider(source, descriptor)
			continue
		}
		if source.Type().HasShape() || !callableFactorySupported(source) && boundMethodValues[source] == nil {
			continue
		}
		// Bounded ordinary-call adapters already have their own native
		// proof. Expose that capability to erased dispatch too; a missing
		// ordinary adapter must still remain a missing descriptor.
		factory, factoryABI := plainFactories[source], FactoryABI1
		if candidate := candidates[source]; candidate != nil {
			factory, factoryABI = candidate.factory, candidate.factoryABI
			if candidate.rootArray != nil {
				// An erased call starts a new lineage with private array storage.
				factory = candidate.rootArray.public
			}
		}
		deferCapable := candidates[source] != nil
		if bound := boundMethodValues[source]; bound != nil {
			factory, factoryABI, deferCapable = boundMethodFactory(bound, candidates, plainFactories)
			if factory == nil {
				continue
			}
		}
		emitNativeCallable(source, factory, factoryABI, deferCapable)
	}
}

func callableCompanion(source, factory *ir.Func, factoryABI FactoryABI, suffix string, method bool) *ir.Func {
	pos := source.Pos()
	signature := callableSignature(source)
	if method {
		signature = callableMethodSignature(source.Type())
	}
	requestType := callableRequestType(signature)
	ptr := types.Types[types.TUNSAFEPTR]
	params := []*types.Field{
		types.NewField(pos, types.LocalPkg.Lookup(".coroctx"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup(".coroenvironment"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup(".cororequest"), ptr),
	}
	results := []*types.Field{
		types.NewField(pos, nil, ptr),
		types.NewField(pos, nil, stacklessResumeType()),
	}
	companion := ir.NewFunc(pos, pos, source.Sym().Pkg.Lookup(source.Sym().Name+suffix),
		types.NewSignature(nil, params, results))
	companion.SetDupok(source.Dupok())
	companion.Nname.Defn = companion
	companion.DeclareParams(true)
	ir.WithFunc(companion, func() {
		ctx := companion.Type().Param(0).Nname.(*ir.Name)
		environment := companion.Type().Param(1).Nname.(*ir.Name)
		request := companion.Type().Param(2).Nname.(*ir.Name)
		var args ir.Nodes
		if factoryABI == FactoryABI3 {
			args = append(args, ctx)
		}
		if method {
			receiver := source.Type().Recv().Type
			var value ir.Node
			if types.IsDirectIface(receiver) {
				// A direct interface receiver occupies the data word itself.
				cell := typecheck.ConvNop(typecheck.NodAddr(environment), types.Types[types.TUNSAFEPTR])
				value = typedDeref(pos, typecheck.ConvNop(cell, types.NewPtr(receiver)), receiver)
			} else {
				value = typedDeref(pos, typecheck.ConvNop(environment, types.NewPtr(receiver)), receiver)
			}
			if wrapper := interfaceMethodWrappers[source]; wrapper != nil {
				value = adjustedMethodReceiver(pos, wrapper.boundMethodValue, value)
			}
			args = append(args, value)
		} else if bound := boundMethodValues[source]; bound != nil {
			args = append(args, boundMethodReceiver(pos, bound, environment))
		}
		captures := captureTypes(source)
		if len(captures) != 0 {
			fields := []*types.Field{types.NewField(pos, types.LocalPkg.Lookup("F"), types.Types[types.TUINTPTR])}
			for i, typ := range captures {
				fields = append(fields, types.NewField(pos, types.LocalPkg.LookupNum(".corocapture", i), types.NewPtr(typ)))
			}
			envType := types.NewStruct(fields)
			envType.SetNoalg(true)
			types.CalcSize(envType)
			value := typecheck.ConvNop(environment, types.NewPtr(envType))
			for _, field := range fields[1:] {
				args = append(args, explicitFrameField(pos, value, field))
			}
		}
		value := typecheck.ConvNop(request, types.NewPtr(requestType))
		for _, field := range requestType.Fields() {
			args = append(args, explicitFrameField(pos, value, field))
		}
		call := typecheck.Call(pos, factory.Nname, args, false)
		if factoryABI == FactoryABI1 {
			companion.Body = ir.Nodes{ir.NewReturnStmt(pos, ir.Nodes{ir.NewNilExpr(pos, ptr), call})}
		} else {
			frame := typecheck.TempAt(pos, companion, ptr)
			resume := typecheck.TempAt(pos, companion, stacklessResumeType())
			companion.Body = ir.Nodes{
				ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{frame, resume}, ir.Nodes{call}),
				ir.NewReturnStmt(pos, ir.Nodes{frame, resume}),
			}
		}
		typecheck.Stmts(companion.Body)
	})
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, companion)
	// The erased adapter has its own proof, including the exact typed factory
	// it invokes. A large argument transfer must never borrow the source gate.
	newNativeStackGate(companion, companion)
	return companion
}

func emitNativeCallable(source, factory *ir.Func, factoryABI FactoryABI, deferCapable bool) {
	companion := callableCompanion(source, factory, factoryABI, ".coro.call", false)
	var deferred *ir.Func
	if deferCapable {
		deferred = callableCompanion(source, factory, factoryABI, ".coro.defer", false)
	}
	var method *ir.Name
	if source.Type().Recv() != nil {
		call := callableCompanion(source, factory, factoryABI, ".coro.iface.call", true)
		var cleanup *ir.Func
		if deferCapable {
			cleanup = callableCompanion(source, factory, factoryABI, ".coro.iface.defer", true)
		}
		method = emitCallableRecord(source, callableMethodSignature(source.Type()), call, cleanup, nil, 0, 0, ".coro.interface")
	}
	descriptor := emitCallableRecord(source, callableSignature(source), companion, deferred, method, 0, 0, ".coro.callable")
	emitCallableProvider(source, descriptor)
}

// Each entry retains its own signature, receiver convention, and stack gates.
// An interface method's request omits the receiver; its data word is passed as
// environment. A bound interface marker is resolved against its captured itab
// before native capability is granted, without invoking its ordinary wrapper.
func emitCallableRecord(source *ir.Func, signature *types.Type, companion, deferred *ir.Func,
	method *ir.Name, boundMethod, nilCheck uint64, suffix string) *ir.Name {
	pos := source.Pos()
	ptr := types.Types[types.TUNSAFEPTR]
	fields := []*types.Field{
		types.NewField(pos, types.LocalPkg.Lookup("version"), types.Types[types.TUINTPTR]),
		types.NewField(pos, types.LocalPkg.Lookup("signature"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup("gate"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup("factory"), types.Types[types.TUINTPTR]),
		types.NewField(pos, types.LocalPkg.Lookup("deferGate"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup("deferFactory"), types.Types[types.TUINTPTR]),
		types.NewField(pos, types.LocalPkg.Lookup("method"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup("boundMethod"), types.Types[types.TUINTPTR]),
		types.NewField(pos, types.LocalPkg.Lookup("nilCheck"), types.Types[types.TUINTPTR]),
	}
	descType := types.NewStruct(fields)
	types.CalcSize(descType)
	sym := source.Sym().Pkg.Lookup(source.Sym().Name + suffix)
	descriptor := ir.NewNameAt(pos, sym, descType)
	descriptor.Class = ir.PEXTERN
	sym.Def = descriptor
	data := descriptor.Linksym()
	if !data.OnList() {
		off := objw.Uintptr(data, 0, uint64(abi.CoroCallableVersion))
		reflectdata.NeedRuntimeType(signature)
		off = objw.SymPtr(data, off, reflectdata.TypeLinksym(signature), 0)
		for _, entry := range []*ir.Func{companion, deferred} {
			if entry != nil {
				off = objw.SymPtr(data, off, nativeSourceGates[entry].Linksym(), 0)
				off = objw.SymPtr(data, off, entry.Linksym(), 0)
			} else {
				off = objw.Uintptr(data, off, 0)
				off = objw.Uintptr(data, off, 0)
			}
		}
		if method != nil {
			off = objw.SymPtr(data, off, method.Linksym(), 0)
		} else {
			off = objw.Uintptr(data, off, 0)
		}
		off = objw.Uintptr(data, off, boundMethod)
		off = objw.Uintptr(data, off, nilCheck)
		flags := int16(obj.RODATA)
		if source.Dupok() {
			flags |= obj.DUPOK
		}
		objw.Global(data, int32(off), flags)
	}
	return descriptor
}

func emitCallableProvider(source *ir.Func, descriptor *ir.Name) {
	pos := source.Pos()
	ptr := types.Types[types.TUNSAFEPTR]
	provider := ir.NewFunc(pos, pos, source.Sym().Pkg.Lookup(source.Sym().Name+".coro.callableProvider"),
		types.NewSignature(nil, nil, []*types.Field{types.NewField(pos, nil, ptr)}))
	provider.SetDupok(source.Dupok())
	provider.Nname.Defn = provider
	provider.DeclareParams(true)
	ir.WithFunc(provider, func() {
		provider.Body = ir.Nodes{ir.NewReturnStmt(pos, ir.Nodes{typecheck.ConvNop(typecheck.NodAddr(descriptor), ptr)})}
		typecheck.Stmts(provider.Body)
	})
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, provider)
	nativeCallableProviders[source] = provider
}
