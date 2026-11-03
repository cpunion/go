// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const sourceFrameProgram = `package main
import ("runtime";"path/filepath")
type frame struct{name string;line int}
var expected []frame
//go:noinline
func inspect(){
 var pcs[64]uintptr
 count:=runtime.Callers(1,pcs[:])
 frames:=runtime.CallersFrames(pcs[:count])
 for _,want:=range expected {
  got,_:=frames.Next()
  if got.Function!=want.name||filepath.Base(got.File)!="main.go"||want.line!=0&&got.Line!=want.line {
   println("Callers",got.Function,got.File,got.Line,"want",want.name,want.line);panic("source Callers frame")
  }
 }
 for skip,want:=range expected {
  pc,file,line,ok:=runtime.Caller(skip)
  if !ok||runtime.FuncForPC(pc).Name()!=want.name||filepath.Base(file)!="main.go"||want.line!=0&&line!=want.line {
   println("Caller",skip,runtime.FuncForPC(pc).Name(),file,line,"want",want.name,want.line);panic("source Caller frame")
  }
 }
}
//go:noinline
func root(f func()){
 runtime.Gosched()
 f() // root-call
}
//go:noinline
func inner(f func()){
 runtime.Gosched()
 f() // inner-call
}
//go:noinline
func outer(f func()){
 runtime.Gosched()
 inner(f) // outer-call
}
//go:noinline
func recursive(n int,f func()){
 runtime.Gosched()
 if n>0 {
  recursive(n-1,f) // recursive-call
  return
 }
 f() // recursive-leaf
}
//go:noinline
func left(n int,f func()){
 runtime.Gosched()
 if n>0 {
  right(n-1,f) // left-call
  return
 }
 f() // left-leaf
}
//go:noinline
func right(n int,f func()){
 runtime.Gosched()
 if n>0 {
  left(n-1,f) // right-call
  return
 }
 f() // right-leaf
}
var rootEntry=root
var outerEntry=outer
var recursiveEntry=recursive
var leftEntry=left
func main(){
 if pc,_,_,ok:=runtime.Caller(0);!ok||pc==0{panic("ordinary root")}
 expected=[]frame{{"main.inspect",0},{"main.root",ROOT_LINE},{"main.main",0}}
 rootEntry(inspect)
 expected=[]frame{{"main.inspect",0},{"main.inner",INNER_LINE},{"main.outer",OUTER_LINE},{"main.main",0}}
 outerEntry(inspect)
 expected=[]frame{{"main.inspect",0},{"main.recursive",RECURSIVE_LEAF_LINE}}
 for i:=0;i<8;i++{expected=append(expected,frame{"main.recursive",RECURSIVE_CALL_LINE})}
 expected=append(expected,frame{"main.main",0})
 recursiveEntry(8,inspect)
 expected=[]frame{{"main.inspect",0},{"main.left",LEFT_LEAF_LINE},{"main.right",RIGHT_CALL_LINE},{"main.left",LEFT_CALL_LINE},{"main.right",RIGHT_CALL_LINE},{"main.left",LEFT_CALL_LINE},{"main.main",0}}
 leftEntry(4,inspect)
 println("source-frames-ok")
}
`

func sourceFrameFixture() string {
	source := sourceFrameProgram
	for marker, token := range map[string]string{
		"root-call": "ROOT_LINE", "inner-call": "INNER_LINE", "outer-call": "OUTER_LINE",
		"recursive-call": "RECURSIVE_CALL_LINE", "recursive-leaf": "RECURSIVE_LEAF_LINE",
		"left-call": "LEFT_CALL_LINE", "left-leaf": "LEFT_LEAF_LINE", "right-call": "RIGHT_CALL_LINE",
	} {
		for i, line := range strings.Split(sourceFrameProgram, "\n") {
			if strings.Contains(line, "// "+marker) {
				source = strings.ReplaceAll(source, token, strconv.Itoa(i+1))
				break
			}
		}
	}
	return source
}

func TestSourceFramesAcrossOrdinaryHost(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(sourceFrameFixture()), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2", ""},
		{"no-dwarf", "coro", "-d=coro=4", "-ldflags=-w"},
		{"pie", "coro", "-l -d=coro=4", "-buildmode=pie"},
		{"ordinary", "nocoro", "-l", ""},
		{"ordinary-inline", "nocoro", "", ""},
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
			if config.experiment == "coro" {
				for _, name := range []string{"root", "inner", "outer", "recursive", "left", "right"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Errorf("source frame lost native admission: %s", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				if data, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(data)) != "source-frames-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
