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

func TestLoopControl(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	source := `package main

import "runtime"

func nestedLoops() int {
	sum := 0
	for i := 0; i < 8; i++ {
		for j := 0; j < 5; j++ {
			runtime.Gosched()
			if j == 1 {
				continue
			}
			if j == 3 {
				break
			}
			sum += 10*i + j
		}
		if i == 2 {
			continue
		}
		sum++
		if i == 3 {
			break
		}
	}
	return sum
}

func advance(i *int) {
	runtime.Gosched()
	*i++
}

func loopPost() int {
	i, sum := 0, 0
	for ; i < 6; advance(&i) {
		runtime.Gosched()
		if i == 1 {
			continue
		}
		if i == 4 {
			break
		}
		sum += i
	}
	return i*10 + sum
}

func selectBranches(ch chan int) int {
	sum := 0
	for i := 0; i < 4; i++ {
		select {
		case <-ch:
			if i == 1 {
				continue
			}
			if i == 2 {
				break
			}
			sum++
		default:
			panic("channel empty")
		}
		sum += 10
	}
	return sum
}

func defaultBranches() int {
	sum := 0
	for i := 0; i < 3; i++ {
		select {
		default:
			if i == 1 {
				continue
			}
			select {
			default:
				runtime.Gosched()
				break
			}
			sum++
			break
		}
		sum += 10
	}
	return sum
}

func rangeBranches(ch chan *int) int {
	sum := 0
	for value := range ch {
		if *value == 1 {
			continue
		}
		if *value == 3 {
			break
		}
		sum += *value
	}
	return sum + 10*len(ch)
}

var trace int

func record(value int) {
	trace = trace*10 + value
}

func deferredLoop() int {
	defer record(9)
	for i := 0; i < 5; i++ {
		defer record(i + 1)
		runtime.Gosched()
		if i == 0 {
			continue
		}
		if i == 2 {
			break
		}
	}
	return 7
}

func terminalLoop() {
	defer record(8)
	for i := 0; ; i++ {
		runtime.Gosched()
		if i == 0 {
			continue
		}
		break
	}
	panic("terminal-loop")
}

func returnLoop() int {
	defer record(4)
	for i := 0; i < 3; i++ {
		runtime.Gosched()
		if i == 0 {
			continue
		}
		return i
	}
	return -1
}

var exited = make(chan int)

func finishExit() {
	close(exited)
}

func exitLoop() {
	defer finishExit()
	for i := 0; ; i++ {
		runtime.Gosched()
		if i == 0 {
			continue
		}
		break
	}
	runtime.Goexit()
}

func checkPanic() {
	defer func() {
		if recover() != "terminal-loop" {
			panic("wrong panic")
		}
	}()
	terminalLoop()
}

func labeledLoop() int {
	i := 0
outer:
	for {
		runtime.Gosched()
		i++
		if i < 3 {
			continue outer
		}
		break outer
	}
	return i
}

func gotoLoop() int {
	i := 0
again:
	runtime.Gosched()
	i++
	if i < 3 {
		goto again
	}
	return i
}

func switchContinue() int {
	sum := 0
	for i := 0; i < 3; i++ {
		runtime.Gosched()
		switch i {
		case 1:
			continue
		}
		sum += i
	}
	return sum
}

func main() {
	if nestedLoops() != 131 {
		panic("nested loop targets")
	}
	if loopPost() != 45 {
		panic("loop post")
	}
	ch := make(chan int, 4)
	for i := 0; i < 4; i++ {
		ch <- i
	}
	if selectBranches(ch) != 32 {
		panic("select targets")
	}
	if defaultBranches() != 22 {
		panic("default select targets")
	}
	values := make(chan *int, 4)
	for i := 1; i <= 4; i++ {
		value := new(int)
		*value = i
		values <- value
	}
	if rangeBranches(values) != 12 {
		panic("channel range targets")
	}
	if deferredLoop() != 7 || trace != 3219 {
		panic("loop defer order")
	}
	trace = 0
	checkPanic()
	if trace != 8 {
		panic("terminal loop defer")
	}
	trace = 0
	if returnLoop() != 1 || trace != 4 {
		panic("return loop defer")
	}
	go exitLoop()
	<-exited
	if labeledLoop() != 3 || gotoLoop() != 3 || switchContinue() != 2 {
		panic("ordinary fallback")
	}
	println("loop-control-ok")
}
`
	file := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(file, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, experiment, flags string
		checkLowering           bool
	}{
		{"coro/inline", "coro", "-d=coro=4", false},
		{"coro/noinline", "coro", "-l -d=coro=4", true},
		{"nocoro/inline", "nocoro", "", false},
		{"nocoro/noinline", "nocoro", "-l", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "loopcontrol.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+test.flags, "-o", exe, file)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			data, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, data)
			}
			if test.checkLowering {
				for _, function := range []string{"labeledLoop", "gotoLoop"} {
					if !strings.Contains(string(data), "coro: skip main."+function+":") {
						t.Errorf("missing ordinary-entry fallback for %s\n%s", function, data)
					}
				}
			}
			cmd = testenv.Command(t, exe)
			data, err = cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(data)) != "loop-control-ok" {
				t.Fatalf("execution = (%v, %q), want loop-control-ok", err, data)
			}
			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			data, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("nm failed: %v\n%s", err, data)
			}
			for _, function := range []string{
				"nestedLoops", "advance", "loopPost", "selectBranches",
				"defaultBranches", "rangeBranches", "deferredLoop", "terminalLoop",
				"returnLoop", "exitLoop", "switchContinue",
			} {
				if test.experiment == "coro" && !test.checkLowering {
					continue
				}
				got := strings.Contains(string(data), "main."+function+".coro")
				if want := test.checkLowering; got != want {
					t.Errorf("%s has coroutine factory = %v, want %v", function, got, want)
				}
			}
			for _, function := range []string{"labeledLoop", "gotoLoop"} {
				if strings.Contains(string(data), "main."+function+".coro") {
					t.Errorf("unsupported control flow has a coroutine factory: %s", function)
				}
			}
		})
	}
}
