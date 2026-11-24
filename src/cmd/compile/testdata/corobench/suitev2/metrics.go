// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"math"
	"runtime/metrics"
	"slices"
	"testing"
)

var (
	sinkUint64 uint64
	sinkInt    int
)

func unhostedCoroCount() uint64 {
	var samples [1]metrics.Sample
	samples[0].Name = "/sched/coro/goroutines/unhosted:goroutines"
	metrics.Read(samples[:])
	if samples[0].Value.Kind() == metrics.KindUint64 {
		return samples[0].Value.Uint64()
	}
	return 0
}

func percentileNS(samples []int64, q float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]int64, len(samples))
	copy(cp, samples)
	slices.Sort(cp)
	pos := q * float64(len(cp)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return float64(cp[lo])
	}
	frac := pos - float64(lo)
	return float64(cp[lo])*(1-frac) + float64(cp[hi])*frac
}

func reportBenchMetrics(b *testing.B, latencies []int64, logicalOps int, rssBefore, rssAfter, rssPeak uint64) {
	b.Helper()
	if len(latencies) > 0 {
		b.ReportMetric(percentileNS(latencies, 0.50), "p50-ns")
		b.ReportMetric(percentileNS(latencies, 0.95), "p95-ns")
		b.ReportMetric(percentileNS(latencies, 0.99), "p99-ns")
	}
	if elapsed := b.Elapsed().Seconds(); elapsed > 0 && logicalOps > 0 {
		b.ReportMetric(float64(logicalOps)/elapsed, "ops/s")
	}
	b.ReportMetric(float64(rssAfter), "rss-live-B")
	b.ReportMetric(float64(int64(rssAfter)-int64(rssBefore)), "rss-delta-B")
	b.ReportMetric(float64(rssPeak), "rss-peak-B")
}
