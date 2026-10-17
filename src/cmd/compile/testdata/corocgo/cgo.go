// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corocgo

/*
#include <stdint.h>
#include <pthread.h>

#cgo nocallback thread_id
#cgo noescape thread_id
#cgo nocallback add
#cgo noescape add
#cgo nocallback read_pointer
#cgo noescape read_pointer

static uintptr_t thread_id(void) { return (uintptr_t)pthread_self(); }
static uint64_t add(uint64_t x, uint64_t y) { return x + y; }
static uint64_t read_pointer(void *p) { return **(uint64_t **)p; }

uint64_t call_callback(uintptr_t);
*/
import "C"

import (
	"runtime"
	"unsafe"
)

func threadID() uintptr { return uintptr(C.thread_id()) }

//go:noinline
func scalar(value uint64) uint64 {
	return uint64(C.add(C.uint64_t(value), 1))
}

// locked is called both with and without a thread lock inherited from Go or
// a C callback. Its public entry must choose the body before entering C.
//
//go:noinline
func locked(want uintptr) uint64 {
	if threadID() != want {
		panic("thread changed before yield")
	}
	runtime.Gosched()
	if threadID() != want {
		panic("thread changed after yield")
	}
	return scalar(41)
}

func callback() uint64 { return uint64(C.call_callback(C.uintptr_t(threadID()))) }

//export GoCallback
func GoCallback(want C.uintptr_t) C.uint64_t {
	return C.uint64_t(locked(uintptr(want)))
}

//go:noinline
func pointer(value **uint64) uint64 {
	return uint64(C.read_pointer(unsafe.Pointer(value)))
}
