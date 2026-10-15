// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMethodValueBoundary(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	for name, contents := range map[string]string{
		"go.mod": "module example.com/coromethodvalue\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf

import "runtime"

type Counter int

func (counter *Counter) Add(delta int) int {
	runtime.Gosched()
	*counter += Counter(delta)
	return int(*counter)
}

type Value int

func (value Value) Add(delta int) int {
	runtime.Gosched()
	return int(value) + delta
}

var Bound = new(Counter).Add

func Bind(counter *Counter) func(int) int {
	return counter.Add
}
`,
		"main.go": `package main

import (
	"example.com/coromethodvalue/leaf"
	"runtime"
)

type Local int

func (local *Local) Add(delta int) int {
	runtime.Gosched()
	*local += Local(delta)
	return int(*local)
}

var bound = new(Local).Add

func main() {
	local := Local(3)
	localValue := local.Add
	if localValue(2) != 5 || local.Add(1) != 6 || bound(7) != 7 {
		panic("local receiver changed")
	}
	counter := leaf.Counter(10)
	importedValue := counter.Add
	if importedValue(2) != 12 || leaf.Bind(&counter)(3) != 15 || leaf.Bound(4) != 4 {
		panic("imported receiver changed")
	}
	value := leaf.Value(20)
	valueMethod := value.Add
	value = 100
	if valueMethod(2) != 22 || value.Add(1) != 101 {
		panic("value receiver was not captured")
	}
	expression := (*leaf.Counter).Add
	if expression(&counter, 1) != 16 {
		panic("method expression changed")
	}
	println("method-values-ok")
}
`,
	} {
		path := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	for _, experiment := range []string{"coro", "nocoro"} {
		for _, noinline := range []bool{false, true} {
			name := experiment + "/inline"
			flags := ""
			if noinline {
				name = experiment + "/noinline"
				flags = "-l "
			}
			if experiment == "coro" {
				flags += "-d=coro=4"
			}
			t.Run(name, func(t *testing.T) {
				exe := filepath.Join(t.TempDir(), "methodvalue.exe")
				args := []string{"build", "-o", exe}
				if flags != "" {
					args = append(args, "-gcflags=example.com/coromethodvalue/...="+flags)
				}
				args = append(args, ".")
				cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
				cmd.Dir = tmp
				cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
				data, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("build failed: %v\n%s", err, data)
				}
				if experiment == "coro" && noinline {
					for _, wrapper := range []string{
						"main.(*Local).Add-fm",
						"example.com/coromethodvalue/leaf.(*Counter).Add-fm",
					} {
						if !strings.Contains(string(data), "coro: skip "+wrapper+": function requires closure context") {
							t.Errorf("missing ordinary-entry fallback for %s\n%s", wrapper, data)
						}
					}
				}
				cmd = testenv.Command(t, exe)
				data, err = cmd.CombinedOutput()
				if err != nil || strings.TrimSpace(string(data)) != "method-values-ok" {
					t.Fatalf("execution = (%v, %q), want method-values-ok", err, data)
				}
				cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
				data, err = cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("nm failed: %v\n%s", err, data)
				}
				if strings.Contains(string(data), ".Add-fm.coro") {
					t.Fatal("method value wrapper has a coroutine factory")
				}
				if experiment == "coro" && noinline {
					for _, method := range []string{
						"main.(*Local).Add.coro",
						"example.com/coromethodvalue/leaf.(*Counter).Add.coro",
					} {
						if !strings.Contains(string(data), method) {
							t.Errorf("ordinary method lost its coroutine factory: %s", method)
						}
					}
				}
			})
		}
	}
}
