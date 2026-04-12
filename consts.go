package tuntap

const (
	// IdealBatchSize is the maximum number of packets that can be processed
	// in a single Read or Write call. On Linux with virtio network header
	// support (IFF_VNET_HDR), NativeTun.BatchSize() returns this value.
	// On all other platforms, BatchSize() returns 1.
	IdealBatchSize = 128
)
