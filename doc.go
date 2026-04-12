// Package tuntap provides cross-platform support for creating and managing
// TUN (network tunnel) devices.
//
// TUN devices operate at layer 3 (network layer), handling raw IP packets
// without Ethernet framing. They are commonly used to implement VPNs,
// virtual networks, and other packet tunneling applications.
//
// # Supported Platforms
//
// This package supports Linux, macOS (Darwin), FreeBSD, OpenBSD, and Windows.
// Each platform provides a NativeTun type that implements the tun.Tun interface
// from github.com/asciimoth/gonnect/tun.
//
// # Basic Usage
//
// Create a TUN device by calling CreateTUN with the desired interface name
// and MTU:
//
//	tun, err := tuntap.CreateTUN("utun0", 1420)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer tun.Close()
//
//	name, _ := tun.Name()
//	fmt.Printf("Created interface: %s\n", name)
//
// # Reading and Writing Packets
//
// Use the Read and Write methods to exchange IP packets with the TUN device.
// Read accepts a slice of buffers and populates them with incoming packets,
// returning the count and the size of each packet:
//
//	bufs := make([][]byte, tuntap.IdealBatchSize)
//	for i := range bufs {
//	    bufs[i] = make([]byte, 65535)
//	}
//	sizes := make([]int, tuntap.IdealBatchSize)
//
//	n, err := tun.Read(bufs, sizes, 0)
//	if err != nil {
//	    // handle error
//	}
//	for i := 0; i < n; i++ {
//	    packet := bufs[i][0:sizes[i]]
//	    // process IP packet
//	}
//
// Write sends one or more IP packets from the provided buffers:
//
//	_, err = tun.Write(bufs[:1], 0)
//
// # Platform-Specific Batch Sizes
//
// On Linux, when the kernel supports IFF_VNET_HDR, the TUN device enables
// virtio network header mode. This allows batched I/O (up to
// IdealBatchSize packets per call) and enables TCP/UDP generic receive
// offload (GRO) and generic segmentation offload (GSO). On all other
// platforms, BatchSize() returns 1.
//
// # Device Events
//
// The Events method returns a channel that emits device state changes
// (interface up/down, MTU updates). Callers should consume from this
// channel in a goroutine:
//
//	go func() {
//	    for event := range tun.Events() {
//	        // handle event
//	    }
//	}()
//
// # Windows
//
// On Windows, this package uses the Wintun driver
// (golang.zx2c4.com/wintun). The global variables WintunTunnelType and
// WintunStaticRequestedGUID can be set before calling CreateTUN to
// customize the adapter type and GUID.
package tuntap
