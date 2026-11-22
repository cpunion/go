// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const privateChannelSource = `package main

import "runtime"

type payload struct { pointer *int }
type verdict bool

func privatePairs(n int, p *int) int {
	values := make(chan payload, 3)
	offsets := make(chan int, 1)
	total := 0
	for i := 0; i < n; i++ {
		values <- payload{p}
		var value any
		var ok verdict
		value, ok = <-values
		if !ok { panic("closed private channel") }
		offsets <- i
		offset := <-offsets
		total += *value.(payload).pointer + offset
		values <- payload{nil}
		<-values
	}
	return total
}

var order int
func sent() int { order = order*10 + 1; return 23 }
func index() int { order = order*10 + 2; return 0 }

func privateAssignment() int {
	channel := make(chan int, 1)
	var values [1]int
	channel <- sent()
	values[index()] = <-channel
	channel <- values[0]
	return <-channel
}

func mixed(p *int) int {
	channel := make(chan *int, 1)
	channel <- p
	value := <-channel
	runtime.Gosched()
	return *value
}

func observed() int {
	channel := make(chan int, 1)
	channel <- 14
	length := len(channel)
	value := <-channel
	channel <- 19
	return value + length + <-channel
}

func captured() int {
	channel := make(chan int, 1)
	observe := func() int { return cap(channel) }
	channel <- 33
	value := <-channel
	return value + observe()
}

func main() {
	value := 7
	if privatePairs(19, &value) != 304 || privateAssignment() != 23 || order != 12 {
		panic("private pair results or evaluation order changed")
	}
	if mixed(&value) != 7 || observed() != 34 || captured() != 34 {
		panic("ordinary use or capture changed")
	}
	runtime.LockOSThread()
	if mixed(&value) != 7 {
		panic("mixed ordinary fallback changed")
	}
	runtime.UnlockOSThread()
	println("switch-return-ok")
}
`

func TestPrivateBufferedChannelPairs(t *testing.T) {
	testSwitchReturnFilesNative(t, map[string]string{"main.go": privateChannelSource}, "", nil,
		[]string{"mixed", "observed", "captured"}, []string{"mixed", "observed"})

	file := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(file, []byte(privateChannelSource), 0o666); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "private-channels.exe")
	goTool := testenv.GoToolPath(t)
	build := testenv.Command(t, goTool, "build", "-gcflags=-l -d=coro=4", "-o", program, file)
	build.Env = append(build.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, name := range []string{"privatePairs", "privateAssignment"} {
		out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", "^main[.]"+name+"$", program).CombinedOutput()
		if err != nil || len(out) == 0 {
			t.Fatalf("missing %s: %v\n%s", name, err, out)
		}
		for _, call := range []string{"runtime.chansend", "runtime.chanrecv", "runtime.coroChan", "runtime.coroNative"} {
			if strings.Contains(string(out), call) {
				t.Fatalf("%s retained %s\n%s", name, call, out)
			}
		}
	}
}
