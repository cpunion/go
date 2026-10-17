// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corocgo

import (
	"runtime"
	"strings"
	"testing"
)

func TestScalar(t *testing.T) {
	if got := scalar(41); got != 42 {
		t.Fatalf("scalar = %d, want 42", got)
	}
}

func TestLockedThread(t *testing.T) {
	for range 10 {
		runtime.LockOSThread()
		runtime.LockOSThread()
		want := threadID()
		got := locked(want)
		runtime.UnlockOSThread()
		if got != 42 || threadID() != want {
			t.Error("inherited thread lock was lost")
		}
		got = locked(want)
		runtime.UnlockOSThread()
		if got != 42 {
			t.Errorf("locked = %d, want 42", got)
		}
		if got := scalar(41); got != 42 {
			t.Errorf("scalar after unlock = %d, want 42", got)
		}
	}
}

func TestCallback(t *testing.T) {
	for range 10 {
		if got := callback(); got != 42 {
			t.Fatalf("callback = %d, want 42", got)
		}
	}
}

var pointerRoot **uint64

func TestPointerCheck(t *testing.T) {
	value := uint64(42)
	p := &value
	// Force both objects onto the heap: the runtime does not inspect the
	// contents of unknown stack pointers in cgoCheckUnknownPointer.
	pointerRoot = &p
	defer func() { pointerRoot = nil }()
	defer func() {
		got := recover()
		if got == nil || !strings.Contains(got.(error).Error(), "Go pointer to unpinned Go pointer") {
			t.Errorf("unpinned pointer panic = %v", got)
		}
	}()
	pointer(&p)
}

func TestPinnedPointer(t *testing.T) {
	value := uint64(42)
	p := &value
	pointerRoot = &p
	defer func() { pointerRoot = nil }()
	var pins runtime.Pinner
	pins.Pin(p)
	defer pins.Unpin()
	if got := pointer(&p); got != 42 {
		t.Fatalf("pinned pointer = %d, want 42", got)
	}
	runtime.GC()
	runtime.KeepAlive(p)
}
