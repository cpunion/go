// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"fmt"
	"strconv"
	"strings"
)

// renderBasicObjectLLVMForTarget emits the scheduler/operation vertical slice
// used by the restricted object-link example. Two logical tasks share one FIFO.
// Each task yields once and parks on a generation-checked operation. The first
// operation completes before the park is committed. The second is completed
// by a native timer thread while the first task continues to make progress.
func renderBasicObjectLLVMForTarget(hostName string, value int64, goos, goarch string) ([]byte, error) {
	pthreadType, err := pthreadHandleType(goos, goarch)
	if err != nil {
		return nil, err
	}
	replacer := strings.NewReplacer(
		"{{HOST}}", hostName,
		"{{VALUE0}}", strconv.FormatInt(value, 10),
		"{{VALUE1}}", strconv.FormatInt(value+1, 10),
		"{{PTHREAD_T}}", pthreadType,
	)
	return []byte(replacer.Replace(basicSchedulerLLVM)), nil
}

func pthreadHandleType(goos, goarch string) (string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("coroutine timer object does not support %s/%s", goos, goarch)
	}
	switch goos {
	case "darwin":
		return "ptr", nil
	case "linux":
		return "i64", nil
	default:
		return "", fmt.Errorf("coroutine timer object does not support %s/%s", goos, goarch)
	}
}

const basicSchedulerLLVM = `; Generated from the restricted Go coroutine scheduler/operation example.

%queue = type { ptr, ptr }
%operation = type { ptr, i64, i8 }
%timer = type { i8, i64, i32 }
%task = type { ptr, ptr, ptr, ptr, ptr, i64, i64, i32, i8, i8, i1, i1, ptr }

declare noalias ptr @malloc(i64)
declare void @free(ptr)
declare i32 @pthread_create(ptr, ptr, ptr, ptr)
declare i32 @pthread_join({{PTHREAD_T}}, ptr)
declare i32 @nanosleep(ptr, ptr)

declare token @llvm.coro.id(i32, ptr, ptr, ptr)
declare i64 @llvm.coro.size.i64()
declare ptr @llvm.coro.begin(token, ptr)
declare token @llvm.coro.save(ptr)
declare i8 @llvm.coro.suspend(token, i1)
declare ptr @llvm.coro.free(token, ptr)
declare i1 @llvm.coro.end(ptr, i1, token)
declare void @llvm.coro.resume(ptr)
declare i1 @llvm.coro.done(ptr)
declare void @llvm.coro.destroy(ptr)

; Task states: new=0, runnable=1, running=2, waiting=3, dead=4.
; Task actions: none=0, yield=1, park=2.
; Operation phases: idle=0, armed=1, parked=2, ready=3, consumed=4.

define internal i1 @queue.push(ptr %queue, ptr %task) {
entry:
  %queued.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 10
  %queued = load i1, ptr %queued.ptr, align 1
  %state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  %state = load i8, ptr %state.ptr, align 1
  %runnable = icmp eq i8 %state, 1
  %not.queued = xor i1 %queued, true
  %valid = and i1 %runnable, %not.queued
  br i1 %valid, label %accept, label %reject

accept:
  %next.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 1
  store ptr null, ptr %next.ptr, align 8
  store i1 true, ptr %queued.ptr, align 1
  %tail.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 1
  %tail = load ptr, ptr %tail.ptr, align 8
  %empty = icmp eq ptr %tail, null
  br i1 %empty, label %empty.queue, label %append

empty.queue:
  %head.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 0
  store ptr %task, ptr %head.ptr, align 8
  br label %finish

append:
  %tail.next.ptr = getelementptr inbounds %task, ptr %tail, i32 0, i32 1
  store ptr %task, ptr %tail.next.ptr, align 8
  br label %finish

finish:
  store ptr %task, ptr %tail.ptr, align 8
  ret i1 true

reject:
  ret i1 false
}

define internal ptr @queue.pop(ptr %queue) {
entry:
  %head.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 0
  %head = load ptr, ptr %head.ptr, align 8
  %empty = icmp eq ptr %head, null
  br i1 %empty, label %none, label %take

take:
  %next.ptr = getelementptr inbounds %task, ptr %head, i32 0, i32 1
  %next = load ptr, ptr %next.ptr, align 8
  store ptr %next, ptr %head.ptr, align 8
  %last = icmp eq ptr %next, null
  br i1 %last, label %clear.tail, label %finish

clear.tail:
  %tail.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 1
  store ptr null, ptr %tail.ptr, align 8
  br label %finish

finish:
  store ptr null, ptr %next.ptr, align 8
  %queued.ptr = getelementptr inbounds %task, ptr %head, i32 0, i32 10
  store i1 false, ptr %queued.ptr, align 1
  ret ptr %head

none:
  ret ptr null
}

define internal i1 @operation.arm(ptr %operation, ptr %task, i64 %generation) {
entry:
  %waiter.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 0
  %waiter = load ptr, ptr %waiter.ptr, align 8
  %phase.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 2
  %phase = load i8, ptr %phase.ptr, align 1
  %idle = icmp eq i8 %phase, 0
  %no.waiter = icmp eq ptr %waiter, null
  %valid = and i1 %idle, %no.waiter
  br i1 %valid, label %arm, label %reject

arm:
  store ptr %task, ptr %waiter.ptr, align 8
  %generation.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 1
  store i64 %generation, ptr %generation.ptr, align 8
  store i8 1, ptr %phase.ptr, align 1
  ret i1 true

reject:
  ret i1 false
}

define internal i1 @operation.publish(ptr %queue, ptr %operation, i64 %generation) {
entry:
  %generation.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 1
  %want = load i64, ptr %generation.ptr, align 8
  %same.generation = icmp eq i64 %want, %generation
  br i1 %same.generation, label %select, label %reject

select:
  %phase.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 2
  %phase = load i8, ptr %phase.ptr, align 1
  switch i8 %phase, label %reject [
    i8 1, label %publish.early
    i8 2, label %wake
  ]

publish.early:
  store i8 3, ptr %phase.ptr, align 1
  ret i1 true

wake:
  %waiter.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 0
  %task = load ptr, ptr %waiter.ptr, align 8
  %has.task = icmp ne ptr %task, null
  br i1 %has.task, label %check.waiting, label %reject

check.waiting:
  %state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  %state = load i8, ptr %state.ptr, align 1
  %waiting = icmp eq i8 %state, 3
  br i1 %waiting, label %enqueue, label %reject

enqueue:
  store i8 3, ptr %phase.ptr, align 1
  store i8 1, ptr %state.ptr, align 1
  %pushed = call i1 @queue.push(ptr %queue, ptr %task)
  ret i1 %pushed

reject:
  ret i1 false
}

define internal i1 @operation.park(ptr %queue, ptr %operation, ptr %task, i64 %generation) {
entry:
  %waiter.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 0
  %waiter = load ptr, ptr %waiter.ptr, align 8
  %same.task = icmp eq ptr %waiter, %task
  %generation.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 1
  %want = load i64, ptr %generation.ptr, align 8
  %same.generation = icmp eq i64 %want, %generation
  %valid = and i1 %same.task, %same.generation
  br i1 %valid, label %select, label %reject

select:
  %phase.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 2
  %phase = load i8, ptr %phase.ptr, align 1
  switch i8 %phase, label %reject [
    i8 1, label %wait
    i8 3, label %ready
  ]

wait:
  store i8 2, ptr %phase.ptr, align 1
  %wait.state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  store i8 3, ptr %wait.state.ptr, align 1
  ret i1 true

ready:
  %ready.state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  store i8 1, ptr %ready.state.ptr, align 1
  %pushed = call i1 @queue.push(ptr %queue, ptr %task)
  ret i1 %pushed

reject:
  ret i1 false
}

define internal i1 @operation.consume(ptr %operation, ptr %task, i64 %generation) {
entry:
  %waiter.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 0
  %waiter = load ptr, ptr %waiter.ptr, align 8
  %same.task = icmp eq ptr %waiter, %task
  %generation.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 1
  %want = load i64, ptr %generation.ptr, align 8
  %same.generation = icmp eq i64 %want, %generation
  %phase.ptr = getelementptr inbounds %operation, ptr %operation, i32 0, i32 2
  %phase = load i8, ptr %phase.ptr, align 1
  %ready = icmp eq i8 %phase, 3
  %identity = and i1 %same.task, %same.generation
  %valid = and i1 %identity, %ready
  br i1 %valid, label %consume, label %reject

consume:
  store i8 4, ptr %phase.ptr, align 1
  store ptr null, ptr %waiter.ptr, align 8
  ret i1 true

reject:
  ret i1 false
}

define internal ptr @timer.thread(ptr %timer) {
entry:
  %request = alloca [2 x i64], align 8
  %seconds.ptr = getelementptr inbounds [2 x i64], ptr %request, i32 0, i32 0
  store i64 0, ptr %seconds.ptr, align 8
  %nanoseconds.ptr = getelementptr inbounds [2 x i64], ptr %request, i32 0, i32 1
  store i64 1000000, ptr %nanoseconds.ptr, align 8
  br label %wait.progress

wait.progress:
  %progress.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 1
  %progress = load atomic i64, ptr %progress.ptr acquire, align 8
  %enough = icmp uge i64 %progress, 8
  br i1 %enough, label %sleep, label %wait.progress

sleep:
  %status = call i32 @nanosleep(ptr %request, ptr null)
  %status.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 2
  store atomic i32 %status, ptr %status.ptr release, align 4
  %ready.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 0
  store atomic i8 1, ptr %ready.ptr release, align 1
  ret ptr null
}

define internal ptr @{{HOST}}.body(ptr %task) presplitcoroutine {
entry:
  %id = call token @llvm.coro.id(i32 0, ptr null, ptr null, ptr null)
  %size = call i64 @llvm.coro.size.i64()
  %mem = call noalias ptr @malloc(i64 %size)
  %hdl = call noalias ptr @llvm.coro.begin(token %id, ptr %mem)
  %handle.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 0
  store ptr %hdl, ptr %handle.ptr, align 8
  %initial.save = call token @llvm.coro.save(ptr %hdl)
  %initial.kind = call i8 @llvm.coro.suspend(token %initial.save, i1 false)
  switch i8 %initial.kind, label %suspend [
    i8 0, label %start
    i8 1, label %cleanup
  ]

start:
  %action.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 9
  store i8 1, ptr %action.ptr, align 1
  %yield.save = call token @llvm.coro.save(ptr %hdl)
  %yield.kind = call i8 @llvm.coro.suspend(token %yield.save, i1 false)
  switch i8 %yield.kind, label %suspend [
    i8 0, label %after.yield
    i8 1, label %cleanup
  ]

after.yield:
  %operation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 3
  %operation = load ptr, ptr %operation.ptr, align 8
  %generation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 6
  %generation = load i64, ptr %generation.ptr, align 8
  %armed = call i1 @operation.arm(ptr %operation, ptr %task, i64 %generation)
  br i1 %armed, label %maybe.publish, label %arm.failed

maybe.publish:
  %early.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 11
  %early = load i1, ptr %early.ptr, align 1
  br i1 %early, label %publish, label %park

publish:
  %queue.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 2
  %queue = load ptr, ptr %queue.ptr, align 8
  %published = call i1 @operation.publish(ptr %queue, ptr %operation, i64 %generation)
  br i1 %published, label %park, label %publish.failed

park:
  store i8 2, ptr %action.ptr, align 1
  %park.save = call token @llvm.coro.save(ptr %hdl)
  %park.kind = call i8 @llvm.coro.suspend(token %park.save, i1 false)
  switch i8 %park.kind, label %suspend [
    i8 0, label %after.park
    i8 1, label %cleanup
  ]

after.park:
  %consumed = call i1 @operation.consume(ptr %operation, ptr %task, i64 %generation)
  br i1 %consumed, label %select.progress, label %consume.failed

select.progress:
  %task.id.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 7
  %task.id = load i32, ptr %task.id.ptr, align 4
  %is.progress.task = icmp eq i32 %task.id, 0
  br i1 %is.progress.task, label %progress.check, label %complete

progress.check:
  %timer.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 12
  %timer = load ptr, ptr %timer.ptr.ptr, align 8
  %timer.ready.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 0
  %timer.ready.value = load atomic i8, ptr %timer.ready.ptr acquire, align 1
  %timer.ready = icmp ne i8 %timer.ready.value, 0
  br i1 %timer.ready, label %complete, label %progress

progress:
  %timer.progress.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 1
  %old.progress = atomicrmw add ptr %timer.progress.ptr, i64 1 monotonic, align 8
  store i8 1, ptr %action.ptr, align 1
  %progress.save = call token @llvm.coro.save(ptr %hdl)
  %progress.kind = call i8 @llvm.coro.suspend(token %progress.save, i1 false)
  switch i8 %progress.kind, label %suspend [
    i8 0, label %after.progress
    i8 1, label %cleanup
  ]

after.progress:
  br label %progress.check

complete:
  %result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %result.ptr = load ptr, ptr %result.ptr.ptr, align 8
  %value.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 5
  %value = load i64, ptr %value.ptr, align 8
  store i64 %value, ptr %result.ptr, align 8
  br label %final

arm.failed:
  %arm.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %arm.result.ptr = load ptr, ptr %arm.result.ptr.ptr, align 8
  store i64 -101, ptr %arm.result.ptr, align 8
  br label %final

publish.failed:
  %publish.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %publish.result.ptr = load ptr, ptr %publish.result.ptr.ptr, align 8
  store i64 -102, ptr %publish.result.ptr, align 8
  br label %final

consume.failed:
  %consume.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %consume.result.ptr = load ptr, ptr %consume.result.ptr.ptr, align 8
  store i64 -103, ptr %consume.result.ptr, align 8
  br label %final

final:
  store i8 0, ptr %action.ptr, align 1
  %final.kind = call i8 @llvm.coro.suspend(token none, i1 true)
  switch i8 %final.kind, label %suspend [
    i8 0, label %invalid.resume
    i8 1, label %cleanup
  ]

invalid.resume:
  unreachable

cleanup:
  %free.mem = call ptr @llvm.coro.free(token %id, ptr %hdl)
  call void @free(ptr %free.mem)
  br label %suspend

suspend:
  %end = call i1 @llvm.coro.end(ptr %hdl, i1 false, token none)
  ret ptr %hdl
}

define internal i1 @task.init(ptr %queue, ptr %task, ptr %operation, ptr %timer, ptr %result, i32 %id, i64 %value, i1 %early) {
entry:
  store %task zeroinitializer, ptr %task, align 8
  store %operation zeroinitializer, ptr %operation, align 8
  store i64 -1, ptr %result, align 8
  %queue.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 2
  store ptr %queue, ptr %queue.ptr, align 8
  %operation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 3
  store ptr %operation, ptr %operation.ptr, align 8
  %result.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  store ptr %result, ptr %result.ptr, align 8
  %value.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 5
  store i64 %value, ptr %value.ptr, align 8
  %id64 = zext i32 %id to i64
  %generation = add i64 %id64, 1
  %generation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 6
  store i64 %generation, ptr %generation.ptr, align 8
  %id.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 7
  store i32 %id, ptr %id.ptr, align 4
  %early.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 11
  store i1 %early, ptr %early.ptr, align 1
  %timer.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 12
  store ptr %timer, ptr %timer.ptr, align 8
  %hdl = call ptr @{{HOST}}.body(ptr %task)
  %nonnull = icmp ne ptr %hdl, null
  %handle.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 0
  %stored = load ptr, ptr %handle.ptr, align 8
  %same = icmp eq ptr %hdl, %stored
  %done = call i1 @llvm.coro.done(ptr %hdl)
  %not.done = xor i1 %done, true
  %identity = and i1 %nonnull, %same
  %valid = and i1 %identity, %not.done
  br i1 %valid, label %enqueue, label %reject

enqueue:
  %state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  store i8 1, ptr %state.ptr, align 1
  %pushed = call i1 @queue.push(ptr %queue, ptr %task)
  ret i1 %pushed

reject:
  ret i1 false
}

define internal i64 @scheduler.run() {
entry:
  %queue = alloca %queue, align 8
  %task0 = alloca %task, align 8
  %task1 = alloca %task, align 8
  %operation0 = alloca %operation, align 8
  %operation1 = alloca %operation, align 8
  %timer = alloca %timer, align 8
  %thread = alloca {{PTHREAD_T}}, align 8
  %result0 = alloca i64, align 8
  %result1 = alloca i64, align 8
  %order = alloca [4 x i32], align 4
  %resume.count = alloca i32, align 4
  %complete.count = alloca i32, align 4
  %late.published = alloca i1, align 1
  %thread.started = alloca i1, align 1
  %thread.joined = alloca i1, align 1
  store %queue zeroinitializer, ptr %queue, align 8
  store %timer zeroinitializer, ptr %timer, align 8
  store [4 x i32] zeroinitializer, ptr %order, align 4
  store i32 0, ptr %resume.count, align 4
  store i32 0, ptr %complete.count, align 4
  store i1 false, ptr %late.published, align 1
  store i1 false, ptr %thread.started, align 1
  store i1 false, ptr %thread.joined, align 1
  %init0 = call i1 @task.init(ptr %queue, ptr %task0, ptr %operation0, ptr %timer, ptr %result0, i32 0, i64 {{VALUE0}}, i1 true)
  br i1 %init0, label %init.second, label %fail

init.second:
  %init1 = call i1 @task.init(ptr %queue, ptr %task1, ptr %operation1, ptr %timer, ptr %result1, i32 1, i64 {{VALUE1}}, i1 false)
  br i1 %init1, label %loop, label %fail

loop:
  %started = load i1, ptr %thread.started, align 1
  br i1 %started, label %poll.timer, label %pop

poll.timer:
  %poll.ready.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 0
  %poll.ready.value = load atomic i8, ptr %poll.ready.ptr acquire, align 1
  %poll.ready = icmp ne i8 %poll.ready.value, 0
  %already.published = load i1, ptr %late.published, align 1
  %not.published = xor i1 %already.published, true
  %publish.now = and i1 %poll.ready, %not.published
  br i1 %publish.now, label %publish.timer, label %pop

publish.timer:
  %timer.published = call i1 @operation.publish(ptr %queue, ptr %operation1, i64 2)
  store i1 true, ptr %late.published, align 1
  br i1 %timer.published, label %pop, label %fail

pop:
  %task = call ptr @queue.pop(ptr %queue)
  %empty = icmp eq ptr %task, null
  br i1 %empty, label %idle, label %run

idle:
  %completed.idle = load i32, ptr %complete.count, align 4
  %all.complete = icmp eq i32 %completed.idle, 2
  br i1 %all.complete, label %join.timer, label %fail

run:
  %state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  %state = load i8, ptr %state.ptr, align 1
  %runnable = icmp eq i8 %state, 1
  br i1 %runnable, label %record, label %fail

record:
  store i8 2, ptr %state.ptr, align 1
  %handle.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 0
  %hdl = load ptr, ptr %handle.ptr, align 8
  %done.before = call i1 @llvm.coro.done(ptr %hdl)
  br i1 %done.before, label %fail, label %log

log:
  %count = load i32, ptr %resume.count, align 4
  %within.log = icmp ult i32 %count, 4
  br i1 %within.log, label %log.first, label %resume

log.first:
  %log.id.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 7
  %log.id = load i32, ptr %log.id.ptr, align 4
  %order.slot = getelementptr inbounds [4 x i32], ptr %order, i32 0, i32 %count
  store i32 %log.id, ptr %order.slot, align 4
  br label %resume

resume:
  %action.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 9
  store i8 0, ptr %action.ptr, align 1
  call void @llvm.coro.resume(ptr %hdl)
  %next.count = add i32 %count, 1
  store i32 %next.count, ptr %resume.count, align 4
  %done.after = call i1 @llvm.coro.done(ptr %hdl)
  br i1 %done.after, label %complete.task, label %dispatch

complete.task:
  %complete.action = load i8, ptr %action.ptr, align 1
  %no.action = icmp eq i8 %complete.action, 0
  br i1 %no.action, label %destroy, label %fail

destroy:
  store i8 4, ptr %state.ptr, align 1
  call void @llvm.coro.destroy(ptr %hdl)
  %completed = load i32, ptr %complete.count, align 4
  %next.completed = add i32 %completed, 1
  store i32 %next.completed, ptr %complete.count, align 4
  br label %loop

dispatch:
  %action = load i8, ptr %action.ptr, align 1
  switch i8 %action, label %fail [
    i8 1, label %yield
    i8 2, label %park
  ]

yield:
  store i8 1, ptr %state.ptr, align 1
  %yielded = call i1 @queue.push(ptr %queue, ptr %task)
  br i1 %yielded, label %loop, label %fail

park:
  %operation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 3
  %operation = load ptr, ptr %operation.ptr, align 8
  %generation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 6
  %generation = load i64, ptr %generation.ptr, align 8
  %parked = call i1 @operation.park(ptr %queue, ptr %operation, ptr %task, i64 %generation)
  br i1 %parked, label %maybe.start.timer, label %fail

maybe.start.timer:
  %park.id.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 7
  %park.id = load i32, ptr %park.id.ptr, align 4
  %is.timer.task = icmp eq i32 %park.id, 1
  br i1 %is.timer.task, label %start.timer, label %loop

start.timer:
  %was.started = load i1, ptr %thread.started, align 1
  br i1 %was.started, label %fail, label %create.timer

create.timer:
  %create.status = call i32 @pthread_create(ptr %thread, ptr null, ptr @timer.thread, ptr %timer)
  %created = icmp eq i32 %create.status, 0
  br i1 %created, label %timer.started, label %fail

timer.started:
  store i1 true, ptr %thread.started, align 1
  br label %loop

join.timer:
  %thread.is.started = load i1, ptr %thread.started, align 1
  br i1 %thread.is.started, label %join.started, label %fail

join.started:
  %thread.value = load {{PTHREAD_T}}, ptr %thread, align 8
  %join.status = call i32 @pthread_join({{PTHREAD_T}} %thread.value, ptr null)
  store i1 true, ptr %thread.joined, align 1
  %joined = icmp eq i32 %join.status, 0
  br i1 %joined, label %verify, label %fail

verify:
  %resumes = load i32, ptr %resume.count, align 4
  %enough.resumes = icmp uge i32 %resumes, 14
  %result0.value = load i64, ptr %result0, align 8
  %result0.ok = icmp eq i64 %result0.value, {{VALUE0}}
  %result1.value = load i64, ptr %result1, align 8
  %result1.ok = icmp eq i64 %result1.value, {{VALUE1}}
  %operation0.phase.ptr = getelementptr inbounds %operation, ptr %operation0, i32 0, i32 2
  %operation0.phase = load i8, ptr %operation0.phase.ptr, align 1
  %operation0.done = icmp eq i8 %operation0.phase, 4
  %operation1.phase.ptr = getelementptr inbounds %operation, ptr %operation1, i32 0, i32 2
  %operation1.phase = load i8, ptr %operation1.phase.ptr, align 1
  %operation1.done = icmp eq i8 %operation1.phase, 4
  %late.done = load i1, ptr %late.published, align 1
  %timer.ready.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 0
  %timer.ready.value = load atomic i8, ptr %timer.ready.ptr acquire, align 1
  %timer.ready = icmp ne i8 %timer.ready.value, 0
  %timer.progress.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 1
  %timer.progress = load atomic i64, ptr %timer.progress.ptr acquire, align 8
  %timer.progressed = icmp uge i64 %timer.progress, 8
  %timer.status.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 2
  %timer.status = load atomic i32, ptr %timer.status.ptr acquire, align 4
  %timer.slept = icmp eq i32 %timer.status, 0
  %head.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 0
  %head = load ptr, ptr %head.ptr, align 8
  %head.empty = icmp eq ptr %head, null
  %tail.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 1
  %tail = load ptr, ptr %tail.ptr, align 8
  %tail.empty = icmp eq ptr %tail, null
  %base0 = and i1 %enough.resumes, %result0.ok
  %base1 = and i1 %base0, %result1.ok
  %base2 = and i1 %base1, %operation0.done
  %base3 = and i1 %base2, %operation1.done
  %base4 = and i1 %base3, %late.done
  %base5 = and i1 %base4, %timer.ready
  %base6 = and i1 %base5, %timer.progressed
  %base7 = and i1 %base6, %timer.slept
  %base8 = and i1 %base7, %head.empty
  %base9 = and i1 %base8, %tail.empty
  br i1 %base9, label %verify.order, label %fail

verify.order:
  %order0.ptr = getelementptr inbounds [4 x i32], ptr %order, i32 0, i32 0
  %order0 = load i32, ptr %order0.ptr, align 4
  %order0.ok = icmp eq i32 %order0, 0
  %order1.ptr = getelementptr inbounds [4 x i32], ptr %order, i32 0, i32 1
  %order1 = load i32, ptr %order1.ptr, align 4
  %order1.ok = icmp eq i32 %order1, 1
  %order2.ptr = getelementptr inbounds [4 x i32], ptr %order, i32 0, i32 2
  %order2 = load i32, ptr %order2.ptr, align 4
  %order2.ok = icmp eq i32 %order2, 0
  %order3.ptr = getelementptr inbounds [4 x i32], ptr %order, i32 0, i32 3
  %order3 = load i32, ptr %order3.ptr, align 4
  %order3.ok = icmp eq i32 %order3, 1
  %order.check1 = and i1 %order0.ok, %order1.ok
  %order.check2 = and i1 %order.check1, %order2.ok
  %order.check3 = and i1 %order.check2, %order3.ok
  br i1 %order.check3, label %success, label %fail

success:
  ret i64 {{VALUE0}}

fail:
  %fail.started = load i1, ptr %thread.started, align 1
  %fail.joined = load i1, ptr %thread.joined, align 1
  %fail.not.joined = xor i1 %fail.joined, true
  %fail.needs.join = and i1 %fail.started, %fail.not.joined
  br i1 %fail.needs.join, label %fail.join, label %fail.return

fail.join:
  %fail.progress.ptr = getelementptr inbounds %timer, ptr %timer, i32 0, i32 1
  store atomic i64 8, ptr %fail.progress.ptr release, align 8
  %fail.thread.value = load {{PTHREAD_T}}, ptr %thread, align 8
  %fail.join.status = call i32 @pthread_join({{PTHREAD_T}} %fail.thread.value, ptr null)
  store i1 true, ptr %thread.joined, align 1
  br label %fail.return

fail.return:
  ret i64 -1
}

define i64 @{{HOST}}() {
entry:
  %result = call i64 @scheduler.run()
  ret i64 %result
}
`
