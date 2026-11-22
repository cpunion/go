package main

import (
	"exchangestress/pair"
	"testing"
)

func BenchmarkPublicUnbufferedExchange(b *testing.B) {
	b.ReportAllocs()
	channel := make(chan int)
	done := make(chan struct{})
	iterations := b.N
	go func() {
		for range iterations {
			value := <-channel
			channel <- value + 1
		}
		close(done)
	}()
	for i := range iterations {
		value, ok := pair.CommaOK(channel, i)
		if !ok || value != i+1 {
			b.Fatal("unbuffered exchange changed the value")
		}
	}
	b.StopTimer()
	<-done
}
