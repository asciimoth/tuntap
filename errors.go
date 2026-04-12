package tuntap

import (
	"errors"
)

var (
	// ErrTooManySegments is returned by NativeTun.Read when incoming
	// segmented packets (from virtio GSO) exceed the capacity of the
	// supplied buffers. This is a transient error indicating that the
	// caller should retry with larger or more buffers; it should not
	// cause the caller to stop reading.
	ErrTooManySegments = errors.New("too many segments")
)
