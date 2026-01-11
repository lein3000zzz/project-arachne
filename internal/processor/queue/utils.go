package queue

func drainAncCloseChannel[T interface{}](channel chan T) {
	for len(channel) > 0 {
		<-channel
	}

	close(channel)
}
