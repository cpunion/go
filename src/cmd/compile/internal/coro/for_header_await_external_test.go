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

func TestForHeaderAwaitExpressions(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
var trace string
var park,release chan int
//go:noinline
func parked(){if park!=nil{park<-1;<-release}}
//go:noinline
func start()int{trace+="I";runtime.Gosched();return 0}
//go:noinline
func check(i int)bool{trace+="C";runtime.Gosched();return i<3}
//go:noinline
func step(i int)int{trace+="P";runtime.Gosched();return i+1}
//go:noinline
func headers()int{sum:=0;for i:=start();check(i);i=step(i){trace+="B";if i==1{continue};sum+=i};return sum}
//go:noinline
func pair(i int)(int,bool){runtime.Gosched();return i,i<3}
//go:noinline
func choose(i int,ok bool)bool{runtime.Gosched();return ok}
//go:noinline
func tuples()int{sum:=0;for i:=0;choose(pair(i));i=step(i){sum+=i};return sum}
func apply(f func(int)bool,i int)bool{if i<0{return false};return f(i)}
//go:noinline
func inlineHeaders()int{f:=func(i int)bool{runtime.Gosched();return i<3};sum:=0;for i:=0;apply(f,i);i=step(i){sum+=i};return sum}
//go:noinline
func captures()([]func()int,[]*int){var fs []func()int;var ps []*int;for i:=0;check(i);i=step(i){fs=append(fs,func()int{return i});ps=append(ps,&i);parked();if i==1{continue};runtime.Gosched()};return fs,ps}
//go:noinline
func addresses()[]*int{var ps []*int;for i:=0;check(i);i=step(i){ps=append(ps,&i);runtime.Gosched();continue};return ps}
//go:noinline
func yieldOnlyAddresses()[]*int{var ps []*int;for i:=0;i<3;i++{ps=append(ps,&i);runtime.Gosched();continue};return ps}
//go:noinline
func existing()[]*int{var ps []*int;i:=0;for ;check(i);i=step(i){ps=append(ps,&i);runtime.Gosched();continue};return ps}
//go:noinline
func nested()int{sum:=0;for i:=0;check(i);i=step(i){for j:=0;j<3&&check(j);j=step(j){if j==1{continue};sum+=i+j};if i==1{continue}};return sum}
//go:noinline
func exit(mode int)(result int){defer func(){if recover()!=nil{result=9};trace+="D"}();for i:=0;check(i);i=step(i){trace+="B";switch mode{case 0:break;case 1:return 8;default:panic("stop")};break};return 7}
//go:noinline
func operand(i int)int{trace+="A";runtime.Gosched();return i}
//go:noinline
func nilPost(isNil bool)(caught any){f:=step;if isNil{f=nil};defer func(){caught=recover();trace+="D"}();for i:=0;check(i);i=f(operand(i)){trace+="B"};return nil}
//go:noinline
func nilCondition(isNil bool)(caught any){f:=check;if isNil{f=nil};defer func(){caught=recover();trace+="D"}();for i:=0;f(operand(i));i=step(i){trace+="B"};return nil}
//go:noinline
func sourceLabels()int{n:=0;again:for i:=0;check(i);i=step(i){n++;if n<2{continue again};goto done};done:return n}
func main(){
 trace="";if headers()!=2||trace!="ICBPCBPCBPC"{panic("init/condition/body/continue/post order: "+trace)}
 trace="";if tuples()!=3{panic("tuple header")};if inlineHeaders()!=3{panic("inline header")}
 var fs []func()int;var ps []*int;park=make(chan int);release=make(chan int);done:=make(chan struct{});go func(){fs,ps=captures();close(done)}();for range 3{<-park;runtime.GC();release<-1};<-done;park=nil;runtime.GC();if len(fs)!=3||len(ps)!=3{panic("capture count")};for i:=range 3{if fs[i]()!=i||*ps[i]!=i||i>0&&ps[i]==ps[i-1]{panic("iteration capture identity")};*ps[i]+=10;if fs[i]()!=i+10{panic("capture cell sharing")}}
 ps=addresses();runtime.GC();for i:=range 3{if *ps[i]!=i||i>0&&ps[i]==ps[i-1]{panic("escaped iteration address")}}
 ps=yieldOnlyAddresses();runtime.GC();for i:=range 3{if *ps[i]!=i||i>0&&ps[i]==ps[i-1]{panic("yield-only iteration address")}}
 ps=existing();runtime.GC();for i:=range 3{if *ps[i]!=3||ps[i]!=ps[0]{panic("existing variable identity")}}
 if nested()!=12{panic("nested condition/post")};for mode,want:=range []int{7,8,9}{trace="";if exit(mode)!=want||trace!="CBD"{panic("exit executed post or lost cleanup: "+trace)}}
 trace="";if _,ok:=nilPost(true).(runtime.Error);!ok||trace!="CBAD"{panic("nil post operand/cleanup: "+trace)};trace="";if _,ok:=nilCondition(true).(runtime.Error);!ok||trace!="AD"{panic("nil condition operand/cleanup: "+trace)}
 if sourceLabels()!=2{panic("source labels fallback")};runtime.LockOSThread();trace="";if headers()!=2||trace!="ICBPCBPCBPC"{panic("ordinary thread-affine headers")};runtime.UnlockOSThread();println("for-header-await-ok")
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
				for _, name := range []string{"headers", "tuples", "inlineHeaders", "captures", "addresses", "yieldOnlyAddresses", "existing", "nested", "exit", "nilPost", "nilCondition"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "for-header-await-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
