package corobench

import "testing"

//go:noinline
func readyParameterChannelPairs(channel chan int, iterations int) int {
	sum := 0
	for i := 0; i < iterations; i++ {
		channel <- i
		value := <-channel
		sum += value
	}
	return sum
}

func BenchmarkReadyChannelParameterPair(b *testing.B) {
	b.ReportAllocs()
	intSink = readyParameterChannelPairs(make(chan int, 1), b.N)
}
