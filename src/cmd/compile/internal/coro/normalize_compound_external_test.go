// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

const compoundAssignmentProgram = `package main
import "runtime"
type box struct{n int}
var scalar int
var pointer *box
var slice []int
var mapping map[int]int
var indexValue int
var trace string
//go:noinline
func scalarRHS()int{trace+="r";runtime.Gosched();scalar=40;return 2}
//go:noinline
func scalarStore()int{scalar=1;scalar+=scalarRHS();return scalar}
//go:noinline
func pointerRHS()int{trace+="r";runtime.Gosched();pointer=&box{n:40};return 2}
//go:noinline
func pointerStore()(int,int){old:=&box{n:1};pointer=old;pointer.n+=pointerRHS();return old.n,pointer.n}
//go:noinline
func selected()*box{trace+="s";return pointer}
//go:noinline
func selectedStore()(int,int){old:=&box{n:1};pointer=old;selected().n+=pointerRHS();return old.n,pointer.n}
//go:noinline
func sliceRHS()int{trace+="r";runtime.Gosched();slice=[]int{40,50};indexValue=1;return 2}
//go:noinline
func index()int{trace+="i";return indexValue}
//go:noinline
func awaitIndex()int{trace+="i";runtime.Gosched();return indexValue}
//go:noinline
func sliceStore()(int,int){slice=[]int{1,3};indexValue=0;slice[indexValue]+=sliceRHS();return slice[0],slice[1]}
//go:noinline
func indexedStore()(int,int){slice=[]int{1,3};indexValue=0;slice[index()]+=sliceRHS();return slice[0],slice[1]}
//go:noinline
func awaitedIndexStore()(int,int){slice=[]int{1,3};indexValue=0;slice[awaitIndex()]+=sliceRHS();return slice[0],slice[1]}
//go:noinline
func arrayStore()(int,int){a:=[2]int{1,3};indexValue=0;a[indexValue]+=sliceRHS();return a[0],a[1]}
//go:noinline
func mapRHS()int{trace+="r";runtime.Gosched();mapping=map[int]int{0:40,1:50};indexValue=1;return 2}
//go:noinline
func mapStore()(int,int){mapping=map[int]int{0:1,1:3};indexValue=0;mapping[indexValue]+=mapRHS();return mapping[0],mapping[1]}
//go:noinline
func keyedStore()(int,int){mapping=map[int]int{0:1,1:3};indexValue=0;mapping[index()]+=mapRHS();return mapping[0],mapping[1]}
//go:noinline
func one()int{trace+="r";runtime.Gosched();return 2}
//go:noinline
func zero()int{trace+="z";runtime.Gosched();return 0}
//go:noinline
func fail()int{trace+="f";runtime.Gosched();panic("rhs")}
//go:noinline
func failedPointer()*int{trace+="l";runtime.Gosched();panic("lhs")}
//go:noinline
func nilPointer()(caught any){defer func(){caught=recover()}();var p *int;*p+=one();return}
//go:noinline
func outOfBounds()(caught any){defer func(){caught=recover()}();a:=[]int{0};a[2]+=one();return}
//go:noinline
func nilMap()(caught any){defer func(){caught=recover()}();var m map[int]int;m[0]+=one();return}
//go:noinline
func unhashableKey()(caught any){defer func(){caught=recover()}();m:=map[any]int{};m[[]int{}]+=one();return}
//go:noinline
func divideMap()(caught any){defer func(){caught=recover()}();var m map[int]int;m[0]/=zero();return}
//go:noinline
func remainderMap()(caught any){defer func(){caught=recover()}();var m map[int]int;m[0]%=zero();return}
//go:noinline
func rhsPanic()(caught any){defer func(){caught=recover()}();var p *int;*p+=fail();return}
//go:noinline
func lhsPanic()(caught any){defer func(){caught=recover()}();*failedPointer()+=one();return}
//go:noinline
func loopPost()int{total:=0;for i:=0;i<6;i+=one(){total++;if i==2{continue}};return total}
//go:noinline
func guarded(enabled bool)int{n:=1;if enabled{n+=one()};return n}
//go:noinline
func rhsBounds()(caught any){defer func(){caught=recover()}();a:=[]int{0};n:=1;n+=a[2]+one();return}
func errorText(value any)string{if value==nil{return "nil"};if v,ok:=value.(runtime.Error);ok{return v.Error()};if v,ok:=value.(string);ok{return v};return "other"}
func main(){
 if _,_,_,ok:=runtime.Caller(0);!ok{panic("ordinary controller")}
 trace="";if scalarStore()!=42||trace!="r"{panic("scalar read timing")}
 trace="";a,b:=pointerStore();if a!=1||b!=42||trace!="r"{panic("pointer read timing")}
 trace="";a,b=selectedStore();if a!=3||b!=40||trace!="sr"{panic("single LHS call")}
 trace="";a,b=sliceStore();if a!=40||b!=52||trace!="r"{panic("late slice and index")}
 trace="";a,b=indexedStore();if a!=42||b!=50||trace!="ir"{panic("frozen index call")}
 trace="";a,b=awaitedIndexStore();if a!=42||b!=50||trace!="ir"{panic("awaited index call")}
 trace="";a,b=arrayStore();if a!=1||b!=5||trace!="r"{panic("late array index")}
 trace="";a,b=mapStore();if a!=40||b!=52||trace!="r"{panic("late map and key")}
 trace="";a,b=keyedStore();if a!=42||b!=50||trace!="ir"{panic("frozen map key call")}
 trace="";if errorText(nilPointer())!="runtime error: invalid memory address or nil pointer dereference"||trace!="r"{panic("nil check timing")}
 trace="";if errorText(outOfBounds())!="runtime error: index out of range [2] with length 1"||trace!="r"{panic("bounds check timing")}
 trace="";if errorText(nilMap())!="assignment to entry in nil map"||trace!="r"{panic("nil map timing")}
 trace="";if errorText(unhashableKey())!="runtime error: hash of unhashable type []int"||trace!="r"{panic("map hash timing")}
 trace="";if errorText(divideMap())!="runtime error: integer divide by zero"||trace!="z"{panic("map division panic priority")}
 trace="";if errorText(remainderMap())!="runtime error: integer divide by zero"||trace!="z"{panic("map remainder panic priority")}
 trace="";if rhsPanic()!="rhs"||trace!="f"{panic("RHS panic priority")}
 trace="";if lhsPanic()!="lhs"||trace!="l"{panic("LHS panic priority")}
 trace="";if loopPost()!=3||trace!="rrr"{panic("compound loop Post")}
 trace="";if guarded(false)!=1||trace!=""||guarded(true)!=3||trace!="r"{panic("guarded compound")}
 trace="";if errorText(rhsBounds())!="runtime error: index out of range [2] with length 1"||trace!="r"{panic("RHS call and bounds order")}
 println("switch-return-ok")
}
`

func TestNormalizeCompoundAssignments(t *testing.T) {
	testSwitchReturnProgram(t, compoundAssignmentProgram, nil, nil,
		"scalarStore", "pointerStore", "selectedStore", "sliceStore", "indexedStore", "awaitedIndexStore", "arrayStore",
		"mapStore", "keyedStore", "nilPointer", "outOfBounds", "nilMap", "unhashableKey", "divideMap", "remainderMap",
		"rhsPanic", "lhsPanic", "loopPost", "guarded", "rhsBounds")
}
