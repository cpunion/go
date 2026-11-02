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

func TestGuardedReceiveExpressions(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
var order int
//go:noinline
func take(v bool,tag int)bool{order=order*10+tag;runtime.Gosched();return v}
//go:noinline
func channel(c chan bool,tag int)chan bool{order=order*10+tag;runtime.Gosched();return c}
//go:noinline
func conjunction(v bool,c chan bool)bool{return v&&<-c}
//go:noinline
func disjunction(v bool,c chan bool)bool{return v||<-c}
//go:noinline
func mixed(v bool,c chan bool)bool{return take(v,1)&&(<-channel(c,2)||take(true,3))}
//go:noinline
func headers(c chan bool,p chan int)int{sum:=0;for i:=0;<-c;i=<-p{if i==1{continue};sum+=i};return sum}
//go:noinline
func pair(c chan bool)(bool,int){order=order*10+2;runtime.Gosched();return <-c,1}
//go:noinline
func consume(v bool,n int)bool{order=order*10+3;runtime.Gosched();return v&&n==1}
//go:noinline
func tuples(v bool,c chan bool)bool{return take(v,1)&&consume(pair(c))}
//go:noinline
func inline(v bool,c chan bool)bool{return v&&func()bool{if <-c{return true};return take(true,4)}()}
//go:noinline
func nilCall(v bool,c chan bool,isNil bool)(caught any){f:=take;if isNil{f=nil};defer func(){if p:=recover();p!=nil{caught=p}}();return v&&f(<-c,5)}
type node struct{value int;child *node}
//go:noinline
func held(cond chan bool,ptr chan *node,ready,release chan int)*node{var p *node;for <-cond{p=<-ptr;ready<-1;<-release};return p}
//go:noinline
func exited(cond chan bool,post chan int,done chan struct{}){defer func(){close(done)}();for ;<-cond;<-post{runtime.Goexit()};panic("continued after Goexit")}
func main(){
 if conjunction(false,nil)||!disjunction(true,nil){panic("skipped nil receive")};c:=make(chan bool,4);c<-true;if !conjunction(true,c)||len(c)!=0{panic("selected AND receive")};c<-false;if disjunction(false,c)||len(c)!=0{panic("selected OR receive")}
 order=0;if mixed(false,nil)||order!=1{panic("mixed skipped receive")};c<-true;order=0;if !mixed(true,c)||order!=12{panic("mixed selected OR")};c<-false;order=0;if !mixed(true,c)||order!=123{panic("mixed receive/await order")}
 cond:=make(chan bool,4);for _,v:=range []bool{true,true,true,false}{cond<-v};post:=make(chan int,3);for _,v:=range []int{1,2,3}{post<-v};if headers(cond,post)!=2||len(cond)!=0||len(post)!=0{panic("repeated receive condition/post")};close(cond);if headers(cond,nil)!=0{panic("closed condition executed body or post")}
 order=0;if tuples(false,nil)||order!=1{panic("tuple initializer crossed branch")};c<-true;order=0;if !tuples(true,c)||order!=123{panic("tuple receive/call order")}
 order=0;if inline(false,nil)||order!=0{panic("inline skipped receive")};c<-true;if !inline(true,c)||order!=0{panic("inline early return")};c<-false;if !inline(true,c)||order!=4{panic("inline selected await")}
 if nilCall(false,nil,true)!=false{panic("nil call selected skipped receive")};c<-true;if _,ok:=nilCall(true,c,true).(runtime.Error);!ok||len(c)!=0{panic("nil callee before argument receive")};c<-true;order=0;if nilCall(true,c,false)!=true||order!=5{panic("selected receive argument")}
 ptr:=make(chan *node,1);ptr<-&node{value:41,child:&node{value:42}};ready,release:=make(chan int),make(chan int);result:=make(chan *node,1);conditions:=make(chan bool,2);conditions<-true;conditions<-false;go func(){result<-held(conditions,ptr,ready,release)}();<-ready;runtime.GC();release<-1;p:=<-result;if p.value!=41||p.child.value!=42{panic("parked received pointer root")}
 exitCond:=make(chan bool,1);exitCond<-true;exitPost:=make(chan int,1);exitPost<-1;done:=make(chan struct{});go exited(exitCond,exitPost,done);<-done;if len(exitPost)!=1{panic("Goexit executed receive post")}
 runtime.LockOSThread();c<-true;if !conjunction(true,c){panic("ordinary guarded receive")};runtime.UnlockOSThread();println("guarded-receive-ok")
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
				for _, name := range []string{"conjunction", "disjunction", "mixed", "headers", "tuples", "inline", "nilCall", "held", "exited"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "guarded-receive-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
