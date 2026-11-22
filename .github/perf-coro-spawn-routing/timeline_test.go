//go:build corospawntimeline && cgo

package corobench

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	_ "unsafe"
)

//go:linkname timelineClock runtime.nanotime
func timelineClock() int64

var (
	timelineOrigin              int64
	timelineFirst, timelineLast atomic.Int64
	timelinePublished           int64
	timelineSeed, timelineDone  atomic.Uint64
	timelineChecksum            atomic.Uint64
	timelineWork, timelineTasks int
)

func timelineWorker() {
	if timelineFirst.Load() == 0 {
		timelineFirst.CompareAndSwap(0, timelineClock()-timelineOrigin)
	}
	seed := timelineSeed.Add(1)
	value := parallelWork(timelineWork, seed)
	runtime.Gosched()
	value = parallelWork(timelineWork, value)
	timelineChecksum.Add(value)
	if timelineDone.Add(1) == uint64(timelineTasks) {
		timelineLast.Store(timelineClock() - timelineOrigin)
	}
}

//go:noinline
func timelineYield(tasks int) uint64 {
	for task := 0; task < tasks; task++ {
		go timelineWorker()
	}
	timelinePublished = timelineClock() - timelineOrigin
	for timelineLast.Load() == 0 {
		runtime.Gosched()
	}
	return timelineChecksum.Load()
}

func traceTimeline(b *testing.B, tasks, work int) {
	for j := 0; j < b.N; j++ {
		timelineTasks, timelineWork = tasks, work
		timelineFirst.Store(0)
		timelineLast.Store(0)
		timelineDone.Store(0)
		timelineSeed.Store(0)
		timelineChecksum.Store(0)
		timelineOrigin = timelineClock()
		if timelineYield(tasks) == 0 {
			b.Fatal("missing timeline checksum")
		}
	}
	b.StopTimer()
	fmt.Printf("spawn-timeline %d %d %d\n", timelinePublished, timelineFirst.Load(), timelineLast.Load())
}

func BenchmarkTimelineWork16(b *testing.B) {
	traceTimeline(b, 16, 4096)
}

func BenchmarkTimelineLight64(b *testing.B) {
	traceTimeline(b, 64, 512)
}
