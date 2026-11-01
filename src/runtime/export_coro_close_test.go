// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

type stacklessCoroCloseTestFrame struct {
	pointer  *byte
	scalar   uintptr
	received bool
	index    int
}

// Use actual channel queues and source factories. A concurrent consumer
// checks result publication, FIFO order within each scheduler, and drainage
// while close is still returning the remaining operations to their caches.
func StacklessCoroClosePublicationForTest(count, schedulers int,
	buffered, pointers, full bool) bool {
	states := make([]*stacklessCoroScheduler, schedulers)
	for i := range states {
		s := newStacklessCoroTestScheduler()
		s.wake = make(chan struct{}, 1)
		s.lifetime.Store(stacklessCoroLifetimeRootComplete | stacklessCoroLifetimeSpawned)
		s.root.state = stacklessCoroTaskComplete
		states[i] = s
	}
	capacity := 0
	if buffered {
		capacity = 1
	}
	var channel *hchan
	var pointerChannel chan *byte
	var scalarChannel chan uintptr
	if pointers {
		pointerChannel = make(chan *byte, capacity)
		channel = *(**hchan)(unsafe.Pointer(&pointerChannel))
	} else {
		scalarChannel = make(chan uintptr, capacity)
		channel = *(**hchan)(unsafe.Pointer(&scalarChannel))
	}
	frames := make([]*stacklessCoroCloseTestFrame, count)
	for i := range frames {
		s := states[i%len(states)]
		frame := new(stacklessCoroCloseTestFrame)
		frame.index = i
		frame.received = true
		var element unsafe.Pointer
		if pointers {
			frame.pointer = new(byte)
			*frame.pointer = 73
			element = unsafe.Pointer(&frame.pointer)
		} else {
			frame.scalar = 73
			element = unsafe.Pointer(&frame.scalar)
		}
		frames[i] = frame
		task := new(stacklessCoroTask)
		task.state = stacklessCoroTaskRunning
		task.resuming = true
		task.context.scheduler = s
		task.context.frame = unsafe.Pointer(frame)
		if full {
			// A real IO-origin operation retains full waiter storage when
			// the next source is a channel. It remains a direct close source.
			s.freeOperations = newStacklessCoroIOOperation()
			s.freeOperationCount = 1
		}
		s.liveSpawns++
		startStacklessCoroChannel(unsafe.Pointer(&task.context), channel,
			element, &frame.received, false)
		task.resuming = false
	}
	GC()
	valid := new(bool)
	*valid = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		last := make([]int, len(states))
		for i := range last {
			last[i] = -1
		}
		completed := 0
		for completed < count {
			progress := false
			for i, s := range states {
				task := s.take()
				if task == nil {
					continue
				}
				frame := (*stacklessCoroCloseTestFrame)(task.context.frame)
				*valid = *valid && frame != nil && !frame.received &&
					frame.pointer == nil && frame.scalar == 0 &&
					frame.index > last[i] && frame.index%len(states) == i
				last[i] = frame.index
				task.context.scheduler = nil
				s.complete(task)
				completed++
				progress = true
			}
			if !progress {
				Gosched()
			}
			if completed != 0 && completed%31 == 0 {
				GC()
			}
		}
	}()
	if pointers {
		close(pointerChannel)
	} else {
		close(scalarChannel)
	}
	<-done
	for _, s := range states {
		*valid = *valid && s.drained() && s.head == nil && s.tail == nil &&
			s.freeOperationCount == 0 && s.freeOperations == nil
	}
	for _, frame := range frames {
		*valid = *valid && !frame.received && frame.pointer == nil && frame.scalar == 0
	}
	KeepAlive(frames)
	KeepAlive(states)
	return *valid
}

func stacklessCoroWakeReceiverForTest(wake chan struct{}) bool {
	c := *(**hchan)(unsafe.Pointer(&wake))
	deadline := nanotime() + 5e9
	for {
		lock(&c.lock)
		parked := c.recvq.first != nil
		unlock(&c.lock)
		if parked {
			return true
		}
		if nanotime() >= deadline {
			return false
		}
		Gosched()
	}
}

// A public root may end while a detached task remains. Exercise actual
// parked work drivers at both the root-return and final-drain boundaries,
// with and without the separate executor-stop notification.
func StacklessCoroCompletionWakeForTest(executors bool) bool {
	s := newStacklessCoroTestScheduler()
	s.wake = make(chan struct{})
	s.rootDone = make(chan struct{})
	s.lifetime.Store(stacklessCoroLifetimeSpawned)
	s.liveSpawns = 2
	s.root.state = stacklessCoroTaskRunning
	s.root.resuming = true
	if executors {
		s.executorStop = make(chan struct{})
		s.executorState.Store(stacklessCoroExecutorStateRunning)
	}
	first, last := new(stacklessCoroTask), new(stacklessCoroTask)
	first.state, last.state = stacklessCoroTaskRunning, stacklessCoroTaskRunning
	first.resuming, last.resuming = true, true
	s.complete(first)
	if s.liveSpawns != 1 || s.rootComplete() || s.drained() {
		return false
	}
	events := make(chan uint8, 1)
	again, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			woke := s.waitForWork()
			var state uint8
			if s.rootComplete() {
				state |= 1
			}
			if s.drained() {
				state |= 2
			}
			if woke {
				state |= 4
			}
			events <- state
			if i == 0 {
				<-again
			}
		}
	}()
	if !stacklessCoroWakeReceiverForTest(s.wake) {
		return false
	}
	s.complete(&s.root)
	valid := <-events == 5
	select {
	case <-s.rootDone:
	default:
		valid = false
	}
	close(again)
	if !stacklessCoroWakeReceiverForTest(s.wake) {
		return false
	}
	s.complete(last)
	state := <-events
	valid = valid && state&3 == 3 && (executors || state&4 != 0)
	<-done
	return valid && s.drained() && s.liveSpawns == 0
}
