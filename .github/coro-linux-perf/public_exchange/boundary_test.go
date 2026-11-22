package main

import (
	"exchangestress/pair"
	"testing"
)

var result pair.Item

func BenchmarkReadyPublicExchange(b *testing.B) {
	b.ReportAllocs()
	channel := make(chan pair.Item, 1)
	payload := 42
	value := pair.Item{ID: 7, Value: &payload}
	for i := 0; i < b.N; i++ {
		result = pair.Exchange(channel, value)
	}
	if result.ID != 7 || *result.Value != 42 {
		b.Fatal("exchange changed the payload")
	}
}
