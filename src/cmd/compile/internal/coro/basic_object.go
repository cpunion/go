// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"bytes"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssa/block"
	"cmd/internal/coroobj"
	"crypto/sha256"
	"fmt"
	"internal/buildcfg"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// NativeObject is a native object member emitted for a coroutine-owned
// function. Name is suitable for the short-name archive format.
type NativeObject struct {
	Name string
	Data []byte
}

type objectArtifact struct {
	object NativeObject
	symbol coroobj.Symbol
}

var objectArtifacts struct {
	sync.Mutex
	list []objectArtifact
}

// BasicObjectCandidate reports whether fn contains the private suspension
// marker used by the object/link vertical slice. The final SSA recognizer
// still validates the complete function shape before taking ownership.
func BasicObjectCandidate(fn *ir.Func) bool {
	markers := 0
	ir.Visit(fn, func(n ir.Node) {
		call, ok := n.(*ir.CallExpr)
		if !ok || call.Op() != ir.OCALLFUNC {
			return
		}
		name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
		if strings.HasSuffix(symbolName(name), ".yieldOnce") {
			markers++
		}
	})
	return markers != 0
}

// WriteBasicObject recognizes the restricted object/link example, compiles
// its LLVM module with clang, and records the resulting native object. A true
// result transfers ownership of f away from the native Go backend.
func WriteBasicObject(clang string, f *ssa.Func) (handled bool, err error) {
	goName, hostName, module, handled, err := basicObjectLLVMModule(f)
	if err != nil || !handled {
		return handled, err
	}
	data, err := compileLLVMObject(clang, module)
	if err != nil {
		return true, err
	}

	digest := sha256.Sum256([]byte(fmt.Sprintf("%s<%d>", goName, f.ABISelf.Which())))
	artifact := objectArtifact{
		object: NativeObject{
			Name: fmt.Sprintf("co%x.o", digest[:6]),
			Data: data,
		},
		symbol: coroobj.Symbol{
			GoName:   goName,
			GoABI:    int(f.ABISelf.Which()),
			HostName: hostName,
		},
	}

	objectArtifacts.Lock()
	defer objectArtifacts.Unlock()
	for _, previous := range objectArtifacts.list {
		if previous.symbol.GoName == goName && previous.symbol.GoABI == artifact.symbol.GoABI {
			return true, fmt.Errorf("native object already emitted for %s<%d>", goName, artifact.symbol.GoABI)
		}
		if previous.object.Name == artifact.object.Name {
			return true, fmt.Errorf("native object member name collision between %s and %s", previous.symbol.GoName, goName)
		}
	}
	objectArtifacts.list = append(objectArtifacts.list, artifact)
	return true, nil
}

// ObjectManifest returns the manifest for all native objects emitted by this
// compiler process.
func ObjectManifest() (*coroobj.Manifest, bool) {
	artifacts := sortedObjectArtifacts()
	if len(artifacts) == 0 {
		return nil, false
	}
	m := &coroobj.Manifest{
		Version: coroobj.Version,
		GOOS:    buildcfg.GOOS,
		GOARCH:  buildcfg.GOARCH,
		Symbols: make([]coroobj.Symbol, len(artifacts)),
	}
	for i, artifact := range artifacts {
		m.Symbols[i] = artifact.symbol
	}
	return m, true
}

// NativeObjects returns a deterministic copy of the emitted native objects.
func NativeObjects() []NativeObject {
	artifacts := sortedObjectArtifacts()
	objects := make([]NativeObject, len(artifacts))
	for i, artifact := range artifacts {
		objects[i] = NativeObject{
			Name: artifact.object.Name,
			Data: bytes.Clone(artifact.object.Data),
		}
	}
	return objects
}

func sortedObjectArtifacts() []objectArtifact {
	objectArtifacts.Lock()
	defer objectArtifacts.Unlock()
	artifacts := slices.Clone(objectArtifacts.list)
	slices.SortFunc(artifacts, func(a, b objectArtifact) int {
		return strings.Compare(a.symbol.GoName, b.symbol.GoName)
	})
	return artifacts
}

func basicObjectLLVMModule(f *ssa.Func) (goName, hostName string, module []byte, matched bool, err error) {
	var markerCalls []*ssa.Value
	calls := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op.IsCall() {
				calls++
			}
			if v.Op == ssa.OpStaticCall && strings.HasSuffix(callName(v), ".yieldOnce") {
				markerCalls = append(markerCalls, v)
			}
		}
	}
	if len(markerCalls) == 0 {
		return "", "", nil, false, nil
	}
	if len(markerCalls) != 1 {
		return "", "", nil, true, fmt.Errorf("found %d yieldOnce calls, want exactly 1", len(markerCalls))
	}
	if calls != 1 {
		return "", "", nil, true, fmt.Errorf("function has %d calls, want exactly 1", calls)
	}
	if len(f.Blocks) != 1 || f.Entry == nil || f.Blocks[0] != f.Entry {
		return "", "", nil, true, fmt.Errorf("function has %d blocks, want one entry block", len(f.Blocks))
	}
	b := f.Entry
	if b.Kind != block.BlockRet || b.NumControls() != 1 {
		return "", "", nil, true, fmt.Errorf("entry ends in %s with %d controls, want Ret with one control", b.Kind, b.NumControls())
	}
	result := b.Controls[0]
	if result.Op != ssa.OpMakeResult || len(result.Args) != 2 {
		return "", "", nil, true, fmt.Errorf("return control is %s with %d arguments, want MakeResult(value, memory)", result.Op, len(result.Args))
	}
	value := result.Args[0]
	if value.Op != ssa.OpConst64 {
		return "", "", nil, true, fmt.Errorf("return value is %s, want Const64", value.Op)
	}
	call := markerCalls[0]
	if len(call.Args) != 1 {
		return "", "", nil, true, fmt.Errorf("yieldOnce call has %d arguments, want only memory", len(call.Args))
	}
	memory := result.Args[1]
	if memory.Op != ssa.OpSelectN || memory.AuxInt != 0 || len(memory.Args) != 1 || memory.Args[0] != call {
		return "", "", nil, true, fmt.Errorf("return memory is not yieldOnce result memory")
	}
	if f.OwnAux == nil || f.OwnAux.Fn == nil || f.OwnAux.Fn.Name == "" {
		return "", "", nil, true, fmt.Errorf("function has no linker symbol")
	}

	goName = f.OwnAux.Fn.Name
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s<%d>", goName, f.ABISelf.Which())))
	hostName = fmt.Sprintf("go_coro_%x", digest[:8])
	module = renderBasicObjectLLVM(hostName, value.AuxInt)
	return goName, hostName, module, true, nil
}

func compileLLVMObject(clang string, module []byte) ([]byte, error) {
	if clang == "" {
		return nil, fmt.Errorf("empty clang path")
	}
	clangPath, err := exec.LookPath(clang)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "go-coro-object-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	irPath := filepath.Join(dir, "coro.ll")
	objectPath := filepath.Join(dir, "coro.o")
	if err := os.WriteFile(irPath, module, 0o666); err != nil {
		return nil, err
	}
	command := exec.Command(clangPath, "-x", "ir", "-O2", "-fno-ident", "-c", irPath, "-o", objectPath)
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: %v\n%s", clangPath, err, output)
	}
	data, err := os.ReadFile(objectPath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s produced an empty object", clangPath)
	}
	return data, nil
}

func renderBasicObjectLLVM(hostName string, value int64) []byte {
	return fmt.Appendf(nil, `; Generated from the restricted Go coroutine object/link example.

declare noalias ptr @malloc(i64)
declare void @free(ptr)

declare token @llvm.coro.id(i32, ptr, ptr, ptr)
declare i64 @llvm.coro.size.i64()
declare ptr @llvm.coro.begin(token, ptr)
declare token @llvm.coro.save(ptr)
declare i8 @llvm.coro.suspend(token, i1)
declare ptr @llvm.coro.free(token, ptr)
declare i1 @llvm.coro.end(ptr, i1, token)
declare void @llvm.coro.resume(ptr)
declare i1 @llvm.coro.done(ptr)
declare void @llvm.coro.destroy(ptr)

define internal ptr @%s.body(ptr %%result) presplitcoroutine {
entry:
  %%id = call token @llvm.coro.id(i32 0, ptr null, ptr null, ptr null)
  %%size = call i64 @llvm.coro.size.i64()
  %%mem = call noalias ptr @malloc(i64 %%size)
  %%hdl = call noalias ptr @llvm.coro.begin(token %%id, ptr %%mem)
  %%save = call token @llvm.coro.save(ptr %%hdl)
  %%suspend.kind = call i8 @llvm.coro.suspend(token %%save, i1 false)
  switch i8 %%suspend.kind, label %%suspend [
    i8 0, label %%resume
    i8 1, label %%cleanup
  ]

resume:
  store i64 %d, ptr %%result, align 8
  %%final.kind = call i8 @llvm.coro.suspend(token none, i1 true)
  switch i8 %%final.kind, label %%suspend [
    i8 0, label %%invalid.resume
    i8 1, label %%cleanup
  ]

invalid.resume:
  unreachable

cleanup:
  %%free.mem = call ptr @llvm.coro.free(token %%id, ptr %%hdl)
  call void @free(ptr %%free.mem)
  br label %%suspend

suspend:
  %%end = call i1 @llvm.coro.end(ptr %%hdl, i1 false, token none)
  ret ptr %%hdl
}

define i64 @%s() {
entry:
  %%result = alloca i64, align 8
  store i64 -1, ptr %%result, align 8
  %%hdl = call ptr @%s.body(ptr %%result)
  %%done.initial = call i1 @llvm.coro.done(ptr %%hdl)
  br i1 %%done.initial, label %%fail, label %%resume

resume:
  call void @llvm.coro.resume(ptr %%hdl)
  %%done = call i1 @llvm.coro.done(ptr %%hdl)
  br i1 %%done, label %%complete, label %%fail

complete:
  %%value = load i64, ptr %%result, align 8
  call void @llvm.coro.destroy(ptr %%hdl)
  ret i64 %%value

fail:
  call void @llvm.coro.destroy(ptr %%hdl)
  ret i64 -1
}
`, hostName, value, hostName, hostName)
}
