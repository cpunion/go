// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package corobench

import (
	"runtime"
	"sync/atomic"
)

var packedGate chan struct{}
var packedReady, packedRelease, packedDone uint64
var paddingValue = 5

func packedWorker(a bool, p *int, b bool, q *int, c bool, n int) {
	atomic.AddUint64(&packedReady, 1)
	<-packedGate
	result := *p + *q + n
	if a {
		result++
	}
	if b {
		result += 2
	}
	if c {
		result += 4
	}
	atomic.AddUint64(&packedDone, uint64(result))
}
func packedTasks(rounds, tasks int, hold bool) uint64 {
	var total uint64
	value := 5
	for round := 0; round < rounds; round++ {
		packedGate = make(chan struct{})
		atomic.StoreUint64(&packedReady, 0)
		atomic.StoreUint64(&packedRelease, 0)
		atomic.StoreUint64(&packedDone, 0)
		for i := 0; i < tasks; i++ {
			go packedWorker(true, &value, true, &value, true, 3)
		}
		for atomic.LoadUint64(&packedReady) < uint64(tasks) {
			runtime.Gosched()
		}
		if hold {
			for atomic.LoadUint64(&packedRelease) == 0 {
				runtime.Gosched()
			}
		}
		close(packedGate)
		for atomic.LoadUint64(&packedDone) < uint64(tasks*20) {
			runtime.Gosched()
		}
		total += atomic.LoadUint64(&packedDone)
	}
	return total
}

func packedSelf(a bool, p *int, b bool, q *int, c bool, n int) int {
	if n == 0 {
		runtime.Gosched()
		return *p + *q
	}
	child := packedSelf(a, p, b, q, c, n-1)
	if a {
		child++
	}
	if b {
		child += 2
	}
	if c {
		child += 4
	}
	return child + *p + *q
}

func packedMutualA(a bool, p *int, b bool, q *int, c bool, n int) int {
	if n == 0 {
		runtime.Gosched()
		return *p + *q
	}
	child := packedMutualB(a, p, b, q, c, n-1)
	if a {
		child++
	}
	if b {
		child += 2
	}
	if c {
		child += 4
	}
	return child + *p + *q
}

func packedMutualB(a bool, p *int, b bool, q *int, c bool, n int) int {
	if n == 0 {
		runtime.Gosched()
		return *p + *q
	}
	child := packedMutualA(a, p, b, q, c, n-1)
	if a {
		child++
	}
	if b {
		child += 2
	}
	if c {
		child += 4
	}
	return child + *p + *q
}

//go:noinline
func packedConsume432(p *[432]byte) int { return int(p[0]) }
func packedBoundary432A(a uint16, b uint8, p *int, n int) int {
	var data [432]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return int(a) + int(b) + *p + packedConsume432(&data)
	}
	child := packedBoundary432B(a, b, p, n-1)
	return child + int(a) + int(b) + *p + packedConsume432(&data)
}
func packedBoundary432B(a uint16, b uint8, p *int, n int) int {
	var data [432]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return int(a) + int(b) + *p + packedConsume432(&data)
	}
	child := packedBoundary432A(a, b, p, n-1)
	return child + int(a) + int(b) + *p + packedConsume432(&data)
}

//go:noinline
func packedConsume440(p *[440]byte) int { return int(p[0]) }
func packedBoundary440A(a uint16, b uint8, p *int, n int) int {
	var data [440]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return int(a) + int(b) + *p + packedConsume440(&data)
	}
	child := packedBoundary440B(a, b, p, n-1)
	return child + int(a) + int(b) + *p + packedConsume440(&data)
}
func packedBoundary440B(a uint16, b uint8, p *int, n int) int {
	var data [440]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return int(a) + int(b) + *p + packedConsume440(&data)
	}
	child := packedBoundary440A(a, b, p, n-1)
	return child + int(a) + int(b) + *p + packedConsume440(&data)
}

//go:noinline
func packedConsume448(p *[448]byte) int { return int(p[0]) }
func packedBoundary448A(a uint16, b uint8, p *int, n int) int {
	var data [448]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return int(a) + int(b) + *p + packedConsume448(&data)
	}
	child := packedBoundary448B(a, b, p, n-1)
	return child + int(a) + int(b) + *p + packedConsume448(&data)
}
func packedBoundary448B(a uint16, b uint8, p *int, n int) int {
	var data [448]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return int(a) + int(b) + *p + packedConsume448(&data)
	}
	child := packedBoundary448A(a, b, p, n-1)
	return child + int(a) + int(b) + *p + packedConsume448(&data)
}
