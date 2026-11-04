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

func TestTerminalSourceCallers(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("runtime";"fmt";"path/filepath";"os")
//go:noinline
func recursive(n int){runtime.Gosched();if n>0{recursive(n-1);return};panic("original")}
var entry=recursive
//go:noinline
func inspect(skip,size int){goto body;body:pcs:=make([]uintptr,size);n:=runtime.Callers(skip,pcs);fmt.Printf("query %d %d %d\n",skip,size,n);if n==0{return};frames:=runtime.CallersFrames(pcs[:n]);for{f,more:=frames.Next();fmt.Printf("%s %s:%d\n",f.Function,filepath.Base(f.File),f.Line);if !more{break}}}
func main(){goto body;body:n:=16;if os.Args[1]=="deep"{n=220};defer func(){for _,skip:=range []int{0,1,2,3,4,49,50,100,219,220,222,224,225,226,227,228,1000}{for _,size:=range []int{0,1,2,17,50,100,512}{inspect(skip,size)}};if recover()!="original"{panic("missing original")}}();entry(n)}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	want := make(map[string]string)
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"ordinary", "nocoro", "-l", ""},
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"no-dwarf", "coro", "-d=coro=4", "-ldflags=-w"},
		{"pie", "coro", "-l -d=coro=4", "-buildmode=pie"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(dir, config.name)
			args := []string{"build", "-gcflags=" + config.flags, "-o", exe}
			if config.extra != "" {
				args = append(args, config.extra)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" && linkedCoroStackUnsafe(t, exe, "main.recursive.coro.stackgate") {
				t.Fatal("terminal PC source owner lost native admission")
			}
			for _, depth := range []string{"shallow", "deep"} {
				for _, p := range []string{"1", "4"} {
					run := testenv.Command(t, exe, depth)
					run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0")
					data, err := run.CombinedOutput()
					text := string(data)
					if err != nil || strings.Contains(text, ".coro.") || strings.Contains(text, "runtime.coroPanic") {
						t.Fatalf("%s P%s: %v\n%s", depth, p, err, text)
					}
					if config.name == "ordinary" && want[depth] == "" {
						want[depth] = text
					}
					if text != want[depth] {
						t.Fatalf("%s P%s: terminal PC oracle differs\ngot:\n%s\nwant:\n%s", depth, p, text, want[depth])
					}
				}
			}
		})
	}
}
