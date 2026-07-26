// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProgram = `package p

import (
	"runtime"
	"time"
)

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

func yielding() {
	runtime.Gosched()
}

func yieldCaller() {
	yielding()
}

func sleeping() {
	time.Sleep(0)
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

	importcfg := filepath.Join(tmp, "importcfg")
	list := testenv.Command(t, testenv.GoToolPath(t), "list", "-export",
		"-f=packagefile {{.ImportPath}}={{.Export}}", "runtime", "time")
	list.Env = append(list.Environ(),
		"GOEXPERIMENT="+experiment,
		"GOCACHE="+filepath.Join(tmp, "gocache"),
	)
	data, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("listing imports failed: %v\n%s", err, data)
	}
	if err := os.WriteFile(importcfg, data, 0o666); err != nil {
		t.Fatal(err)
	}

	args := []string{"tool", "compile", "-l", "-p=p", "-importcfg=" + importcfg,
		"-o", filepath.Join(tmp, "p.o")}
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
		"coro: func=p.yielding effect=may-suspend local=may-suspend recursive=false seeds=scheduler-yield primary=coro",
		"coro: func=p.yieldCaller effect=may-suspend local=nosuspend recursive=false seeds=- primary=coro",
		"coro: func=p.sleeping effect=may-suspend local=may-suspend recursive=false seeds=timer-wait primary=coro",
		"coro: site=1 func=p.yielding kind=yield foreign=-",
		"coro: site=1 func=p.yieldCaller kind=await foreign=-",
		"coro: site=1 func=p.sleeping kind=timer foreign=-",
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
