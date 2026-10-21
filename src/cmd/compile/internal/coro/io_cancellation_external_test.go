// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// TestIOCancellation exercises public file reads across cancellation and
// deadline-reset reuse, with one and four simultaneous native scheduler roots.
// Factories and bounded ordinary goroutine counts establish native execution;
// the nocoro controls exercise the same source through ordinary Go execution.
func TestIOCancellation(t *testing.T) {
	const source = `package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

var native string

type reply struct {
	index int
	n     int
	err   error
}

func readPipe(index int, file *os.File, buffer []byte, ready chan int, out chan reply, done chan struct{}) {
	ready <- index
	n, err := file.Read(buffer)
	out <- reply{index, n, err}
	done <- struct{}{}
}

func readBatch(files []*os.File, buffers [][]byte, ready chan int, out chan reply) {
	done := make(chan struct{}, len(files))
	for i := 0; i < len(files); i++ {
		go readPipe(i, files[i], buffers[i], ready, out, done)
	}
	for i := 0; i < len(files); i++ {
		<-done
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func batch(round int, deadline bool) {
	const count = 128
	readers := make([]*os.File, count)
	writers := make([]*os.File, count)
	buffers := make([][]byte, count)
	for i := range readers {
		var err error
		readers[i], writers[i], err = os.Pipe()
		must(err)
		buffers[i] = make([]byte, 1)
	}
	ready := make(chan int, count)
	out := make(chan reply, count)
	finished := make(chan struct{})
	before := runtime.NumGoroutine()
	go func() { readBatch(readers, buffers, ready, out); close(finished) }()
	seen := make([]bool, count)
	for range count {
		select {
		case i := <-ready:
			if i < 0 || i >= count || seen[i] {
				panic("duplicate readiness")
			}
			seen[i] = true
		case <-time.After(5 * time.Second):
			panic("readiness stalled")
		}
	}
	// This is a blocked-read checkpoint, not proof that every poll waiter
	// has been installed. Both arming/cancellation orders remain valid.
	time.Sleep(5 * time.Millisecond)
	if len(out) != 0 {
		panic("empty pipe read completed before cancellation")
	}
	if native == "1" && runtime.NumGoroutine()-before > 24 {
		panic(fmt.Sprintf("native batch allocated ordinary workers: before=%d during=%d", before, runtime.NumGoroutine()))
	}
	runtime.GC()
	for i, file := range readers {
		if deadline {
			must(file.SetReadDeadline(time.Now().Add(-time.Second)))
		} else {
			must(file.Close())
		}
		if (i+round)%17 == 0 {
			runtime.Gosched()
		}
	}
	for range count {
		select {
		case got := <-out:
			if got.index < 0 || got.index >= count || !seen[got.index] {
				panic("duplicate cancelled completion")
			}
			seen[got.index] = false
			want := os.ErrClosed
			if deadline {
				want = os.ErrDeadlineExceeded
			}
			if got.n != 0 || !errors.Is(got.err, want) {
				panic(fmt.Sprintf("cancelled read: %+v want %v", got, want))
			}
			var pathErr *os.PathError
			if !errors.As(got.err, &pathErr) || pathErr.Op != "read" || pathErr.Path != readers[got.index].Name() {
				panic("lost PathError")
			}
		case <-time.After(5 * time.Second):
			panic("cancelled read stalled")
		}
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		panic("root did not finish")
	}
	if deadline {
		// Reuse the same descriptor after cancellation and an explicit
		// deadline reset. A stale completion must not cancel this new read.
		for _, file := range readers {
			must(file.SetReadDeadline(time.Time{}))
		}
		finished = make(chan struct{})
		go func() { readBatch(readers, buffers, ready, out); close(finished) }()
		for range count {
			select {
			case i := <-ready:
				if i < 0 || i >= count || seen[i] {
					panic("duplicate reuse readiness")
				}
				seen[i] = true
			case <-time.After(5 * time.Second):
				panic("reuse readiness stalled")
			}
		}
		for i, file := range writers {
			n, err := file.Write([]byte{byte(i + round)})
			must(err)
			if n != 1 {
				panic("short release write")
			}
		}
		for range count {
			select {
			case got := <-out:
				if got.index < 0 || got.index >= count || !seen[got.index] {
					panic("duplicate reuse completion")
				}
				seen[got.index] = false
				if got.n != 1 || got.err != nil || buffers[got.index][0] != byte(got.index+round) {
					panic(fmt.Sprintf("reused read: %+v", got))
				}
			case <-time.After(5 * time.Second):
				panic("reused read stalled")
			}
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			panic("reused root did not finish")
		}
	}
	for i := range readers {
		readers[i].Close()
		writers[i].Close()
	}
}

func main() {
 if info,ok:=debug.ReadBuildInfo();ok {
  experiment, flags := "", ""
  for _,setting:=range info.Settings {
   if setting.Key=="GOEXPERIMENT" {experiment=setting.Value}
   if setting.Key=="-gcflags" {flags=setting.Value}
  }
  if experiment=="coro" && strings.Contains(flags,"-d=coro=4") {native="1"}
 }
 for _,procs:=range []int{1,4} {
  runtime.GOMAXPROCS(procs)
  for _,roots:=range []int{1,4} {
   done:=make(chan struct{},roots)
   for worker:=0;worker<roots;worker++ {
    go func(id int) {
     for round:=0;round<4;round++ {batch(round*4+id,false);batch(round*4+id,true)}
     done<-struct{}{}
    }(worker)
   }
   for i:=0;i<roots;i++ {<-done}
  }
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"readPipe", "readBatch"})
}
