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
#include <math.h>
#include <stdint.h>
#include <time.h>
#include <unistd.h>

CGOBENCH_NOINLINE void coro_cgo_empty(void) {
}

CGOBENCH_NOINLINE void coro_direct_empty(void) {
}

CGOBENCH_NOINLINE void coro_cgo_sin_add(double value, double *sum) {
	*sum += sin(value);
}

CGOBENCH_NOINLINE void coro_direct_sin_add(double value, double *sum) {
	*sum += sin(value);
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

static uint64_t coro_runnable_handoff(uint64_t *entered, uint64_t epoch,
		uint64_t *gate) {
	uint64_t start = coro_nanotime();
	if (start == UINT64_MAX) {
		return UINT64_MAX;
	}
	__atomic_store_n(entered, epoch, __ATOMIC_RELEASE);
	uint64_t spins = 0;
	while (__atomic_load_n(gate, __ATOMIC_ACQUIRE) < epoch) {
		if ((++spins & ((UINT64_C(1) << 20) - 1)) == 0) {
			uint64_t now = coro_nanotime();
			if (now == UINT64_MAX || now - start >= 5000000000) {
				__atomic_store_n(entered, UINT64_MAX, __ATOMIC_RELEASE);
				return UINT64_MAX;
			}
		}
	}
	uint64_t end = coro_nanotime();
	if (end == UINT64_MAX) {
		return UINT64_MAX;
	}
	return end - start;
}

CGOBENCH_NOINLINE void coro_cgo_runnable_handoff(uint64_t *entered,
		uint64_t epoch, uint64_t *gate, uint64_t *elapsed) {
	if (*elapsed == UINT64_MAX) {
		return;
	}
	coro_record_handoff(elapsed,
			coro_runnable_handoff(entered, epoch, gate));
}

CGOBENCH_NOINLINE void coro_direct_runnable_handoff(uint64_t *entered,
		uint64_t epoch, uint64_t *gate, uint64_t *elapsed) {
	if (*elapsed == UINT64_MAX) {
		return;
	}
	coro_record_handoff(elapsed,
			coro_runnable_handoff(entered, epoch, gate));
}

CGOBENCH_NOINLINE uint64_t coro_add_u64(uint64_t a, uint64_t b) {
	return a + b;
}
