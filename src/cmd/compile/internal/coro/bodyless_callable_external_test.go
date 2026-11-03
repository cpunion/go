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

// A declaration's serialized body index does not grant permission to clone
// an implementation. The linked Go implementation must still sort and call
// its comparator after coroutine callback-parameter analysis.
func TestBodylessCallableDeclaration(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	const source = `package main

import (
	"runtime"
	"sort"
	_ "unsafe"
)

// The local declaration has a serialized body index but no Go definition.
// Its real implementation and callback must survive source specialization.
//go:linkname localSort sort.Slice
func localSort(any, func(int, int) bool)

var values = []int{9, 1, 7, 3}
var calls int

func less(i, j int) bool { calls++; return values[i] < values[j] }
func owner() { localSort(values, less); runtime.Gosched() }

func main() {
	owner()
	if calls == 0 || !sort.IntsAreSorted(values) { panic("bodyless declaration lost its implementation or callback") }
	println("bodyless-callable-ok")
}
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(path, []byte(source), 0666); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, flags, link, extra, experiment string }{
		{"ordinary", "-l", "", "", "nocoro"},
		{"native", "-l -d=coro=4", "", "", "coro"},
		{"inline", "-d=coro=4", "", "", "coro"},
		{"checkptr", "-l -d=checkptr=2 -d=coro=4", "", "", "coro"},
		{"checkptr-inline", "-d=checkptr=2 -d=coro=4", "", "", "coro"},
		{"no-dwarf", "-d=coro=4", " -w", "", "coro"},
		{"pie", "-l -d=coro=4", "", "-buildmode=pie", "coro"},
		{"race", "-d=coro=4", "", "-race", "coro"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(tmp, config.name+".exe")
			args := []string{"build", "-gcflags=" + config.flags, "-ldflags=-checklinkname=0" + config.link, "-o", exe}
			if config.extra != "" {
				args = append(args, config.extra)
			}
			args = append(args, path)
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				if data, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(data)) != "bodyless-callable-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
