package pair

type Item struct {
	ID    int
	Value *int
}

//go:noinline
func Exchange(channel chan Item, sent Item) Item {
	channel <- sent
	received := <-channel
	return received
}
