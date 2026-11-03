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

// This tests discovery through the real compiler/linker and runtime funcdata.
// It deliberately does not stand in for open call-site or interface admission.
func TestCallableCompanionMetadata(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	const source = `package main
import("reflect";"runtime";"unsafe")

// The fixture inspects a private compiler/runtime ABI. Disabling linkname
// checking below does not make this an application-facing runtime interface.
//go:linkname lookup runtime.coroLookupCallable
func lookup(uintptr,unsafe.Pointer)unsafe.Pointer

func named(n int)int{runtime.Gosched();return n+3}
func plain(n int)int{return n+5}
func large(value [1024]byte)int{runtime.Gosched();return int(value[0])}
type box struct{n int;pad[2047]byte}
func(p *box)add(n int)int{runtime.Gosched();p.n+=n;return p.n}
func captured(p *box)func(int)int{return func(n int)int{runtime.Gosched();return p.n+n}}

func signature(f any)unsafe.Pointer{return (*[2]unsafe.Pointer)(unsafe.Pointer(&f))[0]}
func descriptor(f any)unsafe.Pointer{return lookup(reflect.ValueOf(f).Pointer(),signature(f))}
func check(f any){
 p:=descriptor(f);if p==nil{panic("missing callable descriptor")}
 words:=(*[4]uintptr)(p)
 if words[0]!=1||words[1]!=uintptr(signature(f))||words[2]==0||words[3]==0{panic("callable descriptor identity")}
 gate:=(*[2]uintptr)(unsafe.Pointer(words[2]))
 if gate[0]!=0||gate[1]!=words[3]{panic("erased adapter has no independent accepted stack proof")}
 code:=reflect.ValueOf(f).Pointer()
 if lookup(code+1,signature(f))!=nil{panic("interior code pointer accepted")}
 if lookup(code,signature((func(string)int)(nil)))!=nil{panic("wrong callable signature accepted")}
 if lookup(code,nil)!=nil||lookup(0,signature(f))!=nil{panic("missing callable identity accepted")}
}

func main(){
 for range 1{
  p:=&box{n:11};p.pad[2046]=17;f:=captured(p)
  check(named);check(f);check((*box).add)
  if descriptor(plain)!=nil{panic("plain function acquired an unproved factory")}
  if descriptor(large)!=nil{panic("large transfer acquired native capability")}
  runtime.GC()
  if named(7)!=10||f(13)!=24||(*box).add(p,2)!=13||plain(4)!=9||large([1024]byte{19})!=19||p.pad[2046]!=17{panic("ordinary source behavior changed")}
  runtime.LockOSThread();if f(2)!=15||named(3)!=6{panic("ordinary thread entry changed")};runtime.UnlockOSThread()
 }
 println("callable-metadata-ok")
}
`
	path := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(path, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, flags, link string }{
		{"native", "-l", ""},
		{"checkptr", "-l -d=checkptr=2", ""},
		{"inline", "", ""},
		{"checkptr-inline", "-d=checkptr=2", ""},
		{"no-dwarf", "", "-w"},
		{"pie", "-l", "-buildmode=pie"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(tmp, config.name+".exe")
			args := []string{"build", "-gcflags=" + config.flags + " -d=coro=4", "-ldflags=-checklinkname=0"}
			if config.link == "-w" {
				args[len(args)-1] += " -w"
			} else if config.link != "" {
				args = append(args, config.link)
			}
			args = append(args, "-o", exe, path)
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Env = append(build.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				if data, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(data)) != "callable-metadata-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
