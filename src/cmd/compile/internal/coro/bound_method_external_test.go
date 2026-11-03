// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestOpenBoundMethods(t *testing.T) {
	const source = `package main
import "runtime"
type box struct{n int;caught any}
func(p *box)add(n int)(int,*box){runtime.Gosched();runtime.GC();p.n+=n;return p.n,p}
func(p *box)store(n int,done chan struct{}){runtime.Gosched();runtime.GC();p.n=n;close(done)}
func(p *box)capture(){runtime.Gosched();runtime.GC();p.caught=recover()}
func(p *box)exit(done chan struct{}){defer close(done);runtime.Gosched();runtime.Goexit();p.n=999}
func(p *box)nilSafe()int{runtime.Gosched();if p==nil{return 71};return p.n}
type value struct{pad byte;n int;p *box}
func(v value)sum(n int)int{runtime.Gosched();runtime.GC();return int(v.pad)+v.n+v.p.n+n}
type large struct{data [2048]byte}
func(v large)sum(n int)int{runtime.Gosched();return int(v.data[0])+int(v.data[2047])+n}
func call(f func(int)(int,*box),n int)(int,*box){runtime.Gosched();return f(n)}
func launch(f func(int,chan struct{}),n int,done chan struct{}){runtime.Gosched();go f(n,done)}
func cleanup(f func(),token any){defer f();runtime.Gosched();panic(token)}
func launchExit(f func(chan struct{}),done chan struct{}){runtime.Gosched();go f(done)}
func valueCall(f func(int)int,n int)int{runtime.Gosched();return f(n)}
func nilCall(f func()int)int{runtime.Gosched();return f()}
func main(){
 p:=&box{n:11};f:=p.add
 n,q:=call(f,3);if n!=14||q!=p{panic("bound tuple result")}
 v:=value{pad:2,n:3,p:p};vf:=v.sum;v=value{}
 if valueCall(vf,5)!=24{panic("bound receiver snapshot")}
 done:=make(chan struct{});launch(p.store,23,done);<-done
 if p.n!=23{panic("bound go")}
 token:=new(int);cleanup(p.capture,token)
 if p.caught!=token{panic("bound defer direct recover")}
 p.capture();if p.caught!=nil{panic("normal bound call borrowed recover")}
 var absent *box
 if nilCall(absent.nilSafe)!=71{panic("typed nil receiver")}
 largeValue:=large{};largeValue.data[0]=13;largeValue.data[2047]=17;lf:=largeValue.sum;largeValue=large{}
 if valueCall(lf,19)!=49{panic("large bound adapter fallback")}
 done=make(chan struct{});launchExit(p.exit,done);<-done
 if p.n!=23{panic("bound Goexit returned")}
 println("switch-return-ok")
}
`
	required := []string{"call", "launch", "cleanup", "launchExit", "valueCall", "nilCall"}
	testSwitchReturnProgram(t, source, nil, required, required...)
}
