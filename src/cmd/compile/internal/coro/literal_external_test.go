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
	"strconv"
	"strings"
	"testing"
)

func TestCaptureFreeLiteralEntries(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" || runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("native executor unavailable")
	}
	const source = `package main
import "runtime"
func direct(c chan int) int { result:=func(in chan int) int {value:=<-in;return value}(c);return result }
func local(c chan int) int { f:=func(in chan int) int {value:=<-in;return value+1};g:=f;return g(c) }
func nested(c chan int) int {result:=func(in chan int) int {f:=func(next chan int) int{value:=<-next;return value+2};return f(in)}(c);return result}
func spawn(in,out chan int) {for range 64 {go func(src,dst chan int){value:=<-src;dst<-value}(in,out)}}
func closeDone(out chan struct{}) {close(out)}
func exitLiteral(done chan struct{}) {func(out chan struct{}) {defer closeDone(out);runtime.Goexit()}(done)}
func capturing(c chan int) int {f:=func()int{return <-c};return f()}
func main() {
 c:=make(chan int,4);c<-10;c<-20;c<-30;c<-40
 if direct(c)!=10 || local(c)!=21 || nested(c)!=32 || capturing(c)!=40 {panic("wrong literal result")}
 in,out:=make(chan int,64),make(chan int)
 before:=runtime.NumGoroutine()
 spawn(in,out)
 if NATIVE && runtime.NumGoroutine()>before+16 {panic("literal children used ordinary goroutines")}
 for i:=range 64 {in<-i}
 runtime.GC()
 total:=0;for range 64 {total+=<-out}
 if total!=2016 {panic("lost literal child")}
 done:=make(chan struct{});go exitLiteral(done);<-done
 println("literal-entries-ok")
}
`
	for _, config := range []struct {
		name, experiment, flags string
		race                    bool
	}{
		{"inline", "coro", "-d=coro=4", false},
		{"noinline", "coro", "-l -d=coro=4", false},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", false},
		{"ordinary", "nocoro", "-l", false},
		{"race", "coro", "-d=coro=4", true},
	} {
		t.Run(config.name, func(t *testing.T) {
			if config.race {
				testenv.MustHaveCGO(t)
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race unavailable")
				}
			}
			native := config.experiment == "coro" && !config.race
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/literals\n\ngo 1.28\n"), 0666); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(strings.ReplaceAll(source, "NATIVE", strconv.FormatBool(native))), 0666); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "program")
			args := []string{"build", "-gcflags=" + config.flags, "-o", binary}
			if config.race {
				args = append(args, "-race")
			}
			args = append(args, ".")
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Dir = dir
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			data, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", binary)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"direct", "local", "nested", "spawn", "exitLiteral"} {
				if got := strings.Contains(string(symbols), "main."+name+".coro"); got != native {
					t.Fatalf("%s native=%t want=%t\n%s", name, got, native, data)
				}
			}
			if strings.Contains(string(symbols), "main.capturing.coro") {
				t.Fatal("captured environment lost its fallback")
			}
			if native && config.name != "inline" && !strings.Contains(string(symbols), "main.direct.func1.coro") {
				t.Fatalf("literal factory missing\n%s", data)
			}
			for _, procs := range []int{1, 4} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs), "GOGC=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "literal-entries-ok" {
					t.Fatalf("P%d: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
