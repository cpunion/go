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

const bufferedExchangeSource = `package main

import "runtime"

type verdict bool

func exchange(ch chan int, value int) (int, bool) {
	ch <- value
	result, ok := <-ch
	return result, ok
}

func exchangeAdjusted(ch chan int, value int) int64 {
	ch <- value
	received := int64(<-ch)
	return received + 1
}

func exchangeOK(ch chan int, value int) bool {
	ch <- value
	_, ok := <-ch
	return ok
}

func pointerExchange(ch chan *int, value *int) (any, verdict) {
	var result any
	var ok verdict
	ch <- value
	result, ok = <-ch
	return result, ok
}

func exchangeOnce(ch chan *int, value *int) *int {
	ch <- value
	result := <-ch
	return result
}

func differentChannels(send, receive chan int, value int) int {
	send <- value
	result := <-receive
	return result
}

func replace(ch *chan int, target chan int) int {
	*ch = target
	return 31
}

func evaluatedChannel(ch, target chan int) int {
	original := ch
	original <- replace(&ch, target)
	result := <-ch
	return result
}

func check(ok bool) {
	if !ok {
		panic("unexpected buffered exchange result")
	}
}

func checkClosed(ch chan int) {
	defer func() {
		check(recover() != nil)
	}()
	exchange(ch, 3)
}

func checkReady() {
	ch := make(chan int, 4)
	for i := range 17 {
		value, ok := exchange(ch, i)
		check(ok && value == i && len(ch) == 0)
	}
	ch <- 41
	ch <- 43
	want := [2]int{41,43}
	for i := range 20 {
		value, ok := exchange(ch, 100+i)
		check(ok && value == want[i%2] && len(ch) == 2)
		want[i%2] = 100+i
	}
	check(<-ch == want[0] && <-ch == want[1])
	check(exchangeAdjusted(ch, 71) == 72 && len(ch) == 0)
	check(exchangeOK(ch, 73) && len(ch) == 0)

	left, right := new(int), new(int)
	*left, *right = 57, 59
	pointers := make(chan *int, 2)
	check(exchangeOnce(pointers, left) == left && len(pointers) == 0)
	result, ok := pointerExchange(pointers, left)
	check(bool(ok) && result.(*int) == left && len(pointers) == 0)
	pointers <- left
	result, ok = pointerExchange(pointers, right)
	runtime.GC()
	check(bool(ok) && *result.(*int) == 57 && *(<-pointers) == 59)
}

func main() {
	checkReady()
	runtime.LockOSThread()
	checkReady()
	runtime.UnlockOSThread()

	ch := make(chan int)
	go func() {
		value := <-ch
		ch <- value + 100
	}()
	value, ok := exchange(ch, 9)
	check(ok && value == 109)

	ptrs := make(chan *int)
	sent, answer := 61, 67
	go func() {
		check(<-ptrs == &sent)
		ptrs <- &answer
	}()
	check(exchangeOnce(ptrs, &sent) == &answer)

	full := make(chan int, 1)
	full <- 7
	done := make(chan int, 1)
	go func() { done <- <-full }()
	value, ok = exchange(full, 8)
	check(ok && value == 8 && <-done == 7)
	close(full)
	checkClosed(full)

	send, receive := make(chan int, 1), make(chan int, 1)
	receive <- 23
	check(differentChannels(send, receive, 19) == 23 && <-send == 19)
	receive <- 37
	check(evaluatedChannel(send, receive) == 37 && <-send == 31)
	println("switch-return-ok")
}
`

func TestBufferedChannelExchange(t *testing.T) {
	testSwitchReturnFilesNative(t, map[string]string{"main.go": bufferedExchangeSource}, "", nil,
		[]string{"exchange", "pointerExchange", "differentChannels"}, []string{"exchange", "pointerExchange"})

	file := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(file, []byte(bufferedExchangeSource), 0o666); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "buffered-exchange.exe")
	goTool := testenv.GoToolPath(t)
	build := testenv.Command(t, goTool, "build", "-gcflags=-l -d=coro=4", "-o", program, file)
	build.Env = append(build.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, name := range []string{"exchange", "pointerExchange", "differentChannels"} {
		symbol := "^main[.]" + name + "[.]coro[.]func[0-9]+$"
		out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", symbol, program).CombinedOutput()
		if err != nil || len(out) == 0 {
			t.Fatalf("missing %s: %v\n%s", name, err, out)
		}
		if got, want := strings.Contains(string(out), "runtime.coroTryChanExchange"), name != "differentChannels"; got != want {
			t.Fatalf("%s calls buffered exchange=%t, want %t\n%s", name, got, want, out)
		}
	}
	for _, name := range []string{"exchangeOnce", "exchange", "exchangeAdjusted", "exchangeOK", "pointerExchange"} {
		symbol := "^main[.]" + name + "$"
		out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", symbol, program).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "runtime.coroTryChanExchange") ||
			!strings.Contains(string(out), "runtime.coroRunFrame") {
			t.Fatalf("%s has no ready exchange or fallback: %v\n%s", name, err, out)
		}
	}
}

func TestPublicBufferedChannelExchangeMiss(t *testing.T) {
	testSwitchReturnFilesNative(t, map[string]string{
		"go.mod": "module exchangecheck\n\ngo 1.27\n",
		"pair/pair.go": `package pair

func Exchange(ch chan *int, sent *int) (*int, bool) {
	ch <- sent
	value, ok := <-ch
	return value, ok
}
`,
		"main.go": `package main

import (
	"exchangecheck/pair"
	"runtime"
)

func checkBlocking(capacity int) {
	ch := make(chan *int, capacity)
	sent, initial := 71, 83
	if capacity != 0 {
		ch <- &initial
	}
	done := make(chan struct{})
	go func() {
		value := <-ch
		runtime.GC()
		if capacity == 0 {
			ch <- value
		} else if value != &initial {
			panic("buffer lost its oldest item")
		}
		close(done)
	}()
	value, ok := pair.Exchange(ch, &sent)
	<-done
	if !ok || value != &sent || *value != 71 {
		panic("blocking exchange changed the value")
	}
}

func checkClosedReceive() {
	ch := make(chan *int)
	sent := 79
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	go func() {
		<-ch
		runtime.GC()
		close(ch)
	}()
	value, ok := pair.Exchange(ch, &sent)
	if value != nil || ok {
		panic("closed channel returned a value")
	}
	defer func() {
		if recover() == nil {
			panic("send on a closed channel returned")
		}
	}()
	pair.Exchange(ch, &sent)
}

func main() {
	for _, procs := range []int{1, 4} {
		runtime.GOMAXPROCS(procs)
		checkBlocking(0)
		checkBlocking(1)
		checkClosedReceive()
	}
	println("switch-return-ok")
}
`,
	}, "exchangecheck/pair=", nil, nil, nil)
}
