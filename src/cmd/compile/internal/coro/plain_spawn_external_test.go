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

func TestPlainSpawn(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
		runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is unavailable")
	}
	files := map[string]string{
		"go.mod": "module example.com/plainspawn\n\ngo 1.28\n",
		"helper/helper.go": `package helper

type Box struct { Value int }

func Leaf(p *Box, out *int, done chan struct{}, values ...int) {
 *out = p.Value + values[0] + values[1]
 close(done)
}

func (b Box) Leaf(out *int, done chan struct{}) {
 *out = b.Value
 close(done)
}

func Spawn(p *Box, out *int, done chan struct{}, value int) {
 go Leaf(p, out, done, value, 3)
}

func SpawnMethod(b Box, out *int, done chan struct{}) {
 go b.Leaf(out, done)
}

// No local go statement demands a factory for this exported entry.
func NoAdapter(out *int, done chan struct{}) { *out = 29; close(done) }
`,
		"main.go": `package main

import (
 "example.com/plainspawn/helper"
 "os"
 "runtime"
)

func local(p *helper.Box, out *int, done chan struct{}) {
 *out = p.Value
 close(done)
}

func launch(p *helper.Box, out *int, done chan struct{}) { go local(p, out, done) }

func imported(p *helper.Box, out *int, done chan struct{}) {
 go helper.Leaf(p, out, done, 7, 3)
}

func ordinary(p *helper.Box, out *int, done chan struct{}) { helper.Leaf(p, out, done, 1, 2) }

func missing(out *int, done chan struct{}) { go helper.NoAdapter(out, done) }

func loop(out *int, done chan struct{}) {
 for i := 0; i < 10; i++ { *out += i }
 close(done)
}

func unbounded(out *int, done chan struct{}) { go loop(out, done) }

func main() {
 const count = 128
 var results [count]int
 var signals [count]chan struct{}
 for i := range count {
  signals[i] = make(chan struct{})
  p := &helper.Box{Value: i}
  switch i % 4 {
  case 0: launch(p, &results[i], signals[i])
  case 1: helper.Spawn(p, &results[i], signals[i], 7)
  case 2: helper.SpawnMethod(*p, &results[i], signals[i]); p.Value = -1
  case 3: imported(p, &results[i], signals[i])
  }
 }
 runtime.GC()
 for i := range count {
  <-signals[i]
  want := i
  if i%4 == 1 || i%4 == 3 { want += 10 }
  if results[i] != want { panic("plain task argument, receiver, or pointer root") }
 }
 out, done := 0, make(chan struct{})
 ordinary(&helper.Box{Value: 17}, &out, done)
 <-done
 if out != 20 { panic("ordinary entry changed") }
 out, done = 0, make(chan struct{})
 missing(&out, done)
 <-done
 if out != 29 { panic("missing adapter fallback") }
 out, done = 0, make(chan struct{})
 unbounded(&out, done)
 <-done
 if out != 45 { panic("loop target fallback") }
 if os.Getenv("PANIC") != "" {
  // Implicit nil dereference must terminate the child, not retry its body
  // indefinitely or become recoverable by the task creator.
  defer func() { if recover() != nil { println("parent-recovered-child") } }()
  launch(nil, new(int), make(chan struct{}))
  runtime.Gosched()
  select {}
 }
 println("plain-spawn-ok")
}
`,
	}
	for _, mode := range []string{"inline", "noinline", "checkptr", "missing-factory", "race", "nocoro"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "race" {
				testenv.MustHaveCGO(t)
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is unavailable")
				}
			}
			dir := t.TempDir()
			for name, contents := range files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
					t.Fatal(err)
				}
			}
			exe := filepath.Join(dir, "plain")
			flags, experiment := "-d=coro=4", "coro"
			if mode != "inline" {
				flags += " -l"
			}
			if mode == "nocoro" {
				flags, experiment = "-l", "nocoro"
			}
			args := []string{"build", "-o", exe, "-gcflags=example.com/plainspawn/...=" + flags}
			if mode == "checkptr" {
				args = append(args, "-gcflags=all=-d=checkptr=2",
					"-gcflags=example.com/plainspawn/...="+flags+" -d=checkptr=2")
			}
			if mode == "missing-factory" {
				args = append(args, "-gcflags=example.com/plainspawn/helper=-l -d=coro=1")
			}
			if mode == "race" {
				args = append(args, "-race")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, ".")...)
			build.Dir = dir
			build.Env = append(build.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, symbols)
			}
			native := mode == "inline" || mode == "noinline" || mode == "checkptr"
			for _, name := range []string{"main.launch", "main.local"} {
				if got := strings.Contains(string(symbols), name+".coro"); got != (mode != "race" && mode != "nocoro") {
					t.Errorf("%s native entry = %t", name, got)
				}
			}
			for _, name := range []string{"main.imported", "example.com/plainspawn/helper.Spawn", "example.com/plainspawn/helper.SpawnMethod", "example.com/plainspawn/helper.Leaf", "example.com/plainspawn/helper.Box.Leaf"} {
				if got := strings.Contains(string(symbols), name+".coro"); got != native {
					t.Errorf("%s native entry = %t, want %t", name, got, native)
				}
			}
			for _, name := range []string{"main.missing", "main.unbounded", "main.loop", "example.com/plainspawn/helper.NoAdapter"} {
				if strings.Contains(string(symbols), name+".coro") {
					t.Errorf("unsupported target lost ordinary fallback: %s", name)
				}
			}
			if native {
				for _, pattern := range []string{`^main\.launch\.coro(\.func[0-9]+)?$`, `^main\.imported\.coro(\.func[0-9]+)?$`} {
					cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", pattern, exe)
					data, err := cmd.CombinedOutput()
					if err != nil || !strings.Contains(string(data), "runtime.coroSpawn(SB)") || strings.Contains(string(data), "runtime.newproc(SB)") {
						t.Fatalf("plain target did not use native spawn: %v\n%s", err, data)
					}
				}
				cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", `^example.com/plainspawn/helper\.Leaf$`, exe)
				data, err := cmd.CombinedOutput()
				if err != nil || len(data) == 0 || strings.Contains(string(data), "runtime.coro") {
					t.Fatalf("ordinary leaf acquired scheduler work: %v\n%s", err, data)
				}
			}
			for _, procs := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+procs, "GOGC=1", "PANIC=")
				out, err := run.CombinedOutput()
				if err != nil || string(out) != "plain-spawn-ok\n" {
					t.Fatalf("GOMAXPROCS=%s: %v\n%s", procs, err, out)
				}
				panicRun := testenv.Command(t, exe)
				panicRun.Env = append(panicRun.Environ(), "GOMAXPROCS="+procs, "GOGC=1", "PANIC=1")
				out, err = panicRun.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "invalid memory address or nil pointer dereference") || strings.Contains(string(out), "parent-recovered-child") {
					t.Fatalf("child's implicit panic: %v\n%s", err, out)
				}
			}
		})
	}
}
