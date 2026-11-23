//go:build cgo

package corobench

import "testing"

func BenchmarkParallelYieldLight64(b *testing.B) {
	b.ReportAllocs()
	if parallelYieldWork(b.N, 64, 512) == 0 {
		b.Fatal("parallelYieldWork returned zero")
	}
}

func BenchmarkParallelYieldHeavy64(b *testing.B) {
	b.ReportAllocs()
	if parallelYieldWork(b.N, 64, 8192) == 0 {
		b.Fatal("parallelYieldWork returned zero")
	}
}

func BenchmarkParallelYieldWork16(b *testing.B) {
	b.ReportAllocs()
	if parallelYieldWork(b.N, 16, 4096) == 0 {
		b.Fatal("parallelYieldWork returned zero")
	}
}
