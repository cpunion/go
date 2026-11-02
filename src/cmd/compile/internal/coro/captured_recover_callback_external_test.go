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

func TestCapturedRecoverCallbacks(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
type box struct{n int;seen any;pad [2048]byte}
//go:noinline
func makeRecover(p *box,delta *int)func(int)(any,int){return func(n int)(any,int){p.n+=*delta*n+int(p.pad[2047]);p.seen=recover();return p.seen,p.n}}
//go:noinline
func otherRecover(p *box)func(int)(any,int){return func(n int)(any,int){p.n+=3*n;p.seen=recover();return p.seen,p.n}}
//go:noinline
func invoke(cb func(int)(any,int),n int)(any,int){return cb(n)}
//go:noinline
func applyRecover(cb func(int)(any,int),n int,out *any,got *int){v,x:=cb(n);if v!=nil{panic("normal callback stole panic")};*got=x;if v:=recover();v!=nil{*out=v}}
//go:noinline
func makeOwner(out *any,got *int)func(func(int)(any,int),int){return func(cb func(int)(any,int),n int){v,x:=cb(n);if v!=nil{panic("captured normal callback stole panic")};*got=x;if v:=recover();v!=nil{*out=v}}}
//go:noinline
func regular()(any,int){p:=&box{n:1};d:=2;return invoke(makeRecover(p,&d),3)}
//go:noinline
func deferred()(out any,n int){p:=&box{n:1};d:=2;cb:=makeRecover(p,&d);f:=makeOwner(&out,&n);defer f(cb,3);cb=nil;f=nil;d=4;runtime.Gosched();panic("owner")}
//go:noinline
func public(p *box,d *int){cb:=makeRecover(p,d);defer cb(3);cb=nil;runtime.Gosched();panic("public")}
//go:noinline
func combined(p *box,d *int){cb:=makeRecover(p,d);v,n:=invoke(cb,2);if v!=nil||n!=5{panic("normal public entry")};defer cb(3);runtime.Gosched();panic("combined")}
//go:noinline
func union(other bool)(out any,n int){p:=&box{n:1};d:=2;cb:=makeRecover(p,&d);if other{cb=otherRecover(p)};defer applyRecover(cb,3,&out,&n);cb=nil;runtime.Gosched();panic("union")}
//go:noinline
func nilCallback(useNil bool)(out any){defer func(){out=recover()}();p:=&box{};d:=1;cb:=makeRecover(p,&d);if useNil{cb=nil};var result any;var got int;defer applyRecover(cb,2,&result,&got);runtime.Gosched();return}
//go:noinline
func parked(p *box,d *int,ready chan struct{},gate chan struct{},done chan int)(out any,n int){defer func(){if out!="parked"{panic("parked owner recovery")};done<-n}();cb:=makeRecover(p,d);defer applyRecover(cb,3,&out,&n);cb=nil;p=nil;d=nil;ready<-struct{}{};<-gate;panic("parked")}
//go:noinline
func startParked(seed int,ready chan struct{},gate chan struct{},done chan int){p:=&box{n:10};p.pad[2047]=byte(seed);d:=2;go parked(p,&d,ready,gate,done);p=nil}
//go:noinline
func three(y func(int)bool){for i:=1;i<=3;i++{if !y(i){return}}}
//go:noinline
func rangeOwner()(out any,n int){p:=&box{n:1};p.pad[2047]=1;for v:=range three{d:=v;cb:=makeRecover(p,&d);defer applyRecover(cb,v,&out,&n);cb=nil};runtime.Gosched();panic("range")}
var saved func(int)bool
var callbackReady,callbackRelease chan struct{}
//go:noinline
func stash(y func(int)bool){saved=y;close(callbackReady);<-callbackRelease}
//go:noinline
func escapedOwner()(out any,n int){p:=&box{n:1};p.pad[2047]=1;for v:=range stash{runtime.LockOSThread();d:=v;cb:=makeRecover(p,&d);defer applyRecover(cb,v,&out,&n);cb=nil;runtime.UnlockOSThread()};panic("escape")}
func mixed(locked bool){callbackReady=make(chan struct{});callbackRelease=make(chan struct{});done:=make(chan int,1);go func(){if locked{runtime.LockOSThread();defer runtime.UnlockOSThread()};out,n:=escapedOwner();if out!="escape"{panic("escaped recover owner")};done<-n}();<-callbackReady;if !locked{runtime.LockOSThread()};saved(1);saved(2);if !locked{runtime.UnlockOSThread()};runtime.GC();close(callbackRelease);if <-done!=8{panic("escaped capture environment")}}
var replace func(*box)
//go:noinline
func shared(p *box,d *int)func(int)(any,int){replace=func(next *box){p=next};return func(n int)(any,int){p.n+=*d*n;p.seen=recover();return p.seen,p.n}}
//go:noinline
func sharedOwner(ready,gate chan struct{},done chan int)(out any,n int){defer func(){if out!="shared"{panic("shared recovery")};done<-n}();p:=&box{n:1};d:=2;cb:=shared(p,&d);defer applyRecover(cb,3,&out,&n);cb=nil;p=nil;ready<-struct{}{};<-gate;panic("shared")}
//go:noinline
func variadicMaker(p *box)func(...int)(any,int){return func(xs ...int)(any,int){p.n+=xs[0]+xs[1];p.seen=recover();return p.seen,p.n}}
//go:noinline
func applyVariadic(cb func(...int)(any,int),out *any,n *int,xs ...int){v,x:=cb(xs...);if v!=nil{panic("variadic indirect recover")};*n=x;*out=recover()}
//go:noinline
func variadicOwner()(out any,n int){p:=&box{};cb:=variadicMaker(p);xs:=[]int{2,3};defer applyVariadic(cb,&out,&n,xs...);cb=nil;xs[0]=7;runtime.Gosched();panic("variadic")}
//go:noinline
func boundsMaker(p *box)func(int)(any,int){return func(n int)(any,int){p.seen=recover();return p.seen,int(p.pad[n])}}
//go:noinline
func replacement()(out any){defer func(){out=recover()}();cb:=boundsMaker(&box{});var ignored any;var n int;defer applyRecover(cb,4096,&ignored,&n);runtime.Gosched();panic("replaced")}
func controls(){if v,n:=regular();v!=nil||n!=7{panic("regular capture")};if v,n:=deferred();v!="owner"||n!=13{panic("callee callback snapshot")};p:=&box{n:1};d:=2;public(p,&d);if p.seen!="public"||p.n!=7{panic("public captured defer")};p=&box{n:1};combined(p,&d);if p.seen!="combined"||p.n!=11{panic("dual entry")};if v,n:=union(false);v!="union"||n!=7{panic("captured union")};if v,n:=union(true);v!="union"||n!=10{panic("other captured union")};if nilCallback(false)!=nil{panic("non-nil captured callback")};if _,ok:=nilCallback(true).(runtime.Error);!ok{panic("nil callback invocation")};if v,n:=rangeOwner();v!="range"||n!=18{panic("range capture cleanup")};if v,n:=variadicOwner();v!="variadic"||n!=10{panic("variadic snapshot")};if _,ok:=replacement().(runtime.Error);!ok{panic("normal callback panic replacement")}}
func main(){controls();runtime.LockOSThread();controls();runtime.UnlockOSThread();ready:=make(chan struct{},32);gate:=make(chan struct{});done:=make(chan int,32);for i:=0;i<32;i++{startParked(i,ready,gate,done)};for i:=0;i<32;i++{<-ready};runtime.GC();close(gate);sum:=0;for i:=0;i<32;i++{sum+=<-done};if sum!=1008{panic("parked capture roots")};r:=make(chan struct{});g:=make(chan struct{});d:=make(chan int,1);go sharedOwner(r,g,d);<-r;runtime.LockOSThread();replace(&box{n:20});runtime.UnlockOSThread();runtime.GC();close(g);if <-d!=26{panic("shared environment cell")};mixed(false);mixed(true);println("captured-recover-callbacks-ok")}
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
				for _, name := range []string{"regular", "deferred", "public", "combined", "union", "nilCallback", "parked", "startParked", "rangeOwner", "escapedOwner", "sharedOwner", "variadicOwner", "replacement"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
							t.Fatal("captured recover callback lost native admission")
						}
					})
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "captured-recover-callbacks-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
