# TunTap

Cross-platform TUN (network tunnel) device support for Go. 

> [!IMPORTANT]
> This project contains code extracted from the original
> [wireguard-go](https://git.zx2c4.com/wireguard-go) project
> with minor modifications.
> All credit goes to the original wireguard-go authors.

## Supported Platforms

- **Linux** — via `/dev/net/tun` with optional batched I/O and TCP/UDP GRO/GSO
- **macOS** — via utun control socket
- **FreeBSD** — via `/dev/tun`
- **OpenBSD** — via `/dev/tunN`
- **Windows** — via [Wintun](https://wintun.net/)

## Installation

```sh
go get github.com/asciimoth/tuntap
```

## Usage

```go
package main

import (
	"fmt"
	"log"

	"github.com/asciimoth/tuntap"
)

func main() {
	// Create a TUN device with the given name and MTU.
	// On macOS use "utun" or "utunN"; on BSDs use "tun" or "tunN";
	// on Linux and Windows, any valid interface name works.
	tun, err := tuntap.CreateTUN("tun0", 1420)
	if err != nil {
		log.Fatal(err)
	}
	defer tun.Close()

	name, _ := tun.Name()
	fmt.Printf("Created TUN interface: %s\n", name)

	// Monitor device events (up/down, MTU changes).
	go func() {
		for event := range tun.Events() {
			fmt.Printf("Event: %v\n", event)
		}
	}()

	// Read IP packets from the TUN device.
	bufs := make([][]byte, tun.BatchSize())
	sizes := make([]int, tun.BatchSize())
	for i := range bufs {
		bufs[i] = make([]byte, 65535)
	}

	n, err := tun.Read(bufs, sizes, 0)
	if err != nil {
		log.Fatal(err)
	}

	// Process each packet.
	for i := 0; i < n; i++ {
		packet := bufs[i][:sizes[i]]
		// packet contains a raw IP frame — route it, decrypt it, etc.
		fmt.Printf("Read packet %d/%d: %d bytes\n", i+1, n, sizes[i])
		_ = packet
	}

	// Write IP packets back to the TUN device.
	// writeBufs := [][]byte{packet}
	// _, err = tun.Write(writeBufs, 0)
}
```

## API

The primary type is `NativeTun`, which provides:

| Method | Description |
|---|---|
| `CreateTUN(name string, mtu int)` | Create a TUN device |
| `Read(bufs, sizes [][]byte, offset int)` | Read IP packets into buffers |
| `Write(bufs [][]byte, offset int)` | Write IP packets to the device |
| `Name() (string, error)` | Get the interface name |
| `MTU() (int, error)` | Get the MTU |
| `Events() <-chan Event` | Channel of device state changes |
| `BatchSize() int` | Max packets per Read/Write call |
| `Close() error` | Close the TUN device |
| `File() *os.File` | Get the underlying file (may be nil on some platforms) |

