// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"fmt"
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlainRecursiveFactory(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/plainrecursion\n\ngo 1.28\n",
		"helper/helper.go": `package helper

func Sum(n int) int {
	if n == 0 {
		return 0
	}
	child := Sum(n - 1)
	return n + child
}

func Even(n int) bool {
	if n == 0 {
		return true
	}
	return Odd(n - 1)
}

func Odd(n int) bool {
	if n == 0 {
		return false
	}
	return Even(n - 1)
}

type Counter struct {
	Value int
}

func (c *Counter) Descend(n int) {
	if n == 0 {
		return
	}
	c.Value++
	c.Descend(n - 1)
}

func Deferred(n int) (result int) {
	defer func() { result++ }()
	if n == 0 {
		return
	}
	result = Deferred(n - 1)
	return
}

func Wrap(n int) int {
	return Sum(n)
}

func MayPanic(n int) int {
	if n < 0 {
		panic("negative")
	}
	if n == 0 {
		return 1
	}
	return MayPanic(n-1) + 1
}
`,
		"main.go": `package main

import (
	"example.com/plainrecursion/helper"
	"runtime"
)

func local(n int) int {
	if n == 0 {
		return 1
	}
	child := local(n - 1)
	return child + 1
}

func check(n int) int {
	runtime.Gosched()
	got := local(n)
	if got != n+1 {
		panic("local recursion")
	}
	got = helper.Wrap(n)
	if got != n*(n+1)/2 {
		panic("imported recursion")
	}
	even := helper.Even(n)
	odd := helper.Odd(n)
	if !even || odd {
		panic("mutual recursion")
	}
	var c helper.Counter
	c.Descend(n)
	if c.Value != n {
		panic("recursive method")
	}
	got = helper.Deferred(n)
	if got != n+1 {
		panic("recursive defer")
	}
	got = helper.MayPanic(n)
	if got != n+1 {
		panic("panic-only recursion")
	}
	return got
}

func worker(done chan int) {
	result := check(4096)
	done <- result
}

func main() {
	done := make(chan int)
	go worker(done)
	got := <-done
	if got != 4097 {
		panic("spawned recursion")
	}
	println("plain-recursive-factory-ok")
}
`,
	}
	for name, contents := range files {
		path := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"inline", "noinline", "checkptr", "race", "nocoro"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "race" {
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is not supported")
				}
				testenv.MustHaveCGO(t)
			}
			exe := filepath.Join(tmp, "recursive-"+mode)
			flags := "-d=coro=4"
			if mode == "noinline" {
				flags += " -l"
			}
			if mode == "nocoro" {
				flags = ""
			}
			args := []string{"build", "-o", exe}
			if mode == "checkptr" {
				args = append(args, "-gcflags=all=-d=checkptr=2")
				flags += " -d=checkptr=2"
			}
			args = append(args, "-gcflags=example.com/plainrecursion/...="+flags)
			if mode == "race" {
				args = append(args, "-race")
			}
			args = append(args, ".")
			cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
			cmd.Dir = tmp
			experiment := "coro"
			if mode == "nocoro" {
				experiment = "nocoro"
			}
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			native := mode != "race" && mode != "nocoro"
			if native {
				for _, name := range []string{
					"helper.Sum", "helper.Even", "helper.Odd", "helper.(*Counter).Descend", "helper.Deferred", "helper.Wrap", "helper.MayPanic", "main.local",
				} {
					if strings.HasPrefix(name, "helper.") {
						name = "example.com/plainrecursion/" + name
					}
					found := false
					for _, line := range strings.Split(string(out), "\n") {
						if strings.Contains(line, "coro: func="+name+" ") &&
							strings.Contains(line, "effect=nosuspend ") &&
							strings.Contains(line, "primary=coro exec=preempt ") {
							found = true
						}
					}
					if !found {
						t.Errorf("missing nonsuspending recursive entry %s\n%s", name, out)
					}
				}
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm failed: %v\n%s", err, symbols)
			}
			for _, name := range []string{
				"helper.Sum", "helper.Even", "helper.Odd", "helper.(*Counter).Descend", "helper.Deferred", "helper.Wrap",
				"helper.MayPanic", "main.local", "main.check", "main.worker", "main.main",
			} {
				if got := strings.Contains(string(symbols), name+".coro"); got != native {
					t.Errorf("%s native factory = %t, want %t", name, got, native)
				}
			}
			for _, procs := range []string{"1", "4"} {
				cmd := testenv.Command(t, exe)
				cmd.Env = append(cmd.Environ(), "GOMAXPROCS="+procs, "GODEBUG=asyncpreemptoff=1")
				out, err := cmd.CombinedOutput()
				if err != nil || string(out) != "plain-recursive-factory-ok\n" {
					t.Fatalf("GOMAXPROCS=%s: run failed: %v\n%s", procs, err, out)
				}
			}
		})
	}
}

func TestRecursiveImportedFallback(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	for _, panicking := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%t", panicking), func(t *testing.T) {
			tmp := t.TempDir()
			check := ""
			if panicking {
				check = `if n < 0 { panic("negative") }`
			}
			files := map[string]string{
				"go.mod": "module example.com/recursivefallback\n\ngo 1.28\n",
				"helper/helper.go": `package helper
func Sum(n int) int {
` + check + `
	if n == 0 { return 1 }
	return Sum(n-1) + 1
}
`,
				"main.go": `package main
import (
	"example.com/recursivefallback/helper"
	"runtime"
)
func colored(n int) int {
	runtime.Gosched()
	return helper.Sum(n)
}
func main() {
	if colored(4096) != 4097 { panic("recursive fallback") }
	println("recursive-imported-fallback-ok")
}
`,
			}
			for name, contents := range files {
				path := filepath.Join(tmp, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
					t.Fatal(err)
				}
			}
			exe := filepath.Join(tmp, "recursive")
			// The helper publishes effects but no native factory. A panic-only
			// imported summary must not permit unbounded ordinary recursion.
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe,
				"-gcflags=example.com/recursivefallback/helper=-l -d=coro=1",
				"-gcflags=example.com/recursivefallback=-l -d=coro=4", ".")
			cmd.Dir = tmp
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "unsupported coroutine dependency") {
				t.Fatalf("missing dependency fallback: %v\n%s", err, out)
			}
			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			out, err = cmd.CombinedOutput()
			if err != nil || strings.Contains(string(out), "main.colored.coro") {
				t.Fatalf("unsupported caller has a native entry: %v\n%s", err, out)
			}
			for _, procs := range []string{"1", "4"} {
				cmd = testenv.Command(t, exe)
				cmd.Env = append(cmd.Environ(), "GOMAXPROCS="+procs, "GODEBUG=asyncpreemptoff=1")
				out, err = cmd.CombinedOutput()
				if err != nil || string(out) != "recursive-imported-fallback-ok\n" {
					t.Fatalf("GOMAXPROCS=%s: run failed: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
