// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSourceIdleTerminalStacks(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("fmt";"os";"runtime";"strconv";"strings";"time")
var opaque func()
func hold(){goto body;body:runtime.Gosched()}
//go:noinline
func recursive(n,mode int){runtime.Gosched();if n>0{recursive(n-1,mode);return};opaque();if mode==1{panic("pending")};if mode==2{runtime.Goexit()}}
//go:noinline
func cleanup(mode int,ready,gate,done chan struct{}){
 ready<-struct{}{}
 <-gate // PARK_FIRST
 value:=recover()
 if mode==1&&value!="pending"{panic("missing recovery")}
 if mode!=1&&value!=nil{panic("unexpected recovery")}
 ready<-struct{}{}
 <-gate // PARK_SECOND
 close(done)
}
//go:noinline
func child(depth,mode int,ready,gate,done chan struct{}){defer cleanup(mode,ready,gate,done);recursive(depth,mode)}
//go:noinline
func launch(depth,mode int,ready,gate,done chan struct{}){go child(depth,mode,ready,gate,done)}
var entry=launch
var rootEntry=child
//go:noinline
func publicChild(depth,mode int,ready,gate,done chan struct{}){goto body;body:rootEntry(depth,mode,ready,gate,done)}
func main(){goto body;body:
 depth,err:=strconv.Atoi(os.Args[1]);if err!=nil{panic(err)};mode,err:=strconv.Atoi(os.Args[2]);if err!=nil{panic(err)};kind,err:=strconv.Atoi(os.Args[3]);if err!=nil{panic(err)};file:=os.Args[4]
 content,err:=os.ReadFile(file);if err!=nil{panic(err)};lines:=[2]int{};for i,line:=range strings.Split(string(content),"\n"){for stage,marker:=range []string{"// PARK_FIRST","// PARK_SECOND"}{if strings.HasSuffix(strings.TrimSpace(line),marker){lines[stage]=i+1}}};if lines[0]==0||lines[1]==0{panic("missing source markers")}
 opaque=hold;baseline:=runtime.NumGoroutine();ready:=make(chan struct{});gate:=make(chan struct{});done:=make(chan struct{});leafOnly:=os.Getenv("CORO_PROFILE_LEAF_ONLY")=="1"
 if kind==0{entry(depth,mode,ready,gate,done)}else{go publicChild(depth,mode,ready,gate,done)}
 for stage:=0;stage<2;stage++{
  <-ready;for i:=0;i<100;i++{runtime.Gosched()}
  for iteration:=0;iteration<3;iteration++{
   runtime.GC();buf:=make([]byte,1<<20);n:=runtime.Stack(buf,true);found:=false
   for _,stack:=range strings.Split(string(buf[:n]),"\n\n"){
    if !strings.Contains(stack,"main.cleanup("){continue};if found{panic("duplicate cleanup stack")};found=true
    want:=0;if mode!=0{want=depth+1;if depth>100{want=97-kind}}
    if strings.Count(stack,"main.recursive(")!=want||strings.Count(stack,"main.child(")!=1||strings.Count(stack,"main.publicChild(")!=kind{panic(fmt.Sprintf("terminal source chain depth %d mode %d kind %d stage %d want %d recursive frames\n%s",depth,mode,kind,stage,want,stack))}
    if (strings.Count(stack,"panic(")==1)!=(mode==1)||(strings.Count(stack,"runtime.Goexit(")==1)!=(mode==2){panic("terminal boundary missing or duplicated\n"+stack)}
    for _,name:=range []string{".coro.","runtime.coroRun(","runtime.coroRunFrame(","runtime.(*stacklessCoroHost).drive(","main.hold(","main.launch("}{if strings.Contains(stack,name){panic("private or completed frame\n"+stack)}}
    textLines:=strings.Split(stack,"\n");for i,line:=range textLines{if strings.HasPrefix(line,"main.cleanup("){position,ok:=strings.CutPrefix(strings.TrimSpace(textLines[i+1]),file+":");if !ok{panic("cleanup source file mismatch")};line,err:=strconv.Atoi(strings.Fields(position)[0]);if err!=nil||line!=lines[stage]{panic(fmt.Sprintf("cleanup source line %d want %d stage %d",line,lines[stage],stage))}}}
   }
   if !found{panic("missing parked cleanup source stack")}
   records:=make([]runtime.StackRecord,baseline+8);n,ok:=runtime.GoroutineProfile(records);if !ok||n!=baseline+1{panic("profile count mismatch")};hosts:=0
   for _,record:=range records[:n]{cleanupFrames,recursiveFrames,childFrames,publicFrames,panics,exits:=0,0,0,0,0,0;private:=false;frames:=runtime.CallersFrames(record.Stack());for{f,more:=frames.Next();switch f.Function{case "main.cleanup":cleanupFrames++;if f.File!=file||f.Line!=lines[stage]{panic(fmt.Sprintf("profile cleanup location %s:%d want %s:%d",f.File,f.Line,file,lines[stage]))};case "main.recursive":recursiveFrames++;case "main.child":childFrames++;case "main.publicChild":publicFrames++;case "runtime.gopanic":panics++;case "runtime.Goexit":exits++};if strings.Contains(f.Function,".coro.")||f.Function=="runtime.coroRun"||f.Function=="runtime.coroRunFrame"||f.Function=="runtime.(*stacklessCoroHost).drive"{private=true};if !more{break}};if cleanupFrames==0{continue};hosts++;if private||cleanupFrames!=1{panic("private or duplicate cleanup profile frame")};if leafOnly{if len(record.Stack())!=1{panic("one-PC profile capacity")};continue};if (panics==1)!=(mode==1)||(exits==1)!=(mode==2){panic("profile terminal boundary missing")};if mode==0{if recursiveFrames!=0{panic("completed recursion in profile")}}else if depth<100{if recursiveFrames!=depth+1{panic("profile lost shallow incoming source chain")}}else if recursiveFrames==0{panic("profile lost deep incoming source chain")};if depth<100&&(childFrames!=1||publicFrames!=kind){panic("profile root suffix mismatch")}}
   if hosts!=1{panic("profile missing cleanup source leaf")}
  }
  gate<-struct{}{}
 }
 <-done;deadline:=time.Now().Add(10*time.Second);for runtime.NumGoroutine()!=baseline{if time.Now().After(deadline){panic("terminal cleanup retained host count")};runtime.Gosched()}
 fmt.Println("idle-terminal-source-ok",depth,mode,kind)
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"ordinary", "nocoro", "-l", ""},
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"ordinary-race", "nocoro", "-l", "-race"},
		{"coro-race", "coro", "-l -d=coro=4", "-race"},
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
			native := config.experiment == "coro" && config.extra == ""
			if native {
				for _, owner := range []string{"recursive", "cleanup", "child", "launch"} {
					if linkedCoroStackUnsafe(t, exe, "main."+owner+".coro.stackgate") {
						t.Fatalf("%s lost native admission", owner)
					}
				}
			}
			runCase := func(depth, mode, kind, p string, leafOnly bool) {
				t.Helper()
				run := testenv.Command(t, exe, depth, mode, kind, file)
				debug, leaf := "asyncpreemptoff=0", ""
				if leafOnly {
					debug += ",profstackdepth=1"
					leaf = "1"
				}
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG="+debug, "CORO_PROFILE_LEAF_ONLY="+leaf)
				data, err := run.CombinedOutput()
				want := "idle-terminal-source-ok " + depth + " " + mode + " " + kind + "\n"
				if err != nil || string(data) != want {
					t.Fatalf("depth %s mode %s kind %s P%s leaf-only %v: %v\n%s", depth, mode, kind, p, leafOnly, err, data)
				}
			}
			for _, depth := range []string{"16", "220"} {
				for _, mode := range []string{"0", "1", "2"} {
					for _, kind := range []string{"0", "1"} {
						for _, p := range []string{"1", "4"} {
							runCase(depth, mode, kind, p, false)
						}
					}
				}
			}
			if native {
				for _, mode := range []string{"1", "2"} {
					for _, kind := range []string{"0", "1"} {
						for _, p := range []string{"1", "4"} {
							runCase("220", mode, kind, p, true)
						}
					}
				}
			}
		})
	}
}
