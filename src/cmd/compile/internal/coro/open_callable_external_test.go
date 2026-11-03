// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These source calls have no closed target proof. A descriptor can select a
// native child; plain or missing descriptors must execute the same operands
// once through the ordinary host. The fixture uses no private runtime ABI.
func TestOpenFunctionCalls(t *testing.T) {
	const source = `package main
import "runtime"
type callback func(int)int
type box struct{n int;pad[4096]byte}
var calls int
func named(n int)int{runtime.Gosched();runtime.GC();return n+3}
func plain(n int)int{return n+5}
func captured(p *box)callback{return func(n int)int{runtime.Gosched();runtime.GC();return n+p.n+int(p.pad[4095])}}
func(p *box)add(n int)int{runtime.Gosched();p.n+=n;return p.n}
func tuple(p *box,n int)(int,*box){runtime.Gosched();runtime.GC();return p.n+n,p}
func variadic(n int,values ...int)int{for _,value:=range values{runtime.Gosched();n+=value};return n}
func store(p *box,n int){runtime.Gosched();runtime.GC();p.n=n}
func pair(n int)(int,int){runtime.Gosched();return n+10,1}

func owner(f callback,n int)int{calls++;value:=f(n);runtime.Gosched();calls++;return value+1}
func methodOwner(f func(*box,int)int,p *box,n int)int{value:=f(p,n);runtime.Gosched();return value}
func tupleOwner(f func(*box,int)(int,*box),p *box,n int)(value int,out *box){value,out=f(p,n);runtime.Gosched();return}
func variadicOwner(f func(int,...int)int)int{value:=f(2,3,5,7);runtime.Gosched();return value}
func emptyOwner(f func(*box,int),p *box,n int){f(p,n);runtime.Gosched()}
func nilOwner(f callback,arg func()int)(caught any){defer func(){caught=recover()}();f(arg());runtime.Gosched();return}
func snapshotOwner(produce func()callback,arg func()int)int{value:=produce()(arg());runtime.Gosched();return value}
func assignmentOwner(f func(int)(int,int))([2]int,int){values:=[2]int{};index:=0;values[index],index=f(7);runtime.Gosched();return values,index}
func plainArgumentOwner()int{value:=chosen(plainArgument());runtime.Gosched();return value}
func knownSnapshotOwner()int{value:=produce()(argument());runtime.Gosched();return value}
var trace string
func destination()int{trace+="d";return 0}
func tracedArgument()int{trace+="a";return 7}
func tracedPair(n int)(int,int){trace+="p";runtime.Gosched();return n+10,1}
func effectAssignmentOwner(f func(int)(int,int),arg func()int)([2]int,int){values:=[2]int{};index:=0;values[destination()],index=f(arg());runtime.Gosched();return values,index}
func boundsOwner(f func(int)(int,int))(caught any){defer func(){caught=recover()}();values:=[2]int{};index:=2;values[index],index=f(7);runtime.Gosched();return}
func partialOwner(f func(int)(int,int))(caught any){one:=&box{};defer func(){caught=recover();if one.n!=17{panic("tuple stores did not proceed left to right")}}();var two *box;one.n,two.n=f(7);runtime.Gosched();return}
var ownerEntry=owner
var methodEntry=methodOwner
var tupleEntry=tupleOwner
var variadicEntry=variadicOwner
var emptyEntry=emptyOwner
var nilEntry=nilOwner
var snapshotEntry=snapshotOwner
var assignmentEntry=assignmentOwner
var plainArgumentEntry=plainArgumentOwner
var knownSnapshotEntry=knownSnapshotOwner
var effectAssignmentEntry=effectAssignmentOwner
var boundsEntry=boundsOwner
var partialEntry=partialOwner
var chosen callback
var producerCalls,argumentCalls int
func produce()callback{producerCalls++;return chosen}
func argument()int{argumentCalls++;chosen=plain;return 7}
func plainArgument()int{argumentCalls++;chosen=plain;return 7}
func main(){
 if ownerEntry(named,7)!=11||calls!=2{panic("native open function result or source replay")}
 if ownerEntry(plain,7)!=13||calls!=4{panic("ordinary open function result or source replay")}
 p:=&box{n:19};p.pad[4095]=11;f:=captured(p);p=nil
 if ownerEntry(f,7)!=38||calls!=6{panic("open captured environment")};f=nil
 p=&box{n:19};p.pad[4095]=23
 if methodEntry((*box).add,p,5)!=24||p.n!=24{panic("open method expression")}
 value,out:=tupleEntry(tuple,p,7)
 if value!=31||out!=p||out.pad[4095]!=23{panic("open typed tuple result")}
 if variadicEntry(variadic)!=17{panic("open normalized variadic request")}
 emptyEntry(store,p,37);if p.n!=37{panic("open empty result request")}
 if _,ok:=nilEntry(nil,argument).(runtime.Error);!ok||argumentCalls!=1{panic("nil function argument evaluation")}
 chosen=named;producerCalls=0;argumentCalls=0
 if snapshotEntry(produce,argument)!=10||producerCalls!=1||argumentCalls!=1{panic("open function snapshot or operand replay")}
 chosen=named;argumentCalls=0
 if plainArgumentEntry()!=12||argumentCalls!=1{panic("plain argument/callee order differs from official")}
 chosen=named;producerCalls=0;argumentCalls=0
 if knownSnapshotEntry()!=10||producerCalls!=1||argumentCalls!=1{panic("known callee producer/argument call order")}
 values,index:=assignmentEntry(pair)
 if values!=[2]int{17,0}||index!=1{panic("open result assignment destination order")}
 trace="";values,index=effectAssignmentEntry(tracedPair,tracedArgument)
 if values!=[2]int{17,0}||index!=1||trace!="dap"{panic("tuple destination/callee operand order")}
 trace="";if _,ok:=boundsEntry(tracedPair).(runtime.Error);!ok||trace!="p"{panic("tuple bounds check crossed producer")}
 trace="";if _,ok:=partialEntry(tracedPair).(runtime.Error);!ok||trace!="p"{panic("tuple partial stores or panic order")}
 runtime.GC();if out.n!=37||out.pad[4095]!=23{panic("open result pointer liveness")}
 println("open-callable-ok")
}
`
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	file := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(file, []byte(source), 0o666); err != nil {
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
			executable := filepath.Join(tmp, config.name)
			args := []string{"build", "-gcflags=" + config.flags, "-o", executable}
			if config.extra != "" {
				args = append(args, config.extra)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" {
				nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", executable)
				data, err := nm.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, data)
				}
				for _, name := range []string{"owner", "methodOwner", "tupleOwner", "variadicOwner", "emptyOwner", "nilOwner", "snapshotOwner", "assignmentOwner", "plainArgumentOwner", "knownSnapshotOwner", "effectAssignmentOwner", "boundsOwner", "partialOwner", "named"} {
					if !hasSwitchReturnFactory(data, name) {
						t.Errorf("open source lost native factory %s", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, executable)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				if data, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(data)) != "open-callable-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
