// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
	"time"
)

func TestStacklessCoroDirectLinkLayoutAndTagging(t *testing.T) {
	taskSize, chunkSize, is64Bit := runtime.CheckStacklessCoroDirectLinkLayoutForTest()
	if is64Bit {
		if taskSize != 48 {
			t.Fatalf("sizeof(stacklessCoroTask) = %d, want 48", taskSize)
		}
		if chunkSize != 208 {
			t.Fatalf("sizeof(stacklessCoroScalarTaskChunk) = %d, want 208", chunkSize)
		}
	}
	if !runtime.CheckStacklessCoroDirectLinkTaggingForTest() {
		t.Fatal("direct link parent/source tagging failed")
	}
	if !runtime.CheckStacklessCoroDirectLinkLifecycleForTest() {
		t.Fatal("direct link lifecycle failed")
	}
}

func TestStacklessCoroDirectLinkRecordRecycling(t *testing.T) {
	if err := runtime.CheckStacklessCoroDirectLinkRecordRecyclingForTest(); err != "" {
		t.Fatalf("direct link record recycling failed: %s", err)
	}
}

func TestStacklessCoroDirectLinkGCLiveness(t *testing.T) {
	h, wantGoid := runtime.NewStacklessCoroDirectLinkGCHandleForTest()
	for range 4 {
		runtime.GC()
	}
	if runtime.StacklessCoroDirectLinkGCFreedForTest() {
		t.Fatal("source record collected while retained via interior +1 tag")
	}
	if got := h.SourceGoid(); got != wantGoid {
		t.Fatalf("SourceGoid() = %#x, want %#x", got, wantGoid)
	}
	if !h.ClearSource() {
		t.Fatal("ClearSource did not clear parentOrSource to nil")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !runtime.StacklessCoroDirectLinkGCFreedForTest() && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if !runtime.StacklessCoroDirectLinkGCFreedForTest() {
		t.Fatal("source record was not collected after ClearSource")
	}
	runtime.KeepAlive(h)
}
