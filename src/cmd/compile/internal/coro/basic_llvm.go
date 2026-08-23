// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssa/block"
	"cmd/internal/obj"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// The basic LLVM proof of concept deliberately recognizes one small SSA
// recipe rather than pretending to be a general Go-to-LLVM backend. It proves
// that the pre-lower handoff can preserve a scalar across a real LLVM suspend
// point and that memory operations remain on the correct side of that point.
type basicRecipe struct {
	addend int64
}

var (
	basicWriteMu sync.Mutex
	basicWritten = make(map[string]string)
)

// WriteBasicLLVM recognizes f as the basic coroutine example and, on a match,
// writes a standalone executable LLVM module to path. The native Go backend
// still compiles f; this artifact is a vertical-slice backend experiment, not
// a Go object file.
func WriteBasicLLVM(path string, f *ssa.Func) (matched bool, err error) {
	module, matched, err := basicLLVMModule(f)
	if err != nil || !matched {
		return matched, err
	}
	if path == "" {
		return true, fmt.Errorf("empty output path")
	}

	// The backend may compile functions concurrently. A corobasic output names
	// one module, so reject ambiguous packages instead of racing or silently
	// replacing the first matching function.
	basicWriteMu.Lock()
	defer basicWriteMu.Unlock()
	if previous, ok := basicWritten[path]; ok {
		return true, fmt.Errorf("output %q already emitted for %s; also matched %s", path, previous, f.Name)
	}
	if err := os.WriteFile(path, module, 0o666); err != nil {
		return true, err
	}
	basicWritten[path] = f.Name
	return true, nil
}

func basicLLVMModule(f *ssa.Func) ([]byte, bool, error) {
	var markerCalls []*ssa.Value
	var calls, loads, stores int
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op.IsCall() {
				calls++
			}
			switch v.Op {
			case ssa.OpLoad:
				loads++
			case ssa.OpStore:
				stores++
			case ssa.OpStaticCall:
				if name := callName(v); strings.HasSuffix(name, ".yieldOnce") {
					markerCalls = append(markerCalls, v)
				}
			}
		}
	}
	if len(markerCalls) == 0 {
		return nil, false, nil
	}
	if len(markerCalls) != 1 {
		return nil, true, fmt.Errorf("found %d yieldOnce calls, want exactly 1", len(markerCalls))
	}
	if len(f.Blocks) != 1 || f.Entry == nil || f.Blocks[0] != f.Entry {
		return nil, true, fmt.Errorf("function has %d blocks, want one entry block", len(f.Blocks))
	}
	if calls != 1 || loads != 2 || stores != 2 {
		return nil, true, fmt.Errorf("function has calls=%d loads=%d stores=%d, want 1/2/2", calls, loads, stores)
	}

	b := f.Entry
	if b.Kind != block.BlockRet || b.NumControls() != 1 {
		return nil, true, fmt.Errorf("entry ends in %s with %d controls, want Ret with one control", b.Kind, b.NumControls())
	}
	result := b.Controls[0]
	if result.Op != ssa.OpMakeResult || len(result.Args) != 2 {
		return nil, true, fmt.Errorf("return control is %s with %d arguments, want MakeResult(value, memory)", result.Op, len(result.Args))
	}

	addend, ok := matchArgAdd(result.Args[0])
	if !ok {
		return nil, true, fmt.Errorf("return value %s is not int64 argument plus a constant", result.Args[0].LongString())
	}

	call := markerCalls[0]
	if len(call.Args) != 1 {
		return nil, true, fmt.Errorf("yieldOnce call has %d arguments, want only memory", len(call.Args))
	}
	beforeStore := call.Args[0]
	if err := matchGlobalIncrement(beforeStore, ".before", nil); err != nil {
		return nil, true, fmt.Errorf("before suspend: %w", err)
	}
	if beforeStore.Args[2].Op != ssa.OpInitMem {
		return nil, true, fmt.Errorf("before suspend: store does not start at InitMem")
	}

	afterStore := result.Args[1]
	if afterStore.Op != ssa.OpStore || len(afterStore.Args) != 3 {
		return nil, true, fmt.Errorf("after suspend: return memory is not a Store")
	}
	afterMemory := afterStore.Args[2]
	if afterMemory.Op != ssa.OpSelectN || afterMemory.AuxInt != 0 || len(afterMemory.Args) != 1 || afterMemory.Args[0] != call {
		return nil, true, fmt.Errorf("after suspend: store memory is not yieldOnce result memory")
	}
	if err := matchGlobalIncrement(afterStore, ".after", afterMemory); err != nil {
		return nil, true, fmt.Errorf("after suspend: %w", err)
	}

	return renderBasicLLVM(basicRecipe{addend: addend}), true, nil
}

func callName(v *ssa.Value) string {
	call, ok := v.Aux.(*ssa.AuxCall)
	if !ok || call.Fn == nil {
		return ""
	}
	return call.Fn.Name
}

func matchArgAdd(v *ssa.Value) (int64, bool) {
	if v.Op != ssa.OpAdd64 || len(v.Args) != 2 {
		return 0, false
	}
	for i, arg := range v.Args {
		if (arg.Op != ssa.OpArg && arg.Op != ssa.OpArgIntReg) || arg.Type.String() != "int64" {
			continue
		}
		constant := v.Args[1-i]
		if constant.Op == ssa.OpConst64 {
			return constant.AuxInt, true
		}
	}
	return 0, false
}

func matchGlobalIncrement(store *ssa.Value, suffix string, memory *ssa.Value) error {
	if store.Op != ssa.OpStore || len(store.Args) != 3 {
		return fmt.Errorf("value is not Store(address, value, memory)")
	}
	address := store.Args[0]
	symbol, ok := address.Aux.(*obj.LSym)
	if address.Op != ssa.OpAddr || !ok || !strings.HasSuffix(symbol.Name, suffix) {
		return fmt.Errorf("store address is not global %s", suffix)
	}
	if memory != nil && store.Args[2] != memory {
		return fmt.Errorf("store uses the wrong memory predecessor")
	}

	increment := store.Args[1]
	if increment.Op != ssa.OpAdd64 || len(increment.Args) != 2 {
		return fmt.Errorf("stored value is not Add64")
	}
	var load *ssa.Value
	for i, arg := range increment.Args {
		if arg.Op != ssa.OpConst64 || arg.AuxInt != 1 {
			continue
		}
		load = increment.Args[1-i]
	}
	if load == nil || load.Op != ssa.OpLoad || len(load.Args) != 2 {
		return fmt.Errorf("stored value is not load plus one")
	}
	if load.Args[0] != address || load.Args[1] != store.Args[2] {
		return fmt.Errorf("load and store do not share address and memory")
	}
	return nil
}

func renderBasicLLVM(recipe basicRecipe) []byte {
	input := int64(40)
	expected := input + recipe.addend
	replacer := strings.NewReplacer(
		"{{ADDEND}}", strconv.FormatInt(recipe.addend, 10),
		"{{EXPECTED}}", strconv.FormatInt(expected, 10),
	)
	return []byte(replacer.Replace(basicLLVMTemplate))
}

const basicLLVMTemplate = `; Generated from the restricted Go pre-lower SSA coroutine example.
; The recognized result recipe is x + {{ADDEND}}.

@before = internal global i64 0, align 8
@after = internal global i64 0, align 8
@result = internal global i64 -1, align 8
@destroyed = internal global i64 0, align 8
@success = private unnamed_addr constant [14 x i8] c"coro-basic-ok\00"

declare noalias ptr @malloc(i64)
declare void @free(ptr)
declare i32 @puts(ptr)

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

define ptr @leaf(i64 %x) presplitcoroutine {
entry:
  %id = call token @llvm.coro.id(i32 0, ptr null, ptr null, ptr null)
  %size = call i64 @llvm.coro.size.i64()
  %mem = call noalias ptr @malloc(i64 %size)
  %hdl = call noalias ptr @llvm.coro.begin(token %id, ptr %mem)

  %before.old = load i64, ptr @before, align 8
  %before.new = add i64 %before.old, 1
  store i64 %before.new, ptr @before, align 8
  %answer = add i64 %x, {{ADDEND}}

  %save = call token @llvm.coro.save(ptr %hdl)
  %suspend.kind = call i8 @llvm.coro.suspend(token %save, i1 false)
  switch i8 %suspend.kind, label %suspend [
    i8 0, label %resume
    i8 1, label %cleanup
  ]

resume:
  %after.old = load i64, ptr @after, align 8
  %after.new = add i64 %after.old, 1
  store i64 %after.new, ptr @after, align 8
  store i64 %answer, ptr @result, align 8
  %final.kind = call i8 @llvm.coro.suspend(token none, i1 true)
  switch i8 %final.kind, label %suspend [
    i8 0, label %invalid.resume
    i8 1, label %cleanup
  ]

invalid.resume:
  unreachable

cleanup:
  store i64 1, ptr @destroyed, align 8
  %free.mem = call ptr @llvm.coro.free(token %id, ptr %hdl)
  call void @free(ptr %free.mem)
  br label %suspend

suspend:
  %end = call i1 @llvm.coro.end(ptr %hdl, i1 false, token none)
  ret ptr %hdl
}

define i32 @main() {
entry:
  %hdl = call ptr @leaf(i64 40)
  %before.first = load i64, ptr @before, align 8
  %after.first = load i64, ptr @after, align 8
  %result.first = load i64, ptr @result, align 8
  %done.first = call i1 @llvm.coro.done(ptr %hdl)
  %before.ok = icmp eq i64 %before.first, 1
  %after.ok = icmp eq i64 %after.first, 0
  %result.pending = icmp eq i64 %result.first, -1
  %not.done = xor i1 %done.first, true
  %initial.1 = and i1 %before.ok, %after.ok
  %initial.2 = and i1 %result.pending, %not.done
  %initial.ok = and i1 %initial.1, %initial.2
  br i1 %initial.ok, label %resume, label %fail.initial

resume:
  call void @llvm.coro.resume(ptr %hdl)
  %before.second = load i64, ptr @before, align 8
  %after.second = load i64, ptr @after, align 8
  %result.second = load i64, ptr @result, align 8
  %done.second = call i1 @llvm.coro.done(ptr %hdl)
  %before.still.one = icmp eq i64 %before.second, 1
  %after.once = icmp eq i64 %after.second, 1
  %answer.ok = icmp eq i64 %result.second, {{EXPECTED}}
  %resumed.1 = and i1 %before.still.one, %after.once
  %resumed.2 = and i1 %answer.ok, %done.second
  %resumed.ok = and i1 %resumed.1, %resumed.2
  br i1 %resumed.ok, label %success, label %fail.resumed

success:
  call void @llvm.coro.destroy(ptr %hdl)
  %destroyed.value = load i64, ptr @destroyed, align 8
  %destroyed.ok = icmp eq i64 %destroyed.value, 1
  br i1 %destroyed.ok, label %success.print, label %fail.destroy

success.print:
  %ignored = call i32 @puts(ptr @success)
  ret i32 0

fail.initial:
  call void @llvm.coro.destroy(ptr %hdl)
  ret i32 10

fail.resumed:
  call void @llvm.coro.destroy(ptr %hdl)
  ret i32 20

fail.destroy:
  ret i32 30
}
`
