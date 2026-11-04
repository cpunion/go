// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeFallbackInitializers(t *testing.T) {
	testNormalizeInitializers(t, true)
}

func TestNormalizeOpenInitializers(t *testing.T) {
	testNormalizeInitializers(t, false)
}

func testNormalizeInitializers(t *testing.T, fallback bool) {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	program := `package main
import "runtime"

type named interface { Name() string }
type item string
func (n item) Name() string { return string(n) }
var trace string
func read() named { runtime.Gosched(); trace += "r"; return item("ok") }
func suffix() string { trace += "s"; return "!" }

func check() {
	names := func(n named) string { trace += "n"; return n.Name() }
	if got, want := names(read()), "ok"; got != want { panic(got) }
	if trace != "rn" { panic(trace) }
	trace = ""
	got := names(read()) + suffix()
	if got != "ok!" || trace != "rns" { panic(got + trace) }
	trace = ""
	if _, ignored := names(read()), 1; ignored != 1 { panic(ignored) }
	if trace != "rn" { panic(trace) }
}
func main() { check(); println("normalize-fallback-ok") }
`
	if fallback {
		// Interface initializers now have native continuations. Retain the
		// ordinary-body regression with an explicitly unsupported control
		// flow boundary, without changing any operand or initializer.
		program = strings.Replace(program, "func check() {", "func check() {\n goto ordinary\n ordinary:", 1)
	}
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, experiment, flags string }{
		{"coro/inline", "coro", "-d=coro=4"},
		{"coro/noinline", "coro", "-l -d=coro=4"},
		{"coro/checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"nocoro/inline", "nocoro", ""},
		{"nocoro/noinline", "nocoro", "-l"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "normalize.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+test.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			if test.experiment == "coro" {
				if fallback {
					if !strings.Contains(string(out), "coro: skip main.check: control flow goto") {
						t.Fatalf("regression function did not exercise native fallback:\n%s", out)
					}
				} else if linkedCoroStackUnsafe(t, exe, "main.check.coro.stackgate") {
					t.Fatal("interface initializer owner lost native admission")
				}
			}
			if test.experiment == "coro" && !strings.Contains(string(out), "func=read.coro,") {
				t.Fatalf("yielding callee did not get a coroutine factory:\n%s", out)
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOGC=1")
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "normalize-fallback-ok" {
				t.Fatalf("execution = (%v, %q), want normalize-fallback-ok", err, out)
			}
		})
	}
}
