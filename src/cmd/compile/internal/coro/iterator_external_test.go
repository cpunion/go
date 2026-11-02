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

func TestClosedNamedIteratorRange(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import("runtime";"strings")
//go:noinline
func named(yield func(int)bool){for i:=range 4 {if !yield(i){return}}}
//go:noinline
func sum()int{n:=0;for v:=range named {n+=v};return n}
//go:noinline
func early()int{n:=0;for v:=range named{if v==2{break};n+=v};return n}
//go:noinline
func returned()int{for v:=range named{if v==2{return v}};return -1}
//go:noinline
func continued()int{n:=0;for v:=range named{if v==1{continue};n+=v};return n}
//go:noinline
func nested()int{n:=0;for i:=range named{for j:=range named{runtime.Gosched();n+=i+j}};return n}
//go:noinline
func pairs(yield func(int,string)bool){for i:=range 3{if !yield(i,"xy"){return}}}
//go:noinline
func paired()int{n:=0;for i,s:=range pairs{n+=i+len(s)};return n}
//go:noinline
func zero(yield func()bool){for range 3{if !yield(){return}}}
//go:noinline
func noValues()int{n:=0;for range zero{n++};return n}
//go:noinline
func parked(ready,release chan int)int{n:=0;for v:=range named{ready<-v;<-release;n+=v};return n}
type object struct{n int;pad [2048]byte}
//go:noinline
func pointerIterator(yield func(*object)bool){for i:=range 4{p:=&object{n:i};p.pad[2047]=byte(i+7);if !yield(p){return}}}
//go:noinline
func pointerSum(ready,release chan int)int{n:=0;for p:=range pointerIterator{ready<-p.n;<-release;if p.pad[2047]!=byte(p.n+7){panic("iterator GC pointer")};n+=p.n};return n}
//go:noinline
func ignoresFalse(yield func(int)bool){yield(0);yield(1)}
//go:noinline
func bad()(caught any){defer func(){caught=recover()}();for range ignoresFalse{break};return nil}
//go:noinline
func swallows(yield func(int)bool){defer func(){recover()}();yield(0)}
//go:noinline
func missing()(caught any){defer func(){caught=recover()}();for range swallows{panic("loop body")};return nil}
var saved func(int)bool
//go:noinline
func stash(yield func(int)bool){saved=yield;yield(0)}
//go:noinline
func escaped()int{n:=0;for v:=range stash{n+=v+1};return n}
func later()(caught any){defer func(){caught=recover()}();saved(1);return nil}
var defers int
//go:noinline
func rangeDefer()(during int){for v:=range named{defer func(x int){defers=10*defers+x+1}(v);runtime.Gosched();during=defers};return}
//go:noinline
func makeIterator(seed int)func(func(int)bool){return func(yield func(int)bool){for i:=range 3{runtime.Gosched();if !yield(seed+i){return}}}}
//go:noinline
func captured()int{n:=0;for v:=range makeIterator(10){runtime.Gosched();n+=v};return n}
func message(v any)string{if v==nil{return ""};return v.(error).Error()}
func main(){
 if sum()!=6||early()!=1||returned()!=2||continued()!=5||nested()!=48||paired()!=9||noValues()!=3{panic("named iterator results")}
 ready,release:=make(chan int),make(chan int);go func(){for pass:=0;pass<2;pass++{for i:=range 4{if <-ready!=i{panic("iterator GC input")};runtime.GC();release<-1}}}();if parked(ready,release)!=6||pointerSum(ready,release)!=6{panic("parked iterator result")}
 if !strings.Contains(message(bad()),"continued iteration after function for loop body returned false"){panic("invalid iterator stop protocol")}
 if !strings.Contains(message(missing()),"recovered a loop body panic and did not resume panicking"){panic("iterator missing panic protocol")}
 if escaped()!=1||!strings.Contains(message(later()),"continued iteration after whole loop exit"){panic("escaped iterator yield")}
 if rangeDefer()!=0||defers!=4321{panic("range defer enclosing activation")};if captured()!=33{panic("captured iterator fallback")};println("named-iterator-ok")
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
				nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", binary)
				symbols, err := nm.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, symbols)
				}
				for _, line := range strings.Split(string(symbols), "\n") {
					fields := strings.Fields(line)
					if len(fields) == 0 {
						continue
					}
					name := fields[len(fields)-1]
					if strings.HasPrefix(name, "main.rangeDefer-range") && strings.HasSuffix(name, ".coro.stackgate") {
						t.Fatalf("enclosing range defer acquired a callback-owned native frame: %s", name)
					}
				}
				for _, name := range []string{"sum", "early", "returned", "continued", "nested", "paired", "noValues", "parked", "pointerSum", "bad", "missing", "escaped"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "named-iterator-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
