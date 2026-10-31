// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func StacklessCoroRegisteredSlotPromotionForTest() bool {
	previous := GOMAXPROCS(4)
	defer GOMAXPROCS(previous)
	s := newStacklessCoroTestScheduler()
	s.root.state = stacklessCoroTaskWaiting
	s.root.context.scheduler = s
	stop, done := make(chan struct{}), make(chan struct{})
	polls := new(int)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			stacklessCoroPollIOAtIdle(s, nil, nil)
			*polls++
			Gosched()
		}
	}()
	valid := true
	for range 10000 {
		op := new(stacklessCoroOperation)
		op.task = &s.root
		received := new(bool)
		*received = true
		op.setReceived(received)
		id := registerStacklessCoroOperation(op)
		valid = valid && !op.registeredIO
		Gosched()
		// This is the actual registered-channel transition: the result's
		// scanned slot becomes a full waiter/resource header while another
		// executor polls the same registry and scheduler.
		waiter := newStacklessCoroSudog(op, nil)
		Gosched()
		valid = valid && op.receivedResult() == received && *received
		valid = valid && takeStacklessCoroOperation(id) == op
		releaseStacklessCoroSudog(unsafe.Pointer(op), waiter)
		clearStacklessCoroOperation(op)
		valid = valid && !op.registeredIO && op.receivedResult() == nil
	}
	close(stop)
	<-done
	// Real IO storage must still be eligible; a retained IO header is also
	// stable when its next source is a channel. No I/O payload is discarded.
	op := newStacklessCoroIOOperation()
	op.task = &s.root
	id := registerStacklessCoroOperation(op)
	valid = valid && op.registeredIO && op.ioState() != nil
	valid = valid && takeStacklessCoroOperation(id) == op
	clearStacklessCoroOperation(op)
	return valid && *polls != 0
}
