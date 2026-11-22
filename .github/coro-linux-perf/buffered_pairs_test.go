package corobench

import "testing"

func bufferedSelectBothReady(iterations int) int {
	left, right := make(chan int, 1), make(chan int, 1)
	left <- 71
	right <- 43
	sum := 0
	for i := 0; i < iterations; i++ {
		select {
		case value := <-left:
			left <- value
			sum += value
		case value := <-right:
			right <- value
			sum += value
		}
	}
	return sum
}

func BenchmarkBufferedSelectBothReady(b *testing.B) {
	b.ReportAllocs()
	sum := bufferedSelectBothReady(b.N)
	if sum < b.N*43 || sum > b.N*71 {
		b.Fatalf("sum %d outside valid bounds", sum)
	}
}
