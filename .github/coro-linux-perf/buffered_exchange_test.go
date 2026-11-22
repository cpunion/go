package corobench

import "testing"

func BenchmarkReadyChannelPrefilledPair(b *testing.B) {
	b.ReportAllocs()
	channel := make(chan int, 2)
	channel <- -1
	intSink = readyParameterChannelPairs(channel, b.N)
	if len(channel) != 1 || <-channel != b.N-1 {
		b.Fatal("buffer contents changed")
	}
}

func BenchmarkUnbufferedChannelParameterPair(b *testing.B) {
	b.ReportAllocs()
	channel := make(chan int)
	iterations := b.N
	go func() {
		for range iterations {
			value := <-channel
			channel <- value
		}
	}()
	intSink = readyParameterChannelPairs(channel, iterations)
}
