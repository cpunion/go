package corobench

import (
	"runtime"
	"testing"
)

var byteReads int

//go:noinline
func effectfulByteRead(data *[440]byte) int {
	byteReads++
	return int(data[0])
}

func effectfulByteA(n int, value *int) int {
	var data [440]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return *value + effectfulByteRead(&data)
	}
	child := effectfulByteB(n-1, value)
	return child + *value + effectfulByteRead(&data)
}

func effectfulByteB(n int, value *int) int {
	var data [440]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return *value + effectfulByteRead(&data)
	}
	child := effectfulByteA(n-1, value)
	return child + *value + effectfulByteRead(&data)
}

func BenchmarkPackedArrayReaderEffect440(b *testing.B) {
	const depth = 512
	value := 5
	byteReads = 0
	for i := 0; i < b.N; i++ {
		intSink = effectfulByteA(depth, &value)
	}
	if intSink != (depth+1)*5+2*32640 || byteReads != b.N*(depth+1) {
		b.Fatalf("result %d, %d reader calls", intSink, byteReads)
	}
}

//go:noinline
func readCallerByte(data *[440]byte) int { return int(data[0]) }

func callerEffectA(n int, value *int) int {
	var data [440]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		*value++
		return *value + readCallerByte(&data)
	}
	child := callerEffectB(n-1, value)
	*value++
	return child + *value + readCallerByte(&data)
}

func callerEffectB(n int, value *int) int {
	var data [440]byte
	data[0] = byte(n)
	if n == 0 {
		runtime.Gosched()
		*value++
		return *value + readCallerByte(&data)
	}
	child := callerEffectA(n-1, value)
	*value++
	return child + *value + readCallerByte(&data)
}

func BenchmarkPackedArrayCallerEffect440(b *testing.B) {
	const depth = 512
	for i := 0; i < b.N; i++ {
		value := 0
		intSink = callerEffectA(depth, &value)
	}
	if intSink != (depth+1)*(depth+2)/2+2*32640 {
		b.Fatalf("result %d", intSink)
	}
}
