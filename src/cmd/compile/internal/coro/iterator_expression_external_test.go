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

func TestIteratorProducerExpressions(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
var constructed int
//go:noinline
func makeIterator(seed int)func(func(int)bool){constructed++;return func(yield func(int)bool){for range 3{seed++;if !yield(seed){return}}}}
//go:noinline
func readonly(seed int)func(func(int)bool){return func(yield func(int)bool){for i:=range 3{if !yield(seed+i){return}}}}
//go:noinline
func first()int{n:=0;for v:=range makeIterator(10){n+=v};return n}
//go:noinline
func readOnly()int{n:=0;for v:=range readonly(10){n+=v};return n}
//go:noinline
func repeated()int{f:=makeIterator(10);n:=0;for v:=range f{n+=v};for v:=range f{n+=v};return n}
//go:noinline
func distinct()int{f,g:=makeIterator(1),makeIterator(100);n:=0;for v:=range f{n+=v};for v:=range g{n+=v};for v:=range f{n+=v};return n}
//go:noinline
func literal(seed int)int{n:=0;for v:=range func(yield func(int)bool){for i:=range 3{if !yield(seed+i){return}}}{n+=v};return n}
//go:noinline
func invoke(f func(int)int,x int)int{return f(x)}
//go:noinline
func makeCaller(seed int)func(func(int)int,int)int{return func(f func(int)int,x int)int{seed++;return f(seed+x)}}
//go:noinline
func direct()int{g:=func(x int)int{runtime.Gosched();return x+1};a:=makeCaller(10)(g,0);b:=inlineCaller(11)(g,0);return a+b}
func inlineCaller(seed int)func(func(int)int,int)int{return func(f func(int)int,x int)int{seed++;if seed<0{return f(-seed+x)};return f(seed+x)}}
var operands int
func argument()int{events=events*10+3;operands++;runtime.Gosched();return 2}
//go:noinline
func maybeCaller(isNil bool)func(func(int)int,int)int{events=events*10+1;if isNil{return nil};return makeCaller(10)}
var events int
func selected()func(int)int{events=events*10+2;return func(v int)int{return v+1}}
//go:noinline
func nilCall(isNil bool)(caught any){defer func(){if v:=recover();v!=nil{caught=v}}();return maybeCaller(isNil)(selected(),argument())}
type object struct{n int;pad[2048]byte}
//go:noinline
func pointerIterator(p *object)func(func(*object)bool){return func(yield func(*object)bool){for range 3{p.n++;if !yield(p){return}}}}
//go:noinline
func held(p *object,ready,release chan int)int{n:=0;for v:=range pointerIterator(p){ready<-v.n;<-release;if v.pad[2047]!=7{panic("captured iterator pointer")};n+=v.n};return n}
//go:noinline
func conditional(flag bool)bool{g:=func(v int)int{runtime.Gosched();return v+1};return flag && inlineCaller(11)(g,0)==13}
func main(){
 if conditional(false)||!conditional(true){panic("conditional inline await")};if first()!=36||constructed!=1{panic("iterator constructor evaluated twice")};if readOnly()!=33||repeated()!=81||distinct()!=333||literal(10)!=33||direct()!=25{panic("captured callee environment")}
 if nilCall(false)!=14||operands!=1||events!=123{panic("captured caller argument")};events=0;if _,ok:=nilCall(true).(runtime.Error);!ok||operands!=2||events!=123{panic("nil captured callee operand order")}
 ready,release:=make(chan int),make(chan int);go func(){for i:=1;i<=3;i++{if <-ready!=i{panic("captured iterator GC input")};runtime.GC();release<-1}}();p:=&object{};p.pad[2047]=7;if held(p,ready,release)!=6{panic("captured iterator GC result")}
 runtime.LockOSThread();if first()!=36||direct()!=25{panic("captured caller ordinary fallback")};runtime.UnlockOSThread();println("iterator-expressions-ok")
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"native-inline", "coro", "-d=coro=4"},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2"},
		{"native-no-dwarf", "coro", "-d=coro=4 -dwarf=false -gendwarfinl=0"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "program")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", binary, path)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"first", "readOnly", "repeated", "distinct", "literal", "direct", "nilCall", "held"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "iterator-expressions-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
