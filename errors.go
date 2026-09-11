package tuntap

import (
	"errors"
	"fmt"
	"io"
)

var (
	// ErrTooManySegments is returned by NativeTun.Read when a virtio GSO
	// frame needs more output buffers than the caller supplied. Read returns
	// no packets and does not change the supplied buffers or sizes when this
	// error occurs. The input frame has already been consumed, so a retry
	// cannot recover that frame.
	ErrTooManySegments = errors.New("too many segments")
)

func shortBufferError(buffer, required, available int) error {
	return fmt.Errorf("output buffer %d: %w: need %d bytes, have %d", buffer, io.ErrShortBuffer, required, available)
}
