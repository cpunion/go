// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

const plainValueProgram = `package main
import "runtime"
//go:noinline
func plain(n int) int {return n+3}
//go:noinline
func namedValue() int {f:=plain;return f(4)}
//go:noinline
func capturedValue(which bool) int {n:=1;f:=func(){n+=2};if which{f=func(){n+=4}};f();return n}
//go:noinline
func capturedResult(which bool) int {n:=1;f:=func()int{return n};if which{f=func()int{return n+1}};n=7;return f()}
//go:noinline
func capturedPointers() int {
 var f [3]func()int
 for i:=range f {v:=i;f[i]=func()int{return v}}
 runtime.GC()
 return f[0]()+f[1]()*10+f[2]()*100
}
func main(){
 if namedValue()!=7||capturedValue(false)!=3||capturedValue(true)!=5||capturedResult(false)!=7||capturedResult(true)!=8||capturedPointers()!=210 {panic("plain function value invocation")}
 println("switch-return-ok")
}
`

func TestPlainFunctionValueCalls(t *testing.T) {
	testSwitchReturnProgram(t, plainValueProgram, nil, nil,
		"namedValue", "capturedValue", "capturedResult", "capturedPointers")
}
