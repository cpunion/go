// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"testing"
	"testing/synctest"
	"time"
)

func TestStacklessCoroSynctestContext(t *testing.T) {
	if runtime.StacklessCoroUseOrdinaryForTest() {
		t.Fatal("ordinary goroutine is in a bubble")
	}
	for range 2 {
		synctest.Test(t, func(t *testing.T) {
			if !runtime.StacklessCoroUseOrdinaryForTest() {
				t.Fatal("bubble goroutine selected a native entry")
			}
			checked := false
			go func() {
				if !runtime.StacklessCoroUseOrdinaryForTest() {
					t.Error("child goroutine lost bubble context")
				}
				checked = true
			}()
			synctest.Wait()
			if !checked {
				t.Fatal("child did not run")
			}
			time.Sleep(time.Hour)
			if !runtime.StacklessCoroUseOrdinaryForTest() {
				t.Fatal("timer wakeup lost bubble context")
			}
		})
		if runtime.StacklessCoroUseOrdinaryForTest() {
			t.Fatal("bubble context leaked after Test")
		}
	}
}

func TestStacklessCoroLockedThreadContext(t *testing.T) {
	if runtime.StacklessCoroUseOrdinaryForTest() {
		t.Fatal("ordinary goroutine selected an ordinary-only entry")
	}
	for range 2 {
		runtime.LockOSThread()
		runtime.LockOSThread()
		if !runtime.StacklessCoroUseOrdinaryForTest() {
			t.Error("locked goroutine selected a native entry")
		}
		runtime.Gosched()
		runtime.UnlockOSThread()
		if !runtime.StacklessCoroUseOrdinaryForTest() {
			t.Error("nested thread lock was lost")
		}
		runtime.UnlockOSThread()
		if runtime.StacklessCoroUseOrdinaryForTest() {
			t.Error("thread lock leaked after UnlockOSThread")
		}
	}
}
