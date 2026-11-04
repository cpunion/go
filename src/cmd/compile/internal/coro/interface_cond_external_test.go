// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"runtime"
	"testing"
)

// Preserve the original 32-waiter regression, including Locker replacement,
// typed roots, panic/recover, Goexit, and OS-thread affinity.
func TestOpenDynamicCond(t *testing.T) {
	const source = `package main
import("runtime";"sync";"sync/atomic";"syscall")


type countedLocker struct { mu *sync.Mutex; locks,unlocks *int32 }
func(l *countedLocker)Lock(){l.mu.Lock();atomic.AddInt32(l.locks,1)}
func(l *countedLocker)Unlock(){atomic.AddInt32(l.unlocks,1);l.mu.Unlock()}

type selectedLocker struct {mu *sync.Mutex;cond *sync.Cond;next sync.Locker;selected *int32}
func(l *selectedLocker)Lock(){l.mu.Lock()}
func(l *selectedLocker)Unlock(){l.cond.L=l.next;atomic.AddInt32(l.selected,1);l.mu.Unlock()}

func lockerOwner(l sync.Locker,done chan int){l.Lock();defer l.Unlock();runtime.Gosched();done<-13}
func condOwner(c *sync.Cond,condition *bool,ready chan struct{},done chan int){
 c.L.Lock();defer c.L.Unlock()
 for !*condition {ready<-struct{}{};c.Wait()}
 runtime.GC();done<-17
}

func checkLocker(l sync.Locker){done:=make(chan int,1);go lockerOwner(l,done);if <-done!=13{panic("locker result")};l.Lock();l.Unlock()}
func checkCond(l,signalLock sync.Locker){
 c:=sync.NewCond(l);condition:=false;ready:=make(chan struct{});done:=make(chan int,1)
 go condOwner(c,&condition,ready,done);<-ready
 signalLock.Lock();condition=true;c.Signal();signalLock.Unlock()
 if <-done!=17{panic("condition result")}
 l.Lock();l.Unlock()
}
func changedLocker(){
 mu:=new(sync.Mutex);var locks,unlocks,selected int32
 after:=&countedLocker{mu,&locks,&unlocks}
 before:=&selectedLocker{mu:mu,next:after,selected:&selected}
 c:=sync.NewCond(before);before.cond=c
 condition:=false;ready:=make(chan struct{});done:=make(chan int,1)
 // Explicit Unlock below re-reads c.L after Wait. A defer would freeze the
 // original receiver and test a different language contract.
 go func(){c.L.Lock();for !condition{ready<-struct{}{};c.Wait()};c.L.Unlock();done<-19}()
 <-ready;mu.Lock();condition=true;c.Broadcast();mu.Unlock()
 if <-done!=19||atomic.LoadInt32(&selected)!=1||atomic.LoadInt32(&locks)!=1||atomic.LoadInt32(&unlocks)!=1{panic("Locker field was frozen across Wait")}
}
func originalControls(){
 checkLocker(new(sync.Mutex));checkLocker(new(sync.RWMutex));checkLocker(new(sync.RWMutex).RLocker())
 var locks,unlocks int32;checkLocker(&countedLocker{new(sync.Mutex),&locks,&unlocks})
 m:=new(sync.Mutex);checkCond(m,m)
 rw:=new(sync.RWMutex);checkCond(rw,rw);checkCond(rw.RLocker(),rw)
 counted:=&countedLocker{new(sync.Mutex),&locks,&unlocks};checkCond(counted,counted);changedLocker()
}
func main(){nativeAdmission();for range 1{controls();runtime.LockOSThread();controls();runtime.UnlockOSThread()};println("switch-return-ok")}


type parkedValue struct { value int; pad [4095]byte }
func pressureOwner(c *sync.Cond,p *parkedValue,released *bool,ready chan struct{},done chan int){
 c.L.Lock();defer c.L.Unlock()
 for !*released {ready<-struct{}{};c.Wait()}
 if p.value!=37||p.pad[4094]!=41 {panic("interface continuation lost typed root")}
 done<-p.value
}
func pressureCase(l,signalLock sync.Locker){
 const count=32
 c:=sync.NewCond(l);released:=false;ready:=make(chan struct{});done:=make(chan int,count)
 p:=&parkedValue{value:37};p.pad[4094]=41
 for i:=0;i<count;i++ {go pressureOwner(c,p,&released,ready,done)}
 for i:=0;i<count;i++ {<-ready}
 p=nil;runtime.GC()
 signalLock.Lock();released=true;c.Broadcast();signalLock.Unlock()
 total:=0;for i:=0;i<count;i++ {total+=<-done};if total!=count*37 {panic("interface waiters lost")}
}

type panicLocker struct { atLock bool }
func(l panicLocker)Lock(){if l.atLock {panic(211)}}
func(l panicLocker)Unlock(){panic(212)}
func panicOwner(l sync.Locker)(out any){defer func(){out=recover()}();l.Lock();l.Unlock();return nil}
func nilOwner(l sync.Locker)(caught bool){defer func(){caught=recover()!=nil}();l.Lock();return false}
type nilLocker struct{}
func(l *nilLocker)Lock(){if l!=nil {panic("typed nil receiver replaced")};runtime.Gosched()}
func(l *nilLocker)Unlock(){if l!=nil {panic("typed nil receiver replaced")}}

type recoveringLocker struct { result *any }
func(l recoveringLocker)Lock(){}
func(l recoveringLocker)Unlock(){*l.result=recover()}
func directRecoverOwner(l sync.Locker){defer l.Unlock();panic(241)}
func normalRecoverOwner(l sync.Locker)(out any){
 defer func(){out=recover()}()
 func(){defer func(){l.Unlock()}();panic(242)}()
 return nil
}

type exitLocker struct{}
func(exitLocker)Lock(){runtime.Goexit()}
func(exitLocker)Unlock(){}
func exitingOwner(l sync.Locker,done chan int){defer func(){done<-251}();l.Lock();done<-0}

type affineLocker struct { id uintptr }
func(l *affineLocker)Lock(){runtime.LockOSThread();l.id=threadID()}
func(l *affineLocker)Unlock(){if threadID()!=l.id {panic("Locker.Unlock changed OS thread")};runtime.UnlockOSThread()}
func affinityOwner(l sync.Locker,expected *affineLocker,done chan int){
 l.Lock()
 for i:=0;i<32;i++ {runtime.Gosched();if threadID()!=expected.id {panic("caller lost Locker thread affinity")}}
 l.Unlock();done<-257
}

func controls(){
 originalControls()
 m:=new(sync.Mutex);pressureCase(m,m)
 rw:=new(sync.RWMutex);pressureCase(rw,rw);pressureCase(rw.RLocker(),rw)
 var locks,unlocks int32
 counted:=&countedLocker{new(sync.Mutex),&locks,&unlocks};pressureCase(counted,counted)
 if panicOwner(panicLocker{true})!=211||panicOwner(panicLocker{})!=212 {panic("interface panic propagation")}
 if !nilOwner(nil) {panic("nil interface method")}
 var typedNil *nilLocker;checkLocker(typedNil)
 var direct,normal any
 directRecoverOwner(recoveringLocker{&direct})
 if direct!=241 {panic("direct deferred interface method lost recover identity")}
 if normalRecoverOwner(recoveringLocker{&normal})!=242||normal!=nil {panic("normal interface method stole panic")}
 done:=make(chan int,1);go exitingOwner(exitLocker{},done);if <-done!=251 {panic("interface Goexit cleanup")}
 affine:=new(affineLocker);go affinityOwner(affine,affine,done);if <-done!=257 {panic("interface affinity result")}
}
`
	number := "SYS_THREAD_SELFID"
	if runtime.GOOS == "linux" {
		number = "SYS_GETTID"
	} else if runtime.GOOS != "darwin" {
		t.Skip("thread identifier unavailable")
	}
	thread := "func threadID()uintptr{v,_,e:=syscall.RawSyscall(syscall." + number + ",0,0,0);if e!=0{panic(e)};return v}"
	const admission = `//go:build goexperiment.coro

package main
import("sync";"unsafe")
//go:linkname lookupInterface runtime.coroLookupInterface
func lookupInterface(unsafe.Pointer,unsafe.Pointer,uintptr,any)(unsafe.Pointer,unsafe.Pointer)
func nativeAdmission(){
 var locks,unlocks,selected int32;var observed any;var typedNil *nilLocker
 mu:=new(sync.Mutex);after:=&countedLocker{mu,&locks,&unlocks}
 before:=&selectedLocker{mu:mu,next:after,selected:&selected};before.cond=sync.NewCond(before)
 lockers:=[]sync.Locker{new(sync.Mutex),new(sync.RWMutex),new(sync.RWMutex).RLocker(),after,before,panicLocker{},recoveringLocker{&observed},exitLocker{},new(affineLocker),typedNil}
 for i,l:=range lockers{
  words:=(*[2]unsafe.Pointer)(unsafe.Pointer(&l))
  for method:=uintptr(0);method<2;method++{
   p,_:=lookupInterface(words[0],words[1],method,(func())(nil))
   if p==nil{println("missing native Locker",i,method);panic("interface method companion missing")}
   descriptor:=(*[9]unsafe.Pointer)(p)
   if descriptor[4]==nil||descriptor[5]==nil||*(*uintptr)(descriptor[4])!=0{println("missing native defer",i,method);panic("interface defer companion missing")}
  }
 }
}
`

	required := []string{"lockerOwner", "condOwner", "pressureOwner", "panicOwner", "directRecoverOwner", "exitingOwner", "affinityOwner"}
	testSwitchReturnFilesNative(t, map[string]string{
		"go.mod":       "module example.com/corocond\n\ngo 1.27\n",
		"main.go":      source + thread,
		"admission.go": admission,
		"ordinary.go":  "//go:build !goexperiment.coro\n\npackage main\nfunc nativeAdmission(){}\n",
	}, "example.com/corocond=", nil, required, required,
		"-ldflags=-checklinkname=0", "-gcflags=sync=-l -d=coro=4", "-gcflags=internal/sync=-l -d=coro=4")
}
