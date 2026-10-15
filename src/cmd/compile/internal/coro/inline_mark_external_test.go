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

func TestInlineMarks(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"

func read() int { runtime.Gosched(); return 7 }
func identity(x int) int { return x }
func equal(x, y int) bool { if x == y { return true }; return false }

func closureCheck(want int) bool {
	identity := func(x int) int { return x }
	if got, expected := identity(read()), want; !equal(got, expected) { return false }
	return true
}
func explicitCheck(want int) bool {
	if got, expected := identity(read()), want; !equal(got, expected) { return false }
	return true
}
func main() {
	if !closureCheck(7) || closureCheck(8) || !explicitCheck(7) || explicitCheck(8) { panic("wrong result") }
	println("inline-marks-ok")
}
`
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, experiment, flags string }{
		{"coro/inline", "coro", "-d=coro=4 -m=2"},
		{"coro/noinline", "coro", "-l -d=coro=4"},
		{"nocoro/inline", "nocoro", "-m=2"},
		{"nocoro/noinline", "nocoro", "-l"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "inlinemarks.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+test.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			if test.experiment == "coro" {
				for _, name := range []string{"read", "closureCheck", "explicitCheck"} {
					if !strings.Contains(string(out), "func="+name+".coro,") {
						t.Errorf("missing lowered %s:\n%s", name, out)
					}
				}
			}
			if strings.HasSuffix(test.name, "/inline") {
				for _, name := range []string{"identity", "closureCheck.func1", "equal"} {
					if !strings.Contains(string(out), "inlining call to "+name) {
						t.Errorf("regression helper %s was not inlined:\n%s", name, out)
					}
				}
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOGC=1")
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "inline-marks-ok" {
				t.Fatalf("execution = (%v, %q), want inline-marks-ok", err, out)
			}
		})
	}
}
