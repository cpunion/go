// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

#if defined(__GNUC__)
#define CGOBENCH_NOINLINE __attribute__((noinline))
#else
#define CGOBENCH_NOINLINE
#endif

#include <stdint.h>

CGOBENCH_NOINLINE void coro_cgo_empty(void) {
}

CGOBENCH_NOINLINE void coro_direct_empty(void) {
}

CGOBENCH_NOINLINE uint64_t coro_add_u64(uint64_t a, uint64_t b) {
	return a + b;
}
