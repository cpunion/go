package pair

//go:noinline
func Adjusted(channel chan int, sent int) int {
	channel <- sent
	value := <-channel
	return value + 1
}

//go:noinline
func CommaOK(channel chan int, sent int) (int, bool) {
	channel <- sent
	value, ok := <-channel
	return value, ok
}
