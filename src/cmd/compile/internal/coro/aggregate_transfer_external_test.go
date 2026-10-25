// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestNativeAggregateTransferBudget(t *testing.T) {
	const source = `package main
import "runtime"
var trace int
func input(n int) [1024]byte {trace=10*trace+n;return [1024]byte{0:byte(n),1023:7}}
func leaf(v [1024]byte) int {runtime.Gosched();return int(v[0])+int(v[1023])}
func caller(v [1024]byte) int {return leaf(v)}
func recursive(n int,v [1024]byte) int {
 if n==0 {runtime.Gosched();return int(v[0])+int(v[1023])}
 child:=recursive(n-1,v)
 return child+int(v[0])+int(v[1023])
}
type large struct {data [1024]byte}
func structLeaf(v large) int {runtime.Gosched();return int(v.data[0])}
func structCaller(v large) int {return structLeaf(v)}
func pairLeaf(a,b [64]byte) int {runtime.Gosched();return int(a[0])+int(b[63])}
func combined(a,b [64]byte) int {return pairLeaf(a,b)}
func small(v [64]byte) int {runtime.Gosched();return int(v[0])}
//go:noinline
func ordinary(v [64]byte) int {return int(v[0])}
func mixed(v [64]byte) int {n:=ordinary(v);child:=small(v);return n+child}
func boundary(n int,v [100]byte) int {
 if n==0 {runtime.Gosched();return int(v[99])}
 child:=boundary(n-1,v);return child+int(v[0])
}
func beyond(n int,v [101]byte) int {
 if n==0 {runtime.Gosched();return int(v[100])}
 child:=beyond(n-1,v);return child+int(v[0])
}
func zero(n int,v [0]*int) int {
 if n==0 {runtime.Gosched();return 1}
 child:=zero(n-1,v);return child+1
}
func main(){
 if caller(input(1))!=8||trace!=1 {panic("call evaluation")}
 if recursive(64,input(2))!=65*9||trace!=12 {panic("recursive aggregate")}
 if structCaller(large{data:[1024]byte{0:11}})!=11 {panic("struct")}
 if combined([64]byte{0:3},[64]byte{63:5})!=8||mixed([64]byte{0:7})!=14 {panic("combined transfer")}
 if boundary(32,[100]byte{0:2,99:9})!=73||beyond(32,[101]byte{0:2,100:9})!=73||zero(32,[0]*int{})!=33 {panic("boundary")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, []switchFallback{
		{"caller", "native aggregate call exceeds stack transfer budget"},
		{"recursive", "native aggregate call exceeds stack transfer budget"},
		{"structCaller", "native aggregate call exceeds stack transfer budget"},
		{"combined", "native aggregate call exceeds stack transfer budget"},
		{"mixed", "native aggregate call exceeds stack transfer budget"},
		{"beyond", "native aggregate call exceeds stack transfer budget"},
	}, []string{"leaf", "structLeaf", "pairLeaf", "small", "boundary", "zero"})
}
