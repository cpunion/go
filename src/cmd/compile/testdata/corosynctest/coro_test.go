// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package corosynctest

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func sleep(d time.Duration) { time.Sleep(d) }

func send(ch chan int, value int) {
	time.Sleep(time.Second)
	ch <- value
}

func exchange(value int) int {
	ch := make(chan int)
	go send(ch, value)
	result := <-ch
	return result
}

type counter struct{ value int }

func (c counter) result(seed int) (value int) {
	defer func() {
		value += c.value + seed
	}()
	time.Sleep(time.Second)
	value = seed
	return
}

func recursive(depth int) int {
	if depth == 0 {
		time.Sleep(time.Second)
		return 1
	}
	value := recursive(depth - 1)
	return value + 1
}

func variadic(values ...int) int {
	time.Sleep(time.Second)
	return values[0] + values[1]
}

func recoverValue(value *any) {
	time.Sleep(time.Second)
	*value = recover()
}

func indirectRecover() any { return recover() }

func indirectRecoverValue(value *any) {
	time.Sleep(time.Second)
	*value = indirectRecover()
}

func exit(done chan struct{}) {
	defer func() { close(done) }()
	time.Sleep(time.Second)
	runtime.Goexit()
}

func TestContexts(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm"
		}
		t.Run(name, func(t *testing.T) {
			// Integer range keeps the controller ordinary. Only the leaves
			// are compiled with native coroutine entries.
			for range 1 {
				if warm {
					sleep(time.Nanosecond)
				}
				for _, duration := range []time.Duration{42 * time.Second, 7 * time.Hour} {
					synctest.Test(t, func(t *testing.T) {
						start := time.Now()
						sleep(duration)
						if elapsed := time.Since(start); elapsed != duration {
							t.Fatalf("fake sleep = %v, want %v", elapsed, duration)
						}
						if got := exchange(42); got != 42 {
							t.Fatalf("exchange = %d", got)
						}
						var group sync.WaitGroup
						var value int
						group.Go(func() { sleep(time.Second); value = 42 })
						group.Wait()
						if value != 42 {
							t.Fatal("WaitGroup did not complete")
						}
						ch := make(chan int, 1)
						go send(ch, 9)
						synctest.Wait()
						select {
						case <-ch:
							t.Fatal("Wait advanced fake time")
						default:
						}
						sleep(time.Second)
						if got := <-ch; got != 9 {
							t.Fatalf("send = %d", got)
						}
					})
					// Reuse the same entries outside and in a different bubble.
					sleep(time.Nanosecond)
				}
			}
		})
	}
}

func TestSourceSemantics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 1 {
			if got := (counter{3}).result(5); got != 13 {
				t.Fatalf("receiver/parameter/named-result captures = %d", got)
			}
			if got := recursive(32); got != 33 {
				t.Fatalf("recursive result = %d", got)
			}
			if got := variadic(3, 7); got != 10 {
				t.Fatalf("variadic result = %d", got)
			}
			var recovered any
			func() { defer recoverValue(&recovered); panic(42) }()
			if recovered != 42 {
				t.Fatalf("direct recover = %v", recovered)
			}
			var indirect, outer any
			func() {
				defer func() { outer = recover() }()
				defer indirectRecoverValue(&indirect)
				panic(42)
			}()
			if indirect != nil || outer != 42 {
				t.Fatalf("indirect/outer recover = %v/%v", indirect, outer)
			}
			done := make(chan struct{})
			go exit(done)
			<-done
		}
	})
}

func TestCrossBubbleChannel(t *testing.T) {
	if os.Getenv("CORO_CROSS_BUBBLE") == "" {
		t.Skip("subprocess-only fatal case")
	}
	var ch chan int
	synctest.Test(t, func(t *testing.T) { ch = make(chan int, 1) })
	synctest.Test(t, func(t *testing.T) { send(ch, 42) })
}
