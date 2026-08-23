// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"strconv"
	"strings"
)

type blockingObjectLLVMRecipe struct {
	namespace    string
	declarations string
	initialize   string
	read         string
	write        string
}

// renderFileObjectLLVMForTarget emits the blocking-file vertical slice used
// by the restricted object-link example. The calling thread remains in read
// while a replacement native thread owns the scheduler queue and advances a
// sibling coroutine. The replacement writes the byte that releases read,
// relinquishes queue ownership, and exits before the calling thread resumes
// scheduler mutation.
func renderFileObjectLLVMForTarget(hostName string, value int64, goos, goarch string) ([]byte, error) {
	return renderBlockingObjectLLVMForTarget(hostName, value, goos, goarch, blockingObjectLLVMRecipe{
		namespace: "file",
		declarations: `declare i32 @pipe(ptr)
declare i64 @read(i32, ptr, i64)
declare i64 @write(i32, ptr, i64)`,
		initialize: `  %pipe.status = call i32 @pipe(ptr %fds.ptr)
  %pipe.ok = icmp eq i32 %pipe.status, 0
  br i1 %pipe.ok, label %initialize.io, label %fail.free`,
		read:  "call i64 @read(i32 %read.fd, ptr %read.byte.ptr, i64 1)",
		write: "call i64 @write(i32 %write.fd, ptr %write.byte.ptr, i64 1)",
	})
}

func renderBlockingObjectLLVMForTarget(hostName string, value int64, goos, goarch string, recipe blockingObjectLLVMRecipe) ([]byte, error) {
	target, err := basicObjectTargetFor(goos, goarch)
	if err != nil {
		return nil, err
	}
	replacer := strings.NewReplacer(
		"{{HOST}}", hostName,
		"{{VALUE0}}", strconv.FormatInt(value, 10),
		"{{VALUE1}}", strconv.FormatInt(value+1, 10),
		"{{IO_NAMESPACE}}", recipe.namespace,
		"{{IO_DECLARATIONS}}", recipe.declarations,
		"{{IO_INITIALIZE}}", recipe.initialize,
		"{{IO_READ}}", recipe.read,
		"{{IO_WRITE}}", recipe.write,
		"{{PTHREAD_T}}", target.pthreadType,
		"{{WRAPPER_ATTRIBUTES}}", target.wrapperAttributes,
		"{{GO_ABI_RETURN}}", target.goABIReturn,
	)
	return []byte(replacer.Replace(fileSchedulerLLVM)), nil
}

const fileSchedulerLLVM = basicSchedulerCoreLLVM + `

%file = type {
  [2 x i32],
  i8, i8, i8, i8, i8, i8, i8,
  i64, i64, i64,
  i32,
  {{PTHREAD_T}}, {{PTHREAD_T}}, {{PTHREAD_T}},
  i8, i8
}
%scheduler = type {
  %queue,
  %task, %task,
  %operation, %operation,
  i64, i64,
  i32, i32,
  %file
}

declare {{PTHREAD_T}} @pthread_self()
declare i32 @pthread_equal({{PTHREAD_T}}, {{PTHREAD_T}})
{{IO_DECLARATIONS}}
declare i32 @close(i32)

define internal i1 @{{IO_NAMESPACE}}.fail(ptr %file) {
entry:
  %failed.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 7
  store atomic i8 1, ptr %failed.ptr release, align 1
  %fds.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 0
  %write.fd.ptr = getelementptr inbounds [2 x i32], ptr %fds.ptr, i32 0, i32 1
  %write.fd = load i32, ptr %write.fd.ptr, align 4
  %close.status = call i32 @close(i32 %write.fd)
  %write.done.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 4
  store atomic i8 1, ptr %write.done.ptr release, align 1
  ret i1 false
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
  %task.id.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 7
  %task.id = load i32, ptr %task.id.ptr, align 4
  %progress.task = icmp eq i32 %task.id, 0
  br i1 %progress.task, label %progress.check, label %blocker.start

progress.check:
  %state.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 12
  %state = load ptr, ptr %state.ptr.ptr, align 8
  %file = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 9
  %read.done.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 5
  %read.done.value = load atomic i8, ptr %read.done.ptr acquire, align 1
  %read.done = icmp ne i8 %read.done.value, 0
  br i1 %read.done, label %complete, label %progress

progress:
  %progress.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 8
  %old.progress = atomicrmw add ptr %progress.ptr, i64 1 monotonic, align 8
  %next.progress = add i64 %old.progress, 1
  %write.now = icmp eq i64 %next.progress, 8
  br i1 %write.now, label %write.byte, label %progress.yield

write.byte:
  %fds.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 0
  %write.fd.ptr = getelementptr inbounds [2 x i32], ptr %fds.ptr, i32 0, i32 1
  %write.fd = load i32, ptr %write.fd.ptr, align 4
  %write.byte.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 15
  %write.count = {{IO_WRITE}}
  %write.count.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 10
  store atomic i64 %write.count, ptr %write.count.ptr release, align 8
  %write.ok = icmp eq i64 %write.count, 1
  br i1 %write.ok, label %write.complete, label %write.failed

write.failed:
  %failure = call i1 @{{IO_NAMESPACE}}.fail(ptr %file)
  br label %progress.yield

write.complete:
  %write.done.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 4
  store atomic i8 1, ptr %write.done.ptr release, align 1
  br label %progress.yield

progress.yield:
  %progress.action.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 9
  store i8 1, ptr %progress.action.ptr, align 1
  %progress.save = call token @llvm.coro.save(ptr %hdl)
  %progress.kind = call i8 @llvm.coro.suspend(token %progress.save, i1 false)
  switch i8 %progress.kind, label %suspend [
    i8 0, label %progress.check
    i8 1, label %cleanup
  ]

blocker.start:
  %blocker.state.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 12
  %blocker.state = load ptr, ptr %blocker.state.ptr.ptr, align 8
  %blocker.file = getelementptr inbounds %scheduler, ptr %blocker.state, i32 0, i32 9
  %operation.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 3
  %operation = load ptr, ptr %operation.ptr.ptr, align 8
  %generation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 6
  %generation = load i64, ptr %generation.ptr, align 8
  %armed = call i1 @operation.arm(ptr %operation, ptr %task, i64 %generation)
  br i1 %armed, label %create.replacement, label %arm.failed

create.replacement:
  %thread.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 12
  %create.status = call i32 @pthread_create(ptr %thread.ptr, ptr null, ptr @{{IO_NAMESPACE}}.replacement.thread, ptr %blocker.state)
  %created = icmp eq i32 %create.status, 0
  br i1 %created, label %wait.replacement, label %create.failed

create.failed:
  %create.read.done.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 5
  store atomic i8 1, ptr %create.read.done.ptr release, align 1
  %create.failed.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 7
  store atomic i8 1, ptr %create.failed.ptr release, align 1
  %create.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %create.result.ptr = load ptr, ptr %create.result.ptr.ptr, align 8
  store i64 -201, ptr %create.result.ptr, align 8
  br label %final

wait.replacement:
  %replacement.ready.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 2
  %replacement.ready.value = load atomic i8, ptr %replacement.ready.ptr acquire, align 1
  %replacement.ready = icmp ne i8 %replacement.ready.value, 0
  br i1 %replacement.ready, label %handoff, label %wait.replacement

handoff:
  %owner.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 1
  store atomic i8 1, ptr %owner.ptr release, align 1
  %blocker.fds.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 0
  %read.fd.ptr = getelementptr inbounds [2 x i32], ptr %blocker.fds.ptr, i32 0, i32 0
  %read.fd = load i32, ptr %read.fd.ptr, align 4
  %read.byte.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 16
  %read.count = {{IO_READ}}
  %read.count.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 9
  store atomic i64 %read.count, ptr %read.count.ptr release, align 8
  br label %wait.replacement.done

wait.replacement.done:
  %replacement.done.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 3
  %replacement.done.value = load atomic i8, ptr %replacement.done.ptr acquire, align 1
  %replacement.done = icmp ne i8 %replacement.done.value, 0
  br i1 %replacement.done, label %join.replacement, label %wait.replacement.done

join.replacement:
  %thread.value = load {{PTHREAD_T}}, ptr %thread.ptr, align 8
  %join.status = call i32 @pthread_join({{PTHREAD_T}} %thread.value, ptr null)
  %join.status.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 11
  store i32 %join.status, ptr %join.status.ptr, align 4
  store atomic i8 2, ptr %owner.ptr release, align 1
  %blocker.read.done.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 5
  store atomic i8 1, ptr %blocker.read.done.ptr release, align 1
  br label %validate.read

validate.read:
  %read.one = icmp eq i64 %read.count, 1
  %read.byte = load i8, ptr %read.byte.ptr, align 1
  %byte.ok = icmp eq i8 %read.byte, 70
  %distinct.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 6
  %distinct.value = load atomic i8, ptr %distinct.ptr acquire, align 1
  %distinct = icmp ne i8 %distinct.value, 0
  %failed.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 7
  %failed.value = load atomic i8, ptr %failed.ptr acquire, align 1
  %not.failed = icmp eq i8 %failed.value, 0
  %blocker.progress.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 8
  %blocker.progress = load atomic i64, ptr %blocker.progress.ptr acquire, align 8
  %progressed = icmp uge i64 %blocker.progress, 8
  %blocker.write.count.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 10
  %blocker.write.count = load atomic i64, ptr %blocker.write.count.ptr acquire, align 8
  %wrote.one = icmp eq i64 %blocker.write.count, 1
  %joined = icmp eq i32 %join.status, 0
  %read.check1 = and i1 %read.one, %byte.ok
  %read.check2 = and i1 %read.check1, %distinct
  %read.check3 = and i1 %read.check2, %not.failed
  %read.check4 = and i1 %read.check3, %progressed
  %read.check5 = and i1 %read.check4, %wrote.one
  %read.valid = and i1 %read.check5, %joined
  br i1 %read.valid, label %publish, label %read.failed

publish:
  %queue.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 2
  %queue = load ptr, ptr %queue.ptr.ptr, align 8
  %published = call i1 @operation.publish(ptr %queue, ptr %operation, i64 %generation)
  br i1 %published, label %park, label %publish.failed

park:
  %blocker.action.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 9
  store i8 2, ptr %blocker.action.ptr, align 1
  %park.save = call token @llvm.coro.save(ptr %hdl)
  %park.kind = call i8 @llvm.coro.suspend(token %park.save, i1 false)
  switch i8 %park.kind, label %suspend [
    i8 0, label %after.park
    i8 1, label %cleanup
  ]

after.park:
  %consumed = call i1 @operation.consume(ptr %operation, ptr %task, i64 %generation)
  br i1 %consumed, label %complete, label %consume.failed

arm.failed:
  %arm.failure = call i1 @{{IO_NAMESPACE}}.fail(ptr %blocker.file)
  %arm.read.done.ptr = getelementptr inbounds %file, ptr %blocker.file, i32 0, i32 5
  store atomic i8 1, ptr %arm.read.done.ptr release, align 1
  %arm.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %arm.result.ptr = load ptr, ptr %arm.result.ptr.ptr, align 8
  store i64 -202, ptr %arm.result.ptr, align 8
  br label %final

read.failed:
  %read.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %read.result.ptr = load ptr, ptr %read.result.ptr.ptr, align 8
  store i64 -203, ptr %read.result.ptr, align 8
  br label %final

publish.failed:
  %publish.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %publish.result.ptr = load ptr, ptr %publish.result.ptr.ptr, align 8
  store i64 -204, ptr %publish.result.ptr, align 8
  br label %final

consume.failed:
  %consume.result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %consume.result.ptr = load ptr, ptr %consume.result.ptr.ptr, align 8
  store i64 -205, ptr %consume.result.ptr, align 8
  br label %final

complete:
  %result.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 4
  %result.ptr = load ptr, ptr %result.ptr.ptr, align 8
  %value.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 5
  %value = load i64, ptr %value.ptr, align 8
  store i64 %value, ptr %result.ptr, align 8
  br label %final

final:
  %final.action.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 9
  store i8 0, ptr %final.action.ptr, align 1
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

define internal i1 @{{IO_NAMESPACE}}.task.init(ptr %state, ptr %task, ptr %operation, ptr %result, i32 %id, i64 %value) {
entry:
  store %task zeroinitializer, ptr %task, align 8
  store %operation zeroinitializer, ptr %operation, align 8
  store i64 -1, ptr %result, align 8
  %queue = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 0
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
  %state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 12
  store ptr %state, ptr %state.ptr, align 8
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
  %state.field.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  store i8 1, ptr %state.field.ptr, align 1
  %pushed = call i1 @queue.push(ptr %queue, ptr %task)
  ret i1 %pushed

reject:
  ret i1 false
}

define internal i1 @scheduler.step(ptr %scheduler, i1 %replacement) {
entry:
  %queue = getelementptr inbounds %scheduler, ptr %scheduler, i32 0, i32 0
  %task = call ptr @queue.pop(ptr %queue)
  %has.task = icmp ne ptr %task, null
  br i1 %has.task, label %check.owner, label %reject

check.owner:
  %id.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 7
  %id = load i32, ptr %id.ptr, align 4
  %progress.task = icmp eq i32 %id, 0
  %owner.valid = select i1 %replacement, i1 %progress.task, i1 true
  br i1 %owner.valid, label %check.state, label %reject

check.state:
  %state.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 8
  %state = load i8, ptr %state.ptr, align 1
  %runnable = icmp eq i8 %state, 1
  br i1 %runnable, label %resume, label %reject

resume:
  store i8 2, ptr %state.ptr, align 1
  %handle.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 0
  %hdl = load ptr, ptr %handle.ptr, align 8
  %done.before = call i1 @llvm.coro.done(ptr %hdl)
  br i1 %done.before, label %reject, label %run

run:
  %action.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 9
  store i8 0, ptr %action.ptr, align 1
  %resume.count.ptr = getelementptr inbounds %scheduler, ptr %scheduler, i32 0, i32 7
  %old.resume.count = atomicrmw add ptr %resume.count.ptr, i32 1 monotonic, align 4
  call void @llvm.coro.resume(ptr %hdl)
  %done.after = call i1 @llvm.coro.done(ptr %hdl)
  br i1 %done.after, label %complete, label %dispatch

complete:
  %complete.action = load i8, ptr %action.ptr, align 1
  %no.action = icmp eq i8 %complete.action, 0
  br i1 %no.action, label %destroy, label %reject

destroy:
  store i8 4, ptr %state.ptr, align 1
  call void @llvm.coro.destroy(ptr %hdl)
  %complete.count.ptr = getelementptr inbounds %scheduler, ptr %scheduler, i32 0, i32 8
  %old.complete.count = atomicrmw add ptr %complete.count.ptr, i32 1 monotonic, align 4
  ret i1 true

dispatch:
  %action = load i8, ptr %action.ptr, align 1
  switch i8 %action, label %reject [
    i8 1, label %yield
    i8 2, label %park
  ]

yield:
  store i8 1, ptr %state.ptr, align 1
  %yielded = call i1 @queue.push(ptr %queue, ptr %task)
  ret i1 %yielded

park:
  %operation.ptr.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 3
  %operation = load ptr, ptr %operation.ptr.ptr, align 8
  %generation.ptr = getelementptr inbounds %task, ptr %task, i32 0, i32 6
  %generation = load i64, ptr %generation.ptr, align 8
  %parked = call i1 @operation.park(ptr %queue, ptr %operation, ptr %task, i64 %generation)
  ret i1 %parked

reject:
  ret i1 false
}

define internal i1 @scheduler.replacement.run(ptr %scheduler) {
entry:
  %file = getelementptr inbounds %scheduler, ptr %scheduler, i32 0, i32 9
  br label %loop

loop:
  %owner.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 1
  %owner = load atomic i8, ptr %owner.ptr acquire, align 1
  %owns.queue = icmp eq i8 %owner, 1
  br i1 %owns.queue, label %check.done, label %reject

check.done:
  %write.done.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 4
  %write.done.value = load atomic i8, ptr %write.done.ptr acquire, align 1
  %write.done = icmp ne i8 %write.done.value, 0
  br i1 %write.done, label %success, label %step

step:
  %advanced = call i1 @scheduler.step(ptr %scheduler, i1 true)
  br i1 %advanced, label %loop, label %reject

success:
  ret i1 true

reject:
  ret i1 false
}

define internal ptr @{{IO_NAMESPACE}}.replacement.thread(ptr %scheduler) {
entry:
  %file = getelementptr inbounds %scheduler, ptr %scheduler, i32 0, i32 9
  %self = call {{PTHREAD_T}} @pthread_self()
  %replacement.thread.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 14
  store {{PTHREAD_T}} %self, ptr %replacement.thread.ptr, align 8
  %original.thread.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 13
  %original = load {{PTHREAD_T}}, ptr %original.thread.ptr, align 8
  %equal = call i32 @pthread_equal({{PTHREAD_T}} %original, {{PTHREAD_T}} %self)
  %distinct = icmp eq i32 %equal, 0
  %distinct.value = zext i1 %distinct to i8
  %distinct.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 6
  store atomic i8 %distinct.value, ptr %distinct.ptr release, align 1
  %ready.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 2
  store atomic i8 1, ptr %ready.ptr release, align 1
  br label %wait.owner

wait.owner:
  %owner.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 1
  %owner = load atomic i8, ptr %owner.ptr acquire, align 1
  %owns.queue = icmp eq i8 %owner, 1
  br i1 %owns.queue, label %drive, label %wait.owner

drive:
  %ran = call i1 @scheduler.replacement.run(ptr %scheduler)
  br i1 %ran, label %finish, label %fail

fail:
  %failed = call i1 @{{IO_NAMESPACE}}.fail(ptr %file)
  br label %finish

finish:
  %done.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 3
  store atomic i8 1, ptr %done.ptr release, align 1
  ret ptr null
}

define internal i64 @scheduler.run() {
entry:
  %state.end = getelementptr %scheduler, ptr null, i32 1
  %state.size = ptrtoint ptr %state.end to i64
  %state = call noalias ptr @malloc(i64 %state.size)
  %state.valid = icmp ne ptr %state, null
  br i1 %state.valid, label %initialize, label %fail.no.state

initialize:
  store %scheduler zeroinitializer, ptr %state, align 8
  %file = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 9
  %fds.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 0
{{IO_INITIALIZE}}

initialize.io:
  %write.byte.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 15
  store i8 70, ptr %write.byte.ptr, align 1
  %original = call {{PTHREAD_T}} @pthread_self()
  %original.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 13
  store {{PTHREAD_T}} %original, ptr %original.ptr, align 8
  %queue = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 0
  %task0 = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 1
  %task1 = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 2
  %operation0 = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 3
  %operation1 = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 4
  %result0 = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 5
  %result1 = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 6
  %init1 = call i1 @{{IO_NAMESPACE}}.task.init(ptr %state, ptr %task1, ptr %operation1, ptr %result1, i32 1, i64 {{VALUE1}})
  br i1 %init1, label %init.progress, label %fail.close

init.progress:
  %init0 = call i1 @{{IO_NAMESPACE}}.task.init(ptr %state, ptr %task0, ptr %operation0, ptr %result0, i32 0, i64 {{VALUE0}})
  br i1 %init0, label %loop, label %fail.close

loop:
  %complete.count.ptr = getelementptr inbounds %scheduler, ptr %state, i32 0, i32 8
  %complete.count = load atomic i32, ptr %complete.count.ptr acquire, align 4
  %complete = icmp eq i32 %complete.count, 2
  br i1 %complete, label %verify, label %step

step:
  %advanced = call i1 @scheduler.step(ptr %state, i1 false)
  br i1 %advanced, label %loop, label %fail.close

verify:
  %result0.value = load i64, ptr %result0, align 8
  %result0.ok = icmp eq i64 %result0.value, {{VALUE0}}
  %result1.value = load i64, ptr %result1, align 8
  %result1.ok = icmp eq i64 %result1.value, {{VALUE1}}
  %phase.ptr = getelementptr inbounds %operation, ptr %operation1, i32 0, i32 2
  %phase = load i8, ptr %phase.ptr, align 1
  %operation.done = icmp eq i8 %phase, 4
  %owner.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 1
  %owner = load atomic i8, ptr %owner.ptr acquire, align 1
  %owner.returned = icmp eq i8 %owner, 2
  %progress.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 8
  %progress = load atomic i64, ptr %progress.ptr acquire, align 8
  %progressed = icmp uge i64 %progress, 8
  %read.count.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 9
  %read.count = load atomic i64, ptr %read.count.ptr acquire, align 8
  %read.one = icmp eq i64 %read.count, 1
  %write.count.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 10
  %write.count = load atomic i64, ptr %write.count.ptr acquire, align 8
  %wrote.one = icmp eq i64 %write.count, 1
  %distinct.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 6
  %distinct.value = load atomic i8, ptr %distinct.ptr acquire, align 1
  %distinct = icmp ne i8 %distinct.value, 0
  %failed.ptr = getelementptr inbounds %file, ptr %file, i32 0, i32 7
  %failed.value = load atomic i8, ptr %failed.ptr acquire, align 1
  %not.failed = icmp eq i8 %failed.value, 0
  %head.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 0
  %head = load ptr, ptr %head.ptr, align 8
  %head.empty = icmp eq ptr %head, null
  %tail.ptr = getelementptr inbounds %queue, ptr %queue, i32 0, i32 1
  %tail = load ptr, ptr %tail.ptr, align 8
  %tail.empty = icmp eq ptr %tail, null
  %check1 = and i1 %result0.ok, %result1.ok
  %check2 = and i1 %check1, %operation.done
  %check3 = and i1 %check2, %owner.returned
  %check4 = and i1 %check3, %progressed
  %check5 = and i1 %check4, %read.one
  %check6 = and i1 %check5, %wrote.one
  %check7 = and i1 %check6, %distinct
  %check8 = and i1 %check7, %not.failed
  %check9 = and i1 %check8, %head.empty
  %valid = and i1 %check9, %tail.empty
  br i1 %valid, label %success.close, label %fail.close

success.close:
  %read.fd.ptr = getelementptr inbounds [2 x i32], ptr %fds.ptr, i32 0, i32 0
  %read.fd = load i32, ptr %read.fd.ptr, align 4
  %close.read = call i32 @close(i32 %read.fd)
  %write.fd.ptr = getelementptr inbounds [2 x i32], ptr %fds.ptr, i32 0, i32 1
  %write.fd = load i32, ptr %write.fd.ptr, align 4
  %close.write = call i32 @close(i32 %write.fd)
  call void @free(ptr %state)
  ret i64 {{VALUE0}}

fail.close:
  %fail.read.fd.ptr = getelementptr inbounds [2 x i32], ptr %fds.ptr, i32 0, i32 0
  %fail.read.fd = load i32, ptr %fail.read.fd.ptr, align 4
  %fail.close.read = call i32 @close(i32 %fail.read.fd)
  %fail.write.fd.ptr = getelementptr inbounds [2 x i32], ptr %fds.ptr, i32 0, i32 1
  %fail.write.fd = load i32, ptr %fail.write.fd.ptr, align 4
  %fail.close.write = call i32 @close(i32 %fail.write.fd)
  br label %fail.free

fail.free:
  call void @free(ptr %state)
  br label %fail.no.state

fail.no.state:
  ret i64 -1
}

define i64 @{{HOST}}(){{WRAPPER_ATTRIBUTES}} {
entry:
  %result = call i64 @scheduler.run()
{{GO_ABI_RETURN}}  ret i64 %result
}
`
