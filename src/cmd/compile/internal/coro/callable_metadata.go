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
	copyFields := func(fields []*types.Field) []*types.Field {
		result := make([]*types.Field, len(fields))
		for i, field := range fields {
			result[i] = types.NewField(field.Pos, nil, field.Type)
			result[i].SetIsDDD(field.IsDDD())
		}
		return result
	}
	return types.NewSignature(nil, copyFields(fn.Type().RecvParams()), copyFields(fn.Type().Results()))
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
func emitNativeCallableMetadata(candidates map[*ir.Func]*lowerCandidate) {
	var sources []*ir.Func
	for source := range candidates {
		sources = append(sources, source)
	}
	slices.SortFunc(sources, func(a, b *ir.Func) int {
		return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b))
	})
	for _, source := range sources {
		candidate := candidates[source]
		if source.Type().HasShape() || !callableFactorySupported(source) {
			continue
		}
		emitNativeCallable(source, candidate.factory, candidate.factoryABI)
	}
}

func emitNativeCallable(source, factory *ir.Func, factoryABI FactoryABI) {
	pos := source.Pos()
	signature := callableSignature(source)
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
	companion := ir.NewFunc(pos, pos, source.Sym().Pkg.Lookup(source.Sym().Name+".coro.call"),
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
		captures := captureTypes(source)
		if len(captures) != 0 {
			fields := []*types.Field{types.NewField(pos, nil, types.Types[types.TUINTPTR])}
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
				ir.NewAssignListStmt(pos, ir.OAS2FUNC, ir.Nodes{frame, resume}, ir.Nodes{call}),
				ir.NewReturnStmt(pos, ir.Nodes{frame, resume}),
			}
		}
		typecheck.Stmts(companion.Body)
	})
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, companion)
	// The erased adapter has its own proof, including the exact typed factory
	// it invokes. A large argument transfer must never borrow the source gate.
	newNativeStackGate(companion, companion)
	gate := nativeSourceGates[companion]

	// These four words match runtime.stacklessCoroCallable. All pointers refer
	// to immutable module data or code; this descriptor owns no heap objects.
	fields := []*types.Field{
		types.NewField(pos, types.LocalPkg.Lookup("version"), types.Types[types.TUINTPTR]),
		types.NewField(pos, types.LocalPkg.Lookup("signature"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup("gate"), ptr),
		types.NewField(pos, types.LocalPkg.Lookup("factory"), types.Types[types.TUINTPTR]),
	}
	descType := types.NewStruct(fields)
	types.CalcSize(descType)
	sym := source.Sym().Pkg.Lookup(source.Sym().Name + ".coro.callable")
	descriptor := ir.NewNameAt(pos, sym, descType)
	descriptor.Class = ir.PEXTERN
	sym.Def = descriptor
	data := descriptor.Linksym()
	off := objw.Uintptr(data, 0, uint64(abi.CoroCallableVersion))
	reflectdata.NeedRuntimeType(signature)
	off = objw.SymPtr(data, off, reflectdata.TypeLinksym(signature), 0)
	off = objw.SymPtr(data, off, gate.Linksym(), 0)
	off = objw.SymPtr(data, off, companion.Linksym(), 0)
	flags := int16(obj.RODATA)
	if source.Dupok() {
		flags |= obj.DUPOK
	}
	objw.Global(data, int32(off), flags)

	// Funcdata accepts section-relative offsets rather than absolute pointers.
	// A tiny static provider keeps the descriptor in ordinary rodata (including
	// PIE relocation handling) without a process-wide initialization registry.
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
