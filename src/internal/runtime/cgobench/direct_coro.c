// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

#if defined(__GNUC__)
#define CGOBENCH_NOINLINE __attribute__((noinline))
#else
#define CGOBENCH_NOINLINE
#endif

#include <errno.h>
#include <stdint.h>
#include <time.h>
#include <unistd.h>

CGOBENCH_NOINLINE void coro_cgo_empty(void) {
}

CGOBENCH_NOINLINE void coro_direct_empty(void) {
}

static uint64_t coro_nanotime(void) {
	struct timespec now;
	if (clock_gettime(CLOCK_MONOTONIC, &now) != 0) {
		return UINT64_MAX;
	}
	return (uint64_t)now.tv_sec * 1000000000 + (uint64_t)now.tv_nsec;
}

static uint64_t coro_handoff(int fd, uint32_t *gate, uint32_t epoch) {
	char token = 0;
	ssize_t n;
	uint64_t start = coro_nanotime();
	do {
		n = write(fd, &token, sizeof(token));
	} while (n < 0 && errno == EINTR);
	if (n != sizeof(token)) {
		__atomic_store_n(gate, UINT32_MAX, __ATOMIC_RELEASE);
		return UINT64_MAX;
	}
	while (__atomic_load_n(gate, __ATOMIC_ACQUIRE) < epoch) {
	}
	uint64_t end = coro_nanotime();
	if (start == UINT64_MAX || end == UINT64_MAX) {
		return UINT64_MAX;
	}
	return end - start;
}

static void coro_record_handoff(uint64_t *elapsed, uint64_t ns) {
	if (*elapsed == UINT64_MAX) {
		return;
	}
	if (ns == UINT64_MAX || UINT64_MAX - *elapsed < ns) {
		*elapsed = UINT64_MAX;
		return;
	}
	*elapsed += ns;
}

CGOBENCH_NOINLINE void coro_cgo_handoff(int fd, uint32_t *gate,
		uint32_t epoch, uint64_t *elapsed) {
	coro_record_handoff(elapsed, coro_handoff(fd, gate, epoch));
}

CGOBENCH_NOINLINE void coro_direct_handoff(int fd, uint32_t *gate,
		uint32_t epoch, uint64_t *elapsed) {
	coro_record_handoff(elapsed, coro_handoff(fd, gate, epoch));
}

CGOBENCH_NOINLINE uint64_t coro_add_u64(uint64_t a, uint64_t b) {
	return a + b;
}
