// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

#include <stdint.h>
#include <pthread.h>

extern uint64_t GoCallback(uintptr_t);

uint64_t
call_callback(void)
{
	return GoCallback((uintptr_t)pthread_self());
}
