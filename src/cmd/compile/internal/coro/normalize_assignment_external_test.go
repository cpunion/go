// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestNormalizeAssignmentStores(t *testing.T) {
	const source = `package main
import "runtime"
type box struct{ n int; next *box }
var pointer *box
var trace string
//go:noinline
func value() int { trace+="v";runtime.Gosched();return 6 }
//go:noinline
func redirect() int { trace+="r";runtime.Gosched();pointer=&box{n:100};return 6 }
//go:noinline
func fail() int { trace+="f";runtime.Gosched();panic("rhs") }
//go:noinline
func selected() *box { trace+="s";return pointer }
//go:noinline
func fieldStore() int { p:=&box{};p.n=value()+1;return p.n }
//go:noinline
func indirectStore() int { n:=0;p:=&n;*p=value()+1;return n }
//go:noinline
func latePointer() { old:=&box{n:1};pointer=old;pointer.n=redirect()+1;if old.n!=1||pointer.n!=7 {panic("early pointer snapshot")} }
//go:noinline
func nilStore()(caught any) {defer func(){caught=recover()}();var p *box;p.n=value()+1;return}
//go:noinline
func nestedNilStore()(caught any) {defer func(){caught=recover()}();p:=&box{};p.next.n=value()+1;return}
//go:noinline
func orderedStores()(caught any) {p:=&box{};defer func(){caught=recover();if p.n!=4 {panic("partial assignment")}}();var q *box;p.n,q.n=4,value()+1;return}
//go:noinline
func lateMulti() {old:=&box{n:1};pointer=old;var n int;n,pointer.n=4,redirect()+1;if n!=4||old.n!=1||pointer.n!=7 {panic("multi assignment pointer")}}
//go:noinline
func rhsPanic()(caught any) {defer func(){caught=recover()}();var p *box;p.n=fail()+1;return}
//go:noinline
func lhsCallFallback() int {pointer=&box{};selected().n=value()+1;return pointer.n}
//go:noinline
func indexFallback()(caught any) {defer func(){caught=recover()}();s:=[]int{0};i:=2;s[i]=value()+1;return}
//go:noinline
func mapFallback()(caught any) {defer func(){caught=recover()}();var m map[int]int;m[0]=value()+1;return}
func controls() {
 trace="";if fieldStore()!=7||trace!="v" {panic("field store")}
 trace="";if indirectStore()!=7||trace!="v" {panic("indirect store")}
 trace="";latePointer();if trace!="r" {panic("late pointer")}
 trace="";if _,ok:=nilStore().(runtime.Error);!ok||trace!="v" {panic("nil store timing")}
 trace="";if _,ok:=nestedNilStore().(runtime.Error);!ok||trace!="v" {panic("nested nil timing")}
 trace="";if _,ok:=orderedStores().(runtime.Error);!ok||trace!="v" {panic("ordered stores")}
 trace="";lateMulti();if trace!="r" {panic("late multi")}
 trace="";if rhsPanic()!="rhs"||trace!="f" {panic("rhs panic priority")}
 trace="";if lhsCallFallback()!=7||trace!="sv" {panic("lhs call order")}
 trace="";if _,ok:=indexFallback().(runtime.Error);!ok||trace!="v" {panic("bounds timing")}
 trace="";if mapFallback()==nil||trace!="v" {panic("nil map timing")}
}
func main(){controls();runtime.LockOSThread();controls();runtime.UnlockOSThread();println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"lhsCallFallback", "nested await site"}, {"indexFallback", "nested await site"}, {"mapFallback", "nested await site"}},
		[]string{"fieldStore", "indirectStore", "latePointer", "nilStore", "nestedNilStore", "orderedStores", "lateMulti", "rhsPanic"})
}
