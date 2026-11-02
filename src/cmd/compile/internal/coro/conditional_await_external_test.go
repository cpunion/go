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

func TestConditionalAwaitExpressions(t *testing.T) {
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
func conjunction(v bool)bool{return v && take(true,1)}
//go:noinline
func disjunction(v bool)bool{return v || take(false,2)}
//go:noinline
func nested(a,b,c bool)bool{return take(a,1)&&(take(b,2)||take(c,3))}
func makeCaller(seed int)func(func(int)int)int{return func(f func(int)int)int{seed++;if seed<0{return f(-seed)};return f(seed)}}
//go:noinline
func inline(v bool)bool{g:=func(x int)int{order=order*10+4;runtime.Gosched();return x+1};return v&&makeCaller(11)(g)==13}
//go:noinline
func pair()(bool,bool){order=order*10+2;runtime.Gosched();return true,true}
//go:noinline
func consume(a,b bool)bool{order=order*10+3;runtime.Gosched();return a&&b}
//go:noinline
func tuples(v bool)bool{return take(v,1)&&consume(pair())}
//go:noinline
func nilCall(v,isNil bool)(caught any){f:=take;if isNil{f=nil};defer func(){if p:=recover();p!=nil{caught=p}}();return v&&f(take(true,4),5)}
//go:noinline
func loop()int{n:=0;for i:=range 5{if i&1==0&&take(true,i+1){n++}};return n}
func main(){
 order=0;if conjunction(false)||order!=0{panic("AND executed skipped operand")};if !conjunction(true)||order!=1{panic("AND selected operand")}
 order=0;if !disjunction(true)||order!=0{panic("OR executed skipped operand")};if disjunction(false)||order!=2{panic("OR selected operand")}
 order=0;if nested(false,true,true)||order!=1{panic("nested first guard")};order=0;if !nested(true,true,false)||order!=12{panic("nested OR guard")};order=0;if !nested(true,false,true)||order!=123{panic("nested selected sequence")}
 order=0;if inline(false)||order!=0{panic("inline executed skipped body")};if !inline(true)||order!=4{panic("inline selected body")}
 order=0;if tuples(false)||order!=1{panic("tuple initializer crossed branch")};order=0;if !tuples(true)||order!=123{panic("tuple selected order")}
 order=0;if nilCall(false,true)!=false||order!=0{panic("skipped nil call")};if _,ok:=nilCall(true,true).(runtime.Error);!ok||order!=4{panic("nil call operand order")};order=0;if nilCall(true,false)!=true||order!=45{panic("selected dynamic call")}
 order=0;if loop()!=3||order!=135{panic("loop condition order")};runtime.GC();runtime.LockOSThread();order=0;if !nested(true,false,true)||order!=123{panic("ordinary fallback")};runtime.UnlockOSThread();println("conditional-await-ok")
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
				for _, name := range []string{"conjunction", "disjunction", "nested", "inline", "tuples", "nilCall", "loop"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "conditional-await-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
