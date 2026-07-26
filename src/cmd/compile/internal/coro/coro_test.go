// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testProgram = `package p

var ch chan int

func leaf() {
	ch <- 1
}

func receive() {
	<-ch
}

func direct() {
	leaf()
}

func deferred() {
	defer leaf()
}

func launched() {
	go leaf()
}

func cycleA() {
	cycleB()
}

func cycleB() {
	if len(ch) == 0 {
		cycleA()
	}
	<-ch
}

func channelRange() {
	for range ch {
	}
}

func selecting() {
	select {
	case <-ch:
	default:
	}
}

func dynamic(f func()) {
	f()
}

func launchedDynamic(f func()) {
	go f()
}
`

func compile(t *testing.T, experiment string, debug int) (string, error) {
	t.Helper()
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "p.go")
	if err := os.WriteFile(src, []byte(testProgram), 0o666); err != nil {
		t.Fatal(err)
	}

	args := []string{"tool", "compile", "-l", "-p=p", "-o", filepath.Join(tmp, "p.o")}
	if debug != 0 {
		args = append(args, fmt.Sprintf("-d=coro=%d", debug))
	}
	args = append(args, src)

	cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+experiment)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAnalysis(t *testing.T) {
	out, err := compile(t, "coro", 1)
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, out)
	}

	for _, want := range []string{
		"coro: func=p.leaf effect=may-suspend local=may-suspend recursive=false seeds=channel-send",
		"coro: func=p.receive effect=may-suspend local=may-suspend recursive=false seeds=channel-receive",
		"coro: func=p.direct effect=may-suspend local=nosuspend recursive=false seeds=-",
		"coro: func=p.deferred effect=may-suspend local=nosuspend recursive=false seeds=-",
		"coro: func=p.launched effect=nosuspend local=nosuspend recursive=false seeds=-",
		"coro: func=p.cycleA effect=may-suspend local=nosuspend recursive=true seeds=-",
		"coro: func=p.cycleB effect=may-suspend local=may-suspend recursive=true seeds=channel-receive",
		"coro: func=p.channelRange effect=may-suspend local=may-suspend recursive=false seeds=channel-range",
		"coro: func=p.selecting effect=may-suspend local=may-suspend recursive=false seeds=channel-receive,channel-select",
		"coro: func=p.dynamic effect=may-suspend local=may-suspend recursive=false seeds=unknown-call",
		"coro: func=p.launchedDynamic effect=nosuspend local=nosuspend recursive=false seeds=-",
		"coro: edge=direct caller=p.direct callee=p.leaf unknown=false",
		"coro: edge=defer caller=p.deferred callee=p.leaf unknown=false",
		"coro: edge=go caller=p.launched callee=p.leaf unknown=false",
		"coro: edge=go caller=p.launchedDynamic callee=<dynamic> unknown=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
}

func TestExperimentGate(t *testing.T) {
	out, err := compile(t, "nocoro", 1)
	if err == nil {
		t.Fatalf("compile unexpectedly succeeded\n%s", out)
	}
	if want := "-d=coro requires GOEXPERIMENT=coro"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	out, err = compile(t, "nocoro", 0)
	if err != nil {
		t.Fatalf("compile without experiment failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "coro:") {
		t.Fatalf("disabled experiment produced analysis output\n%s", out)
	}
}

func TestPreLowerHandoff(t *testing.T) {
	out, err := compile(t, "coro", 3)
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"coro: phase=pre-lower-ssa func=",
		" action=continue-native",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
}

func emitBasicLLVM(t *testing.T) (module, output string) {
	t.Helper()
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	module = filepath.Join(tmp, "basic.ll")
	src := filepath.Join(testenv.GOROOT(t), "src", "cmd", "compile", "internal", "coro", "testdata", "basic.go")
	cmd := testenv.Command(t, testenv.GoToolPath(t),
		"tool", "compile", "-l", "-p=basic",
		"-d=corobasic="+module,
		"-o", filepath.Join(tmp, "basic.o"), src)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro")
	data, err := cmd.CombinedOutput()
	output = string(data)
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"coro: phase=pre-lower-ssa func=leaf,",
		" action=emit-basic-llvm path=" + module,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("compiler output does not contain %q\n%s", want, output)
		}
	}
	return module, output
}

func TestBasicLLVMEmission(t *testing.T) {
	module, _ := emitBasicLLVM(t)
	data, err := os.ReadFile(module)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"define ptr @leaf(i64 %x) presplitcoroutine",
		"call i8 @llvm.coro.suspend(token %save, i1 false)",
		"call i8 @llvm.coro.suspend(token none, i1 true)",
		"%answer.ok = icmp eq i64 %result.second, 43",
		"call void @llvm.coro.destroy(ptr %hdl)",
		"%destroyed.ok = icmp eq i64 %destroyed.value, 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("LLVM module does not contain %q", want)
		}
	}
	if strings.Contains(text, "{{") {
		t.Errorf("LLVM module contains an unreplaced template marker")
	}
}

func llvmTool(t *testing.T, env string, names ...string) string {
	t.Helper()
	if path := os.Getenv(env); path != "" {
		return path
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skipf("LLVM execution test requires one of %s (or %s)", strings.Join(names, ", "), env)
	return ""
}

func TestBasicLLVMExecution(t *testing.T) {
	module, _ := emitBasicLLVM(t)
	opt := llvmTool(t, "LLVM_OPT", "opt", "opt-20", "opt-19", "opt-18")
	clang := llvmTool(t, "LLVM_CLANG", "clang", "clang-20", "clang-19", "clang-18")

	tmp := filepath.Dir(module)
	split := filepath.Join(tmp, "basic.split.ll")
	cmd := testenv.Command(t, opt, "-S",
		"-passes=coro-early,coro-split,coro-cleanup,verify",
		module, "-o", split)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("LLVM coroutine lowering failed: %v\n%s", err, data)
	}
	data, err := os.ReadFile(split)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@leaf.resume", "@leaf.destroy", "%leaf.Frame = type"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("split LLVM module does not contain %q", want)
		}
	}

	exe := filepath.Join(tmp, "basic-coro")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd = testenv.Command(t, clang, "-O1", split, "-o", exe)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("linking LLVM coroutine example failed: %v\n%s", err, data)
	}
	cmd = testenv.Command(t, exe)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("LLVM coroutine example failed: %v\n%s", err, data)
	} else if got, want := strings.TrimSpace(string(data)), "coro-basic-ok"; got != want {
		t.Fatalf("LLVM coroutine example output = %q, want %q", got, want)
	}
}

func TestBasicLLVMRejectsUnsupportedShape(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "bad.go")
	program := `package bad

var suspend = make(chan struct{})
var before, after int64

//go:noinline
func yieldOnce() {
	<-suspend
}

//go:noinline
func leaf(x int64) int64 {
	before++
	yieldOnce()
	yieldOnce()
	after++
	return x + 3
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(tmp, "bad.ll")
	cmd := testenv.Command(t, testenv.GoToolPath(t),
		"tool", "compile", "-l", "-p=bad",
		"-d=corobasic="+module,
		"-o", filepath.Join(tmp, "bad.o"), src)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro")
	data, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unsupported coroutine shape unexpectedly compiled\n%s", data)
	}
	if want := "basic LLVM coroutine: found 2 yieldOnce calls, want exactly 1"; !strings.Contains(string(data), want) {
		t.Fatalf("compiler output does not contain %q\n%s", want, data)
	}
	if _, err := os.Stat(module); !os.IsNotExist(err) {
		t.Fatalf("unsupported coroutine emitted module: stat error %v", err)
	}
}

func TestBasicLLVMExperimentGate(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "p.go")
	if err := os.WriteFile(src, []byte("package p\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := testenv.Command(t, testenv.GoToolPath(t),
		"tool", "compile",
		"-d=corobasic="+filepath.Join(tmp, "p.ll"),
		"-o", filepath.Join(tmp, "p.o"), src)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=nocoro")
	data, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("compile unexpectedly succeeded\n%s", data)
	}
	if want := "-d=corobasic requires GOEXPERIMENT=coro"; !strings.Contains(string(data), want) {
		t.Fatalf("compiler output does not contain %q\n%s", want, data)
	}
}

func TestInliningCompatibility(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "p.go")
	program := `package p

func leaf(ch chan int) {
	ch <- 1
}

func caller(ch chan int) {
	leaf(ch)
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	cmd := testenv.Command(t, testenv.GoToolPath(t),
		"tool", "compile", "-m=2", "-d=coro=2",
		"-p=p", "-o", filepath.Join(tmp, "p.o"), src)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro")
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, out)
	}

	for _, want := range []string{
		"can inline leaf",
		"inlining call to leaf",
		"coro: phase=provisional",
		"coro: func=p.caller effect=may-suspend local=nosuspend",
		"coro: phase=final",
		"coro: func=p.caller effect=may-suspend local=may-suspend recursive=false seeds=channel-send",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
}

func TestCrossPackageSummary(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		name = filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/coropoc\n\ngo 1.28\n")
	writeFile("leaf/leaf.go", `package leaf

var Ch chan int

func Suspend() {
	Ch <- 1
}

func Plain() int {
	return 1
}
`)
	writeFile("mid/mid.go", `package mid

import "example.com/coropoc/leaf"

func Suspend() {
	leaf.Suspend()
}

func Plain() int {
	return leaf.Plain()
}
`)
	writeFile("root/main.go", `package main

import "example.com/coropoc/mid"

func rootSuspend() {
	mid.Suspend()
}

func main() {
	ch := make(chan int, 1)
	ch <- 42
	if <-ch != 42 {
		panic("bad channel result")
	}
	println("coro-poc-ok")
}
`)

	exe := filepath.Join(tmp, "coropoc")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/coropoc/...=-l -d=coro=2", "./root")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("cross-package build failed: %v\n%s", err, out)
	}

	for _, want := range []string{
		"coro: edge=direct caller=example.com/coropoc/mid.Suspend callee=example.com/coropoc/leaf.Suspend unknown=false effect=may-suspend",
		"coro: edge=direct caller=example.com/coropoc/mid.Plain callee=example.com/coropoc/leaf.Plain unknown=false effect=nosuspend",
		"coro: func=example.com/coropoc/mid.Suspend effect=may-suspend",
		"coro: edge=direct caller=main.rootSuspend callee=example.com/coropoc/mid.Suspend unknown=false effect=may-suspend",
		"coro: func=main.rootSuspend effect=may-suspend",
		"coro: func=main.main effect=may-suspend",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}

	cmd = testenv.Command(t, exe)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("experiment executable failed: %v\n%s", err, data)
	} else if want := "coro-poc-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("executable output does not contain %q\n%s", want, data)
	}
}

func TestRejectMixedArchive(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	leafDir := filepath.Join(tmp, "leaf")
	midDir := filepath.Join(tmp, "mid")
	for _, dir := range []string{leafDir, midDir} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"),
		[]byte("module example.com/coropoc\n\ngo 1.28\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leafDir, "leaf.go"),
		[]byte("package leaf\n\nfunc F() {}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(midDir, "mid.go"),
		[]byte("package mid\n\nimport \"example.com/coropoc/leaf\"\n\nfunc F() { leaf.F() }\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(tmp, "leaf.a")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", archive, "./leaf")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=nocoro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building archive without summary failed: %v\n%s", err, out)
	}

	importcfg := filepath.Join(tmp, "importcfg")
	cfg := "packagefile example.com/coropoc/leaf=" + archive + "\n"
	if err := os.WriteFile(importcfg, []byte(cfg), 0o666); err != nil {
		t.Fatal(err)
	}
	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "compile",
		"-p=example.com/coropoc/mid",
		"-importcfg="+importcfg,
		"-o", filepath.Join(tmp, "mid.a"),
		filepath.Join(midDir, "mid.go"))
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("mixed archive compile unexpectedly succeeded\n%s", out)
	}
	for _, want := range []string{
		"could not import example.com/coropoc/leaf",
		"X:coro",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output does not contain %q\n%s", want, out)
		}
	}
}
