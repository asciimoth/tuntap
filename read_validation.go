package tuntap

import "io"

func validateReadBuffers(batchSize int, bufs [][]byte, sizes []int) error {
	if len(bufs) < batchSize || len(sizes) < batchSize {
		return io.ErrShortBuffer
	}
	return nil
}
