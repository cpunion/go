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

type basicObjectRecipe uint8

const (
	basicObjectRecipeNone basicObjectRecipe = iota
	basicObjectRecipeTimer
	basicObjectRecipeFile
	basicObjectRecipeSocket
)

func basicObjectRecipeForName(name string) basicObjectRecipe {
	switch {
	case strings.HasSuffix(name, ".yieldOnce"):
		return basicObjectRecipeTimer
	case strings.HasSuffix(name, ".blockingReadOnce"):
		return basicObjectRecipeFile
	case strings.HasSuffix(name, ".blockingSocketReadOnce"):
		return basicObjectRecipeSocket
	default:
		return basicObjectRecipeNone
	}
}

var objectArtifacts struct {
	sync.Mutex
	list []objectArtifact
}

// BasicObjectCandidate reports whether fn contains a private suspension
// marker used by an object/link vertical slice. The final SSA recognizer still
// validates the complete function shape before taking ownership.
func BasicObjectCandidate(fn *ir.Func) bool {
	markers := 0
	ir.Visit(fn, func(n ir.Node) {
		call, ok := n.(*ir.CallExpr)
		if !ok || call.Op() != ir.OCALLFUNC {
			return
		}
		name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
		if basicObjectRecipeForName(symbolName(name)) != basicObjectRecipeNone {
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
	return basicObjectLLVMModuleForTarget(f, buildcfg.GOOS, buildcfg.GOARCH)
}

func basicObjectLLVMModuleForTarget(f *ssa.Func, goos, goarch string) (goName, hostName string, module []byte, matched bool, err error) {
	var markerCalls []*ssa.Value
	recipe := basicObjectRecipeNone
	calls := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op.IsCall() {
				calls++
			}
			if v.Op == ssa.OpStaticCall {
				candidate := basicObjectRecipeForName(callName(v))
				if candidate == basicObjectRecipeNone {
					continue
				}
				markerCalls = append(markerCalls, v)
				if recipe == basicObjectRecipeNone {
					recipe = candidate
				}
			}
		}
	}
	if len(markerCalls) == 0 {
		return "", "", nil, false, nil
	}
	if len(markerCalls) != 1 {
		return "", "", nil, true, fmt.Errorf("found %d coroutine marker calls, want exactly 1", len(markerCalls))
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
		return "", "", nil, true, fmt.Errorf("coroutine marker call has %d arguments, want only memory", len(call.Args))
	}
	memory := result.Args[1]
	if memory.Op != ssa.OpSelectN || memory.AuxInt != 0 || len(memory.Args) != 1 || memory.Args[0] != call {
		return "", "", nil, true, fmt.Errorf("return memory is not coroutine marker result memory")
	}
	if f.OwnAux == nil || f.OwnAux.Fn == nil || f.OwnAux.Fn.Name == "" {
		return "", "", nil, true, fmt.Errorf("function has no linker symbol")
	}

	goName = f.OwnAux.Fn.Name
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s<%d>", goName, f.ABISelf.Which())))
	hostName = fmt.Sprintf("go_coro_%x", digest[:8])
	render := renderBasicObjectLLVMForTarget
	switch recipe {
	case basicObjectRecipeFile:
		render = renderFileObjectLLVMForTarget
	case basicObjectRecipeSocket:
		render = renderSocketObjectLLVMForTarget
	}
	module, err = render(hostName, value.AuxInt, goos, goarch)
	if err != nil {
		return "", "", nil, true, err
	}
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
