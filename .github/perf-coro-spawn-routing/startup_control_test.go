//go:build cgo

package corobench

import (
	"runtime"
	"testing"

	"./startupcontrol"
)

type startupControl uint8

const (
	primeThreads startupControl = 1 << iota
	primeContext
)

func measureFirstBatch(b *testing.B, tasks, work int, control startupControl) {
	b.ReportAllocs()
	b.StopTimer()
	if control&primeThreads != 0 {
		startupcontrol.PrimeThreads()
	}
	if control&primeContext != 0 && yieldEntry() != 1 {
		b.Fatal("context preparation failed")
	}
	before, _ := runtime.ThreadCreateProfile(nil)
	b.StartTimer()
	result := parallelYieldWork(b.N, tasks, work)
	b.StopTimer()
	after, _ := runtime.ThreadCreateProfile(nil)
	if result == 0 {
		b.Fatal("parallelYieldWork returned zero")
	}
	b.ReportMetric(float64(after-before)/float64(b.N), "threads/op")
}

func BenchmarkColdControlWork16(b *testing.B)    { measureFirstBatch(b, 16, 4096, 0) }
func BenchmarkThreadControlWork16(b *testing.B)  { measureFirstBatch(b, 16, 4096, primeThreads) }
func BenchmarkContextControlWork16(b *testing.B) { measureFirstBatch(b, 16, 4096, primeContext) }
func BenchmarkBothControlWork16(b *testing.B) {
	measureFirstBatch(b, 16, 4096, primeThreads|primeContext)
}

func BenchmarkColdControlLight64(b *testing.B)    { measureFirstBatch(b, 64, 512, 0) }
func BenchmarkThreadControlLight64(b *testing.B)  { measureFirstBatch(b, 64, 512, primeThreads) }
func BenchmarkContextControlLight64(b *testing.B) { measureFirstBatch(b, 64, 512, primeContext) }
func BenchmarkBothControlLight64(b *testing.B) {
	measureFirstBatch(b, 64, 512, primeThreads|primeContext)
}
