// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package corobench

import (
	"runtime"
	"sync/atomic"
	"time"
)

func yieldLoop(iterations int) int {
	completed := 0
	for i := 0; i < iterations; i++ {
		runtime.Gosched()
		completed++
	}
	return completed
}

func yieldEntry() int {
	runtime.Gosched()
	return 1
}

func recursiveYield(depth int) int {
	if depth <= 0 {
		runtime.Gosched()
		return 1
	}
	return recursiveYield(depth-1) + 1
}

const recursiveLargeFrameBytes = 5 << 10

//go:noinline
func recursiveLargeFrameValue(frame *[recursiveLargeFrameBytes]byte) int {
	return int(frame[0] & 1)
}

func recursiveLargeFrame(depth int) int {
	var frame [recursiveLargeFrameBytes]byte
	frame[0] = byte(depth)
	if depth <= 0 {
		runtime.Gosched()
		return 1 + recursiveLargeFrameValue(&frame)
	}
	return recursiveLargeFrame(depth-1) + 1 +
		recursiveLargeFrameValue(&frame)
}

func mutualYieldA(depth int) int {
	if depth <= 0 {
		runtime.Gosched()
		return 1
	}
	return mutualYieldB(depth-1) + 1
}

func mutualYieldB(depth int) int {
	if depth <= 0 {
		runtime.Gosched()
		return 1
	}
	return mutualYieldA(depth-1) + 1
}

func finishDefer(result *int) {
	*result = *result + 1
}

func deferYield() (result int) {
	defer finishDefer(&result)
	runtime.Gosched()
	return 1
}

func recoverYield() (result int) {
	defer func() {
		if recover() != nil {
			result = 1
		}
	}()
	runtime.Gosched()
	panic("corobench")
}

var taskDone uint64

func taskWorker() {
	runtime.Gosched()
	atomic.AddUint64(&taskDone, 1)
}

func taskSequence(iterations int) uint64 {
	if iterations <= 0 {
		return 0
	}
	atomic.StoreUint64(&taskDone, 0)
	for want := uint64(1); want <= uint64(iterations); want++ {
		go taskWorker()
		for atomic.LoadUint64(&taskDone) < want {
			runtime.Gosched()
		}
	}
	return atomic.LoadUint64(&taskDone)
}

func taskBursts(rounds, tasks int) uint64 {
	if rounds <= 0 || tasks <= 0 {
		return 0
	}
	var total uint64
	for round := 0; round < rounds; round++ {
		atomic.StoreUint64(&taskDone, 0)
		for task := 0; task < tasks; task++ {
			go taskWorker()
		}
		for atomic.LoadUint64(&taskDone) < uint64(tasks) {
			runtime.Gosched()
		}
		total += atomic.LoadUint64(&taskDone)
	}
	return total
}

var (
	taskParkGate  chan struct{}
	taskParkReady uint64
	taskParkDone  uint64
)

func taskParkWorker() {
	atomic.AddUint64(&taskParkReady, 1)
	<-taskParkGate
	atomic.AddUint64(&taskParkDone, 1)
}

func taskParkBursts(rounds, tasks int) uint64 {
	if rounds <= 0 || tasks <= 0 {
		return 0
	}
	var total uint64
	for round := 0; round < rounds; round++ {
		taskParkGate = make(chan struct{})
		atomic.StoreUint64(&taskParkReady, 0)
		atomic.StoreUint64(&taskParkDone, 0)
		for task := 0; task < tasks; task++ {
			go taskParkWorker()
		}
		for atomic.LoadUint64(&taskParkReady) < uint64(tasks) {
			runtime.Gosched()
		}
		close(taskParkGate)
		for atomic.LoadUint64(&taskParkDone) < uint64(tasks) {
			runtime.Gosched()
		}
		total += atomic.LoadUint64(&taskParkDone)
	}
	return total
}

var taskParkRelease uint64

func taskParkUntilReleased(tasks int) uint64 {
	if tasks <= 0 {
		return 0
	}
	taskParkGate = make(chan struct{})
	atomic.StoreUint64(&taskParkReady, 0)
	atomic.StoreUint64(&taskParkDone, 0)
	atomic.StoreUint64(&taskParkRelease, 0)
	for task := 0; task < tasks; task++ {
		go taskParkWorker()
	}
	for atomic.LoadUint64(&taskParkReady) < uint64(tasks) {
		runtime.Gosched()
	}
	for atomic.LoadUint64(&taskParkRelease) == 0 {
		runtime.Gosched()
	}
	close(taskParkGate)
	for atomic.LoadUint64(&taskParkDone) < uint64(tasks) {
		runtime.Gosched()
	}
	return atomic.LoadUint64(&taskParkDone)
}

var (
	parallelYieldDone     uint64
	parallelYieldChecksum uint64
	parallelYieldSeed     uint64
	parallelYieldWorkSize int
)

var (
	parallelSpawnStarted uint64
	parallelSpawnDone    uint64
	parallelSpawnRelease uint32
	parallelSpawnTimeout uint32
)

func parallelSpawnWorker() {
	runtime.Gosched()
	atomic.AddUint64(&parallelSpawnStarted, 1)
	for atomic.LoadUint32(&parallelSpawnRelease) == 0 &&
		atomic.LoadUint32(&parallelSpawnTimeout) == 0 {
	}
	atomic.AddUint64(&parallelSpawnDone, 1)
}

func parallelSpawnProgress(workers int) bool {
	if workers <= 0 {
		return false
	}
	// Keep this helper on the automatically colored path while leaving the
	// spawn-and-wait episode itself free of suspension points.
	runtime.Gosched()
	atomic.StoreUint64(&parallelSpawnStarted, 0)
	atomic.StoreUint64(&parallelSpawnDone, 0)
	atomic.StoreUint32(&parallelSpawnRelease, 0)
	for worker := 0; worker < workers; worker++ {
		go parallelSpawnWorker()
	}
	for atomic.LoadUint64(&parallelSpawnStarted) != uint64(workers) &&
		atomic.LoadUint32(&parallelSpawnTimeout) == 0 {
	}
	started := atomic.LoadUint64(&parallelSpawnStarted) == uint64(workers)
	atomic.StoreUint32(&parallelSpawnRelease, 1)
	for atomic.LoadUint64(&parallelSpawnDone) != uint64(workers) &&
		atomic.LoadUint32(&parallelSpawnTimeout) == 0 {
	}
	return started && atomic.LoadUint64(&parallelSpawnDone) == uint64(workers)
}

func parallelWork(iterations int, value uint64) uint64 {
	for i := 0; i < iterations; i++ {
		value = value*6364136223846793005 + 1442695040888963407
	}
	return value
}

func parallelYieldWorker() {
	seed := atomic.AddUint64(&parallelYieldSeed, 1)
	value := parallelWork(parallelYieldWorkSize, seed)
	runtime.Gosched()
	value = parallelWork(parallelYieldWorkSize, value)
	atomic.AddUint64(&parallelYieldChecksum, value)
	atomic.AddUint64(&parallelYieldDone, 1)
}

func parallelYieldWork(rounds, tasks, work int) uint64 {
	if rounds <= 0 || tasks <= 0 || work <= 0 {
		return 0
	}
	var total uint64
	for round := 0; round < rounds; round++ {
		atomic.StoreUint64(&parallelYieldDone, 0)
		atomic.StoreUint64(&parallelYieldChecksum, 0)
		atomic.StoreUint64(&parallelYieldSeed, uint64(round*tasks))
		parallelYieldWorkSize = work
		for task := 0; task < tasks; task++ {
			go parallelYieldWorker()
		}
		for atomic.LoadUint64(&parallelYieldDone) < uint64(tasks) {
			runtime.Gosched()
		}
		total += atomic.LoadUint64(&parallelYieldChecksum)
	}
	return total
}

var (
	channelPing             chan int
	channelPong             chan int
	channelIterations       int
	blockedSelectReady      chan int
	blockedSelectIterations int
	blockedSelectTrigger    uint64
	blockedSelectDone       uint32
)

func channelWorker() {
	for i := 0; i < channelIterations; i++ {
		value := <-channelPing
		channelPong <- value + 1
	}
}

func channelRoundTrips(iterations int) int {
	if iterations <= 0 {
		return 0
	}
	channelPing = make(chan int)
	channelPong = make(chan int)
	channelIterations = iterations
	go channelWorker()
	sum := 0
	for i := 0; i < iterations; i++ {
		channelPing <- i
		value := <-channelPong
		sum += value
	}
	return sum
}

func readyChannelPairs(iterations int) int {
	channel := make(chan int, 1)
	sum := 0
	for i := 0; i < iterations; i++ {
		channel <- i
		value := <-channel
		sum += value
	}
	return sum
}

func readySelects(iterations int) int {
	left := make(chan int, 1)
	right := make(chan int, 1)
	sum := 0
	for i := 0; i < iterations; i++ {
		if i&1 == 0 {
			left <- i
		} else {
			right <- i
		}
		select {
		case value := <-left:
			sum += value
		case value := <-right:
			sum += value
		}
	}
	return sum
}

func blockedSelectWorker() {
	for i := 0; i < blockedSelectIterations; i++ {
		for atomic.LoadUint64(&blockedSelectTrigger) < uint64(i+1) {
			runtime.Gosched()
		}
		blockedSelectReady <- i + 1
	}
	atomic.StoreUint32(&blockedSelectDone, 1)
}

func blockedSelects(iterations int) int {
	if iterations <= 0 {
		return 0
	}
	blockedSelectReady = make(chan int)
	blockedSelectIterations = iterations
	atomic.StoreUint64(&blockedSelectTrigger, 0)
	atomic.StoreUint32(&blockedSelectDone, 0)
	go blockedSelectWorker()
	var disabled <-chan int
	sum := 0
	for i := 0; i < iterations; i++ {
		atomic.StoreUint64(&blockedSelectTrigger, uint64(i+1))
		select {
		case value := <-blockedSelectReady:
			sum += value
		case <-disabled:
		}
	}
	for atomic.LoadUint32(&blockedSelectDone) == 0 {
		runtime.Gosched()
	}
	return sum
}

func sleepLoop(iterations int, delay time.Duration) int {
	completed := 0
	for i := 0; i < iterations; i++ {
		time.Sleep(delay)
		completed++
	}
	return completed
}
