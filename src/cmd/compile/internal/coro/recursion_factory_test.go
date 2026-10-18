// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
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
					"helper.Sum", "helper.Even", "helper.Odd", "helper.(*Counter).Descend", "helper.Deferred", "helper.Wrap", "main.local",
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
				"main.local", "main.check", "main.worker", "main.main",
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
