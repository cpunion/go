// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include <errno.h>
#include <math.h>
#include <stdint.h>
#include <time.h>

#define COROBENCH_NOINLINE __attribute__((noinline))

typedef struct {
	uint64_t integer;
	double floating;
} probe_pair;

COROBENCH_NOINLINE uint64_t probe_scalar(uint64_t value) {
	return value + 1;
}

COROBENCH_NOINLINE probe_pair probe_pair_add(probe_pair value, probe_pair step) {
	probe_pair result = {
		value.integer + step.integer,
		value.floating + step.floating,
	};
	return result;
}

COROBENCH_NOINLINE int64_t probe_errno(int64_t value) {
	errno = 0;
	return value + 1;
}

COROBENCH_NOINLINE double probe_sin(double value) {
	return sin(value);
}

static uint64_t probe_nanotime(void) {
	struct timespec now;
	if (clock_gettime(CLOCK_MONOTONIC, &now) != 0) {
		return 0;
	}
	return (uint64_t)now.tv_sec * 1000000000ULL + (uint64_t)now.tv_nsec;
}

COROBENCH_NOINLINE uint64_t probe_block(
	uint64_t epoch, uint64_t *entered, uint64_t *gate) {
	__atomic_store_n(entered, epoch, __ATOMIC_RELEASE);
	uint64_t start = probe_nanotime();
	while (__atomic_load_n(gate, __ATOMIC_ACQUIRE) < epoch) {
	}
	uint64_t end = probe_nanotime();
	return end >= start ? end - start : 0;
}

COROBENCH_NOINLINE uint64_t probe_block_group(
	uint64_t epoch, uint64_t *entered, uint64_t *release,
	uint64_t timeout_ns) {
	__atomic_add_fetch(entered, 1, __ATOMIC_RELEASE);
	uint64_t start = probe_nanotime();
	if (start == 0) {
		return 0;
	}
	const struct timespec pause = {0, 50000};
	while (__atomic_load_n(release, __ATOMIC_ACQUIRE) < epoch) {
		uint64_t now = probe_nanotime();
		if (now == 0 || now - start >= timeout_ns) {
			return 0;
		}
		nanosleep(&pause, 0);
	}
	return 1;
}
