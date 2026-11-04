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
type pairKey struct{x,y int}
type wordMapKey struct{x uint64}
var boolKeyValue bool
var pairKeyValue pairKey
var wordKeyValue wordMapKey
var stringKeyValue string
var structKeyMap map[pairKey]int
//go:noinline
func structKeyRHS()int{trace+="r";runtime.Gosched();structKeyMap=map[pairKey]int{pairKey{0,0}:40,pairKey{1,0}:50};indexValue=1;return 2}
//go:noinline
func structKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";structKeyMap=map[pairKey]int{pairKey{0,0}:1,pairKey{1,0}:3};structKeyMap[pairKey{indexValue,0}]+=structKeyRHS();return structKeyMap[pairKey{0,0}],structKeyMap[pairKey{1,0}]}
var boolKeyMap map[bool]int
//go:noinline
func boolKeyRHS()int{trace+="r";runtime.Gosched();boolKeyMap=map[bool]int{true:40,false:50};indexValue=1;return 2}
//go:noinline
func boolKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";boolKeyMap=map[bool]int{true:1,false:3};boolKeyMap[indexValue==0]+=boolKeyRHS();return boolKeyMap[true],boolKeyMap[false]}
var smallKeyMap map[uint8]int
//go:noinline
func smallKeyRHS()int{trace+="r";runtime.Gosched();smallKeyMap=map[uint8]int{0:40,1:50};indexValue=1;return 2}
//go:noinline
func smallKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";smallKeyMap=map[uint8]int{0:1,1:3};smallKeyMap[uint8(indexValue+0)]+=smallKeyRHS();return smallKeyMap[0],smallKeyMap[1]}
var intExpressionKeyMap map[int]int
//go:noinline
func intExpressionKeyRHS()int{trace+="r";runtime.Gosched();intExpressionKeyMap=map[int]int{0:40,1:50};indexValue=1;return 2}
//go:noinline
func intExpressionKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";intExpressionKeyMap=map[int]int{0:1,1:3};intExpressionKeyMap[indexValue+0]+=intExpressionKeyRHS();return intExpressionKeyMap[0],intExpressionKeyMap[1]}
var wordKeyMap map[wordMapKey]int
//go:noinline
func wordKeyRHS()int{trace+="r";runtime.Gosched();wordKeyMap=map[wordMapKey]int{wordMapKey{0}:40,wordMapKey{1}:50};indexValue=1;return 2}
//go:noinline
func wordKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";wordKeyMap=map[wordMapKey]int{wordMapKey{0}:1,wordMapKey{1}:3};wordKeyMap[wordMapKey{uint64(indexValue)}]+=wordKeyRHS();return wordKeyMap[wordMapKey{0}],wordKeyMap[wordMapKey{1}]}
var arrayKeyMap map[[1]uint64]int
//go:noinline
func arrayKeyRHS()int{trace+="r";runtime.Gosched();arrayKeyMap=map[[1]uint64]int{[1]uint64{0}:40,[1]uint64{1}:50};indexValue=1;return 2}
//go:noinline
func arrayKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";arrayKeyMap=map[[1]uint64]int{[1]uint64{0}:1,[1]uint64{1}:3};arrayKeyMap[[1]uint64{uint64(indexValue)}]+=arrayKeyRHS();return arrayKeyMap[[1]uint64{0}],arrayKeyMap[[1]uint64{1}]}
var floatKeyMap map[float64]int
//go:noinline
func floatKeyRHS()int{trace+="r";runtime.Gosched();floatKeyMap=map[float64]int{0:40,1:50};indexValue=1;return 2}
//go:noinline
func floatKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";floatKeyMap=map[float64]int{0:1,1:3};floatKeyMap[float64(indexValue+0)]+=floatKeyRHS();return floatKeyMap[0],floatKeyMap[1]}
var complexKeyMap map[complex128]int
//go:noinline
func complexKeyRHS()int{trace+="r";runtime.Gosched();complexKeyMap=map[complex128]int{0:40,1:50};indexValue=1;return 2}
//go:noinline
func complexKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";complexKeyMap=map[complex128]int{0:1,1:3};complexKeyMap[complex(float64(indexValue),0)]+=complexKeyRHS();return complexKeyMap[0],complexKeyMap[1]}
var interfaceKeyMap map[any]int
//go:noinline
func interfaceKeyRHS()int{trace+="r";runtime.Gosched();interfaceKeyMap=map[any]int{0:40,1:50};indexValue=1;return 2}
//go:noinline
func interfaceKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";interfaceKeyMap=map[any]int{0:1,1:3};interfaceKeyMap[any(indexValue+0)]+=interfaceKeyRHS();return interfaceKeyMap[0],interfaceKeyMap[1]}
var lateBoolKeyMap map[bool]int
//go:noinline
func lateBoolKeyRHS()int{trace+="r";runtime.Gosched();lateBoolKeyMap=map[bool]int{true:40,false:50};boolKeyValue=false;return 2}
//go:noinline
func lateBoolKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";lateBoolKeyMap=map[bool]int{true:1,false:3};lateBoolKeyMap[boolKeyValue]+=lateBoolKeyRHS();return lateBoolKeyMap[true],lateBoolKeyMap[false]}
var lateStructKeyMap map[pairKey]int
//go:noinline
func lateStructKeyRHS()int{trace+="r";runtime.Gosched();lateStructKeyMap=map[pairKey]int{pairKey{0,0}:40,pairKey{1,0}:50};pairKeyValue=pairKey{1,0};return 2}
//go:noinline
func lateStructKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";lateStructKeyMap=map[pairKey]int{pairKey{0,0}:1,pairKey{1,0}:3};lateStructKeyMap[pairKeyValue]+=lateStructKeyRHS();return lateStructKeyMap[pairKey{0,0}],lateStructKeyMap[pairKey{1,0}]}
var savedWordKeyMap map[wordMapKey]int
//go:noinline
func savedWordKeyRHS()int{trace+="r";runtime.Gosched();savedWordKeyMap=map[wordMapKey]int{wordMapKey{0}:40,wordMapKey{1}:50};wordKeyValue=wordMapKey{1};return 2}
//go:noinline
func savedWordKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";savedWordKeyMap=map[wordMapKey]int{wordMapKey{0}:1,wordMapKey{1}:3};savedWordKeyMap[wordKeyValue]+=savedWordKeyRHS();return savedWordKeyMap[wordMapKey{0}],savedWordKeyMap[wordMapKey{1}]}
var lateStringKeyMap map[string]int
//go:noinline
func lateStringKeyRHS()int{trace+="r";runtime.Gosched();lateStringKeyMap=map[string]int{"a":40,"b":50};stringKeyValue="b";return 2}
//go:noinline
func lateStringKey()(int,int){indexValue=0;boolKeyValue=true;pairKeyValue=pairKey{0,0};wordKeyValue=wordMapKey{0};stringKeyValue="a";lateStringKeyMap=map[string]int{"a":1,"b":3};lateStringKeyMap[stringKeyValue]+=lateStringKeyRHS();return lateStringKeyMap["a"],lateStringKeyMap["b"]}
//go:noinline
func keyBounds()(caught any){defer func(){caught=recover()}();a:=[]int{0};m:=map[uint8]int{};m[uint8(a[2])]+=one();return}
//go:noinline
func keyNil()(caught any){defer func(){caught=recover()}();var p *int;m:=map[uint8]int{};m[uint8(*p)]+=one();return}
type rootedKey struct{p *box;n int}
var rootKeyPointer *box
var rootKeyMap map[rootedKey]int
//go:noinline
func rootedMapKeyRHS()int{trace+="r";runtime.Gosched();rootKeyPointer=&box{n:40};runtime.GC();return 2}
//go:noinline
func rootedMapKey()(int,int){rootKeyPointer=&box{n:7};rootKeyMap=map[rootedKey]int{};rootKeyMap[rootedKey{rootKeyPointer,0}]+=rootedMapKeyRHS();for k,v:=range rootKeyMap{return k.p.n,v};panic("missing map entry")}
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
 trace="";a,b=structKey();if a!=42||b!=50||trace!="r"{panic("structKey timing")}
 trace="";a,b=boolKey();if a!=42||b!=50||trace!="r"{panic("boolKey timing")}
 trace="";a,b=smallKey();if a!=42||b!=50||trace!="r"{panic("smallKey timing")}
 trace="";a,b=intExpressionKey();if a!=40||b!=52||trace!="r"{panic("intExpressionKey timing")}
 trace="";a,b=wordKey();if a!=42||b!=50||trace!="r"{panic("wordKey timing")}
 trace="";a,b=arrayKey();if a!=42||b!=50||trace!="r"{panic("arrayKey timing")}
 trace="";a,b=floatKey();if a!=42||b!=50||trace!="r"{panic("floatKey timing")}
 trace="";a,b=complexKey();if a!=42||b!=50||trace!="r"{panic("complexKey timing")}
 trace="";a,b=interfaceKey();if a!=42||b!=50||trace!="r"{panic("interfaceKey timing")}
 trace="";a,b=lateBoolKey();if a!=40||b!=52||trace!="r"{panic("lateBoolKey timing")}
 trace="";a,b=lateStructKey();if a!=40||b!=52||trace!="r"{panic("lateStructKey timing")}
 trace="";a,b=savedWordKey();if a!=42||b!=50||trace!="r"{panic("savedWordKey timing")}
 trace="";a,b=lateStringKey();if a!=40||b!=52||trace!="r"{panic("lateStringKey timing")}
 trace="";if errorText(keyBounds())!="runtime error: index out of range [2] with length 1"||trace!=""{panic("early key bounds")}
 trace="";if errorText(keyNil())!="runtime error: invalid memory address or nil pointer dereference"||trace!=""{panic("early key nil check")}
 trace="";a,b=rootedMapKey();if a!=7||b!=2||trace!="r"{panic("key pointer lifetime")}
 println("switch-return-ok")
}
`

func TestNormalizeCompoundAssignments(t *testing.T) {
	testSwitchReturnProgram(t, compoundAssignmentProgram, nil, nil,
		"scalarStore", "pointerStore", "selectedStore", "sliceStore", "indexedStore", "awaitedIndexStore", "arrayStore",
		"mapStore", "keyedStore", "nilPointer", "outOfBounds", "nilMap", "unhashableKey", "divideMap", "remainderMap",
		"rhsPanic", "lhsPanic", "loopPost", "guarded", "rhsBounds",
		"structKey", "boolKey", "smallKey", "intExpressionKey", "wordKey", "arrayKey", "floatKey", "complexKey", "interfaceKey", "lateBoolKey", "lateStructKey", "savedWordKey", "lateStringKey", "keyBounds", "keyNil", "rootedMapKey")
}
