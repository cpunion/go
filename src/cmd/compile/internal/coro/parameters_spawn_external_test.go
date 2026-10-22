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

func TestClosedParameterSpawn(t *testing.T) {
	const source = `package main
import (
 "runtime"
 "runtime/debug"
)
type box struct {n int}
func first(p *box,n int,ready chan int,gate chan struct{},done chan int) {ready<-n;<-gate;runtime.Gosched();done<-p.n+n}
func second(p *box,n int,ready chan int,gate chan struct{},done chan int) {ready<-n;<-gate;runtime.Gosched();done<-p.n+n+1}
//go:noinline
func apply(f func(*box,int,chan int,chan struct{},chan int),p *box,n int,ready chan int,gate chan struct{},done chan int) int {f(p,n,ready,gate,done);return n}
var arguments int
func argument() int {arguments++;return arguments}
func spawn(ready chan int,gate chan struct{},done chan int) {
 for i:=range 128 {
  f:=first;if i%2!=0 {f=second}
  go apply(f,&box{i},argument(),ready,gate,done)
  f=nil
 }
}
//go:noinline
func nested(f func(*box,int,chan int,chan struct{},chan int),depth int,ready chan int,gate chan struct{},done chan int) {if depth!=0 {go nested(f,depth-1,ready,gate,done);return};f(&box{7},9,ready,gate,done)}
func nesting(ready chan int,gate chan struct{},done chan int) {go nested(second,16,ready,gate,done)}
func sum(xs ...int) int {runtime.Gosched();n:=0;for i:=range len(xs) {n+=xs[i]};return n}
//go:noinline
func variadic(f func(...int)int,done chan int,xs ...int) {n:=f(xs...);done<-n}
func variable(done chan int) {go variadic(sum,done,2,3)}
func main(){
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 baseline:=runtime.NumGoroutine();spawn(ready,gate,done)
 total:=0;for range 128 {total+=<-ready};if total!=8256 || arguments!=128 {panic("go parameter argument evaluation")}
 if native && runtime.NumGoroutine()>baseline+12 {panic("go parameter used ordinary goroutines")}
 runtime.GC();close(gate);total=0;for range 128 {total+=<-done};if total!=16448 {panic("go parameter capture or GC")}
 nesting(ready,gate,done);if <-ready!=9 || <-done!=17 {panic("nested parameter spawn")}
 variable(done);if <-done!=5 {panic("variadic parameter spawn")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"spawn", "nesting", "variable"})
}

func TestClosedParameterSpawnTerminal(t *testing.T) {
	const source = `package main
import "runtime"
func first(n int) int {runtime.Gosched();return n+10}
func panicking(n int) int {runtime.Gosched();panic(n)}
func exiting(n int) int {runtime.Gosched();runtime.Goexit();return n}
type outcome struct {value int;panicked any;finished bool}
//go:noinline
func apply(f func(int)int,n int,result *outcome,done chan struct{}) {
 defer func(){result.panicked=recover();result.finished=true;close(done)}()
 v:=f(n);result.value=v
}
func spawn(which int,result *outcome,done chan struct{}) {f:=first;switch which {case 1:f=panicking;case 2:f=exiting;case 3:f=nil};go apply(f,7,result,done)}
var side int
func badArgument() int {side++;panic(41)}
func creator(result *outcome,done chan struct{}) (observed int) {defer func(){if x:=recover();x!=nil {observed=x.(int)}}();go apply(first,badArgument(),result,done);return 0}
var exitDone chan struct{}
func exitArgument() int {runtime.Goexit();return 0}
func creatorExit(result *outcome,done chan struct{}) {defer func(){close(exitDone)}();go apply(first,exitArgument(),result,done);panic("continued after creator Goexit")}
func main(){
 for which:=range 4 {
  result,done:=new(outcome),make(chan struct{});spawn(which,result,done);<-done
  if !result.finished {panic("child defer did not finish")}
  switch which {
  case 0:if result.value!=17 || result.panicked!=nil {panic("normal callback")}
  case 1:if result.panicked!=7 {panic("child panic")}
  case 2:if result.value!=0 || result.panicked!=nil {panic("child Goexit")}
  case 3:if _,ok:=result.panicked.(runtime.Error);!ok {panic("child nil callback")}
  }
 }
 result,done:=new(outcome),make(chan struct{})
 if creator(result,done)!=41 || side!=1 {panic("creator panic ownership")}
 exitDone=make(chan struct{});go creatorExit(result,done);<-exitDone
 if result.finished {panic("child started after argument failure")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"spawn", "creator", "creatorExit"})
}

func TestClosedParameterSpawnBoundaries(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/coroparamspawn\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(n int) int {runtime.Gosched();return n+40}
//go:noinline
func Open(f func(int)int,n int,done chan int) {v:=f(n);done<-v}
`,
		"main.go": `package main
import (
 "runtime"
 "example.com/coroparamspawn/leaf"
)
func first(n int) int {runtime.Gosched();return n+10}
func outside(n int) int {runtime.Gosched();return n+90}
//go:noinline
func apply(f func(int)int,n int,done chan int) {v:=f(n);done<-v}
func imported(which bool,done chan int) {f:=first;if which {f=leaf.First};go apply(f,3,done)}
func open(f func(int)int,done chan int) {go apply(f,4,done)}
var global=outside
func globalSpawn(done chan int) {go apply(global,5,done)}
func importedOpen(done chan int) {go leaf.Open(first,6,done)}
func rejected(done chan int) {go apply(first,7,done);goto end;end:runtime.Gosched()}
//go:noinline
func inspection(f func(),done chan int) {f();done<-1}
func inspect() {pc,_,_,ok:=runtime.Caller(1);if !ok || runtime.FuncForPC(pc).Name()!="main.inspection" {panic("ordinary child source frame changed")}}
func rejectedInspection(done chan int) {go inspection(inspect,done);goto end;end:runtime.Gosched()}
func deferred(done chan int) {defer apply(first,8,done);runtime.Gosched()}
func main(){
 done:=make(chan int,8)
 imported(false,done);if <-done!=13 {panic("local callback")}
 imported(true,done);if <-done!=43 {panic("imported callback")}
 open(outside,done);if <-done!=94 {panic("open parameter fallback")}
 globalSpawn(done);if <-done!=95 {panic("global parameter fallback")}
 importedOpen(done);if <-done!=16 {panic("imported callee fallback")}
 rejected(done);if <-done!=17 {panic("rejected caller fallback")}
 rejectedInspection(done);if <-done!=1 {panic("ordinary child fallback")}
 deferred(done);if <-done!=18 {panic("deferred source fallback")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/coroparamspawn/...=",
		[]switchFallback{{"apply", "dynamic await site"}, {"rejected", "control flow goto"}, {"rejectedInspection", "control flow goto"}},
		[]string{"imported"})
}
func TestClosedParameterSpawnContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package paramspawncontext
import (
 "runtime"
 "testing"
 "testing/synctest"
 "time"
)
var value int
func slow() {time.Sleep(time.Hour);value=1}
func fast() {runtime.Gosched();value=2}
//go:noinline
func apply(f func(),done chan struct{}) {defer func(){close(done)}();f()}
func spawnSlow(done chan struct{}) {go apply(slow,done)}
func spawnFast(done chan struct{}) {go apply(fast,done)}
func TestBubble(t *testing.T) {
 for range 1 {
  synctest.Test(t,func(t *testing.T){
   start:=time.Now()
   done:=make(chan struct{})
   spawnSlow(done);<-done
   if value!=1 || time.Since(start)!=time.Hour {t.Fatal("callback lost ordinary synctest accounting")}
   synctest.Wait()
  })
 }
}
func TestThread(t *testing.T) {
 for range 1 {
  runtime.LockOSThread()
  defer runtime.UnlockOSThread()
  done:=make(chan struct{})
  spawnFast(done);<-done
  if value!=2 {t.Fatal("thread-bound callback fallback")}
 }
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":          "module example.com/paramspawncontext\n\ngo 1.28\n",
		"context_test.go": source,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"inline", "noinline", "race", "nocoro"} {
		t.Run(mode, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "contexts.exe")
			args := []string{"test", "-c", "-o", exe}
			experiment := "coro"
			flags := "-d=coro=4"
			if mode == "noinline" {
				flags = "-l " + flags
			}
			if mode == "race" {
				testenv.MustHaveCGO(t)
				args = append(args, "-race")
			}
			if mode == "nocoro" {
				experiment, flags = "nocoro", "-l"
			}
			args = append(args, "-gcflags=example.com/paramspawncontext="+flags)
			cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
			cmd.Dir = dir
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			cmd = testenv.Command(t, exe, "-test.timeout=10s")
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("contexts: %v\n%s", err, data)
			}
			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			data, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"spawnFast", "spawnSlow"} {
				present := strings.Contains(string(data), " T example.com/paramspawncontext."+name+".coro\n")
				if present != (mode == "inline" || mode == "noinline") {
					t.Fatalf("%s: native factory present=%v", name, present)
				}
			}
		})
	}
}

func TestClosedParameterSpawnFatal(t *testing.T) {
	const source = `package main
import (
 "context"
 "os"
 "os/exec"
 "runtime"
 "strings"
 "time"
)
func first(n int) int {runtime.Gosched();return n}
func failing(n int) int {runtime.Gosched();panic("parameter-child-panic")}
//go:noinline
func apply(f func(int)int,n int) {f(n)}
func argument() int {println("argument-once");return 1}
func spawn(which bool,nilTarget bool) {f:=first;if which {f=failing};if nilTarget {f=nil};go apply(f,argument())}
func main(){
 if len(os.Args)>1 {
  defer func(){if recover()!=nil {println("parent-recovered-child")}}()
  spawn(os.Args[1]=="panic",os.Args[1]=="nil")
  select {}
 }
 exe,err:=os.Executable();if err!=nil {panic(err)}
 for _,mode:=range []string{"nil","panic"} {
  ctx,cancel:=context.WithTimeout(context.Background(),2*time.Second)
  data,err:=exec.CommandContext(ctx,exe,mode).CombinedOutput()
  expired:=ctx.Err()!=nil;cancel();out:=string(data)
  want:="invalid memory address or nil pointer dereference";if mode=="panic" {want="parameter-child-panic"}
  if expired || err==nil || !strings.Contains(out,want) || strings.Contains(out,"parent-recovered-child") {panic("child failure ownership: "+out)}
  count:=0;for _,line:=range strings.Split(out,"\n") {if line=="argument-once" {count++}};if count!=1 {panic("child argument evaluation: "+out)}
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"spawn"})
}

func TestClosedParameterSpawnMissingFactory(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/paramspawnmissing\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(n int) int {runtime.Gosched();return n+10}
func Plain(n int) int {return n+20}
`,
		"main.go": `package main
import "example.com/paramspawnmissing/leaf"
//go:noinline
func apply(f func(int)int,n int,done chan int) {v:=f(n);done<-v}
func spawn(which bool,done chan int) {f:=leaf.First;if which {f=leaf.Plain};go apply(f,3,done)}
func main(){done:=make(chan int,1);spawn(false,done);if <-done!=13 {panic("available imported factory")};spawn(true,done);if <-done!=23 {panic("missing imported factory fallback")};println("switch-return-ok")}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/paramspawnmissing/...=", []switchFallback{{"spawn", "unsupported coroutine dependency main.apply.param1"}}, nil)
}
