package main

import (
	"exchangestress/pair"
	"testing"
)

var intResult int
var boolResult bool

func BenchmarkReadyPublicExchangeAdjusted(b *testing.B) {
	b.ReportAllocs()
	channel := make(chan int, 1)
	for i := 0; i < b.N; i++ {
		intResult = pair.Adjusted(channel, i)
	}
	if intResult != b.N {
		b.Fatal("adjusted receive changed")
	}
}

func BenchmarkReadyPublicExchangeCommaOK(b *testing.B) {
	b.ReportAllocs()
	channel := make(chan int, 1)
	for i := 0; i < b.N; i++ {
		intResult, boolResult = pair.CommaOK(channel, i)
	}
	if intResult != b.N-1 || !boolResult {
		b.Fatal("comma-ok receive changed")
	}
}
