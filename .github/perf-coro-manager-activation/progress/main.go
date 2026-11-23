package main

import (
	"runtime"
	"sync/atomic"
)

var expected uint64
var entered, finished atomic.Uint64
var released atomic.Bool

func worker() {
	runtime.Gosched()
	entered.Add(1)
	for !released.Load() {
	}
	finished.Add(1)
}

func parent(workers int) {
	runtime.Gosched()
	for i := 0; i < workers; i++ {
		go worker()
	}
	for entered.Load() != expected {
	}
	released.Store(true)
	for finished.Load() != expected {
	}
}

func main() {
	n := runtime.GOMAXPROCS(0) - 1
	expected = uint64(n)
	parent(n)
}
