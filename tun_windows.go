/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package tuntap

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"

	gtun "github.com/asciimoth/gonnect/tun"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

const (
	rateMeasurementGranularity = uint64((time.Second / 2) / time.Nanosecond)
	spinloopRateThreshold      = 800000000 / 8                                   // 800mbps
	spinloopDuration           = uint64(time.Millisecond / 80 / time.Nanosecond) // ~1gbit/s
)

type rateJuggler struct {
	current       atomic.Uint64
	nextByteCount atomic.Uint64
	nextStartTime atomic.Int64
	changing      atomic.Bool
}

// NativeTun is a Windows-specific TUN device using the Wintun driver.
// It implements the tun.Tun interface from github.com/asciimoth/gonnect/tun.
//
// On Windows, this package uses the Wintun driver (golang.zx2c4.com/wintun).
// The global variables WintunTunnelType and WintunStaticRequestedGUID can be
// set before calling CreateTUN to customize the adapter type and GUID.
type NativeTun struct {
	wt        *wintun.Adapter
	name      string
	handle    windows.Handle
	rate      rateJuggler
	session   wintun.Session
	readWait  windows.Handle
	events    chan gtun.Event
	running   sync.WaitGroup
	closeOnce sync.Once
	close     atomic.Bool
	forcedMTU int
	outSizes  []int
}

var (
	// WintunTunnelType specifies the adapter type reported by Windows for
	// created TUN devices. The default is "WireGuard". Set this before
	// calling CreateTUN to customize the type.
	WintunTunnelType = "WireGuard"

	// WintunStaticRequestedGUID specifies a static GUID to request when
	// creating a TUN device. Set this before calling CreateTUN to use a
	// fixed GUID. If nil (the default), a GUID is assigned automatically.
	WintunStaticRequestedGUID *windows.GUID
)

//go:linkname procyield runtime.procyield
func procyield(cycles uint32)

//go:linkname nanotime runtime.nanotime
func nanotime() int64

// CreateTUN creates a Wintun-based TUN device with the given interface name
// and MTU. If an interface with the same name already exists, it is reused.
func CreateTUN(ifname string, mtu int) (gtun.Tun, error) {
	return CreateTUNWithRequestedGUID(ifname, WintunStaticRequestedGUID, mtu)
}

// CreateTUNWithRequestedGUID creates a Wintun-based TUN device with the given
// interface name and a requested GUID. If an interface with the same name
// already exists, it is reused.
func CreateTUNWithRequestedGUID(ifname string, requestedGUID *windows.GUID, mtu int) (gtun.Tun, error) {
	wt, err := wintun.CreateAdapter(ifname, WintunTunnelType, requestedGUID)
	if err != nil {
		return nil, fmt.Errorf("Error creating interface: %w", err)
	}

	forcedMTU := 1420
	if mtu > 0 {
		forcedMTU = mtu
	}

	tun := &NativeTun{
		wt:        wt,
		name:      ifname,
		handle:    windows.InvalidHandle,
		events:    make(chan gtun.Event, 10),
		forcedMTU: forcedMTU,
	}

	tun.session, err = wt.StartSession(0x800000) // Ring capacity, 8 MiB
	if err != nil {
		tun.wt.Close()
		close(tun.events)
		return nil, fmt.Errorf("Error starting session: %w", err)
	}
	tun.readWait = tun.session.ReadWaitEvent()
	return tun, nil
}

func (tun *NativeTun) Name() (string, error) {
	return tun.name, nil
}

func (tun *NativeTun) File() *os.File {
	return nil
}

func (tun *NativeTun) Events() <-chan gtun.Event {
	return tun.events
}

func (tun *NativeTun) Close() error {
	var err error
	tun.closeOnce.Do(func() {
		tun.close.Store(true)
		windows.SetEvent(tun.readWait)
		tun.running.Wait()
		tun.session.End()
		if tun.wt != nil {
			tun.wt.Close()
		}
		close(tun.events)
	})
	return err
}

// MTU returns the configured MTU of the TUN device.
func (tun *NativeTun) MTU() (int, error) {
	return tun.forcedMTU, nil
}

// ForceMTU updates the MTU of the TUN device and emits an EventMTUUpdate
// on the Events channel if the value changed. This is a Windows-specific
// method to work around the lack of automatic MTU monitoring.
func (tun *NativeTun) ForceMTU(mtu int) {
	if tun.close.Load() {
		return
	}
	update := tun.forcedMTU != mtu
	tun.forcedMTU = mtu
	if update {
		tun.events <- gtun.EventMTUUpdate
	}
}

// BatchSize returns 1, as Wintun does not support batched I/O.
func (tun *NativeTun) BatchSize() int {
	// TODO: implement batching with wintun
	return 1
}

// Note: Read() and Write() assume the caller comes only from a single thread; there's no locking.

func (tun *NativeTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if err := validateReadBuffers(tun.BatchSize(), bufs, sizes); err != nil {
		return 0, err
	}
	tun.running.Add(1)
	defer tun.running.Done()
retry:
	if tun.close.Load() {
		return 0, os.ErrClosed
	}
	start := nanotime()
	shouldSpin := tun.rate.current.Load() >= spinloopRateThreshold && uint64(start-tun.rate.nextStartTime.Load()) <= rateMeasurementGranularity*2
	for {
		if tun.close.Load() {
			return 0, os.ErrClosed
		}
		packet, err := tun.session.ReceivePacket()
		switch err {
		case nil:
			n := copy(bufs[0][offset:], packet)
			sizes[0] = n
			tun.session.ReleaseReceivePacket(packet)
			tun.rate.update(uint64(n))
			return 1, nil
		case windows.ERROR_NO_MORE_ITEMS:
			if !shouldSpin || uint64(nanotime()-start) >= spinloopDuration {
				windows.WaitForSingleObject(tun.readWait, windows.INFINITE)
				goto retry
			}
			procyield(1)
			continue
		case windows.ERROR_HANDLE_EOF:
			return 0, os.ErrClosed
		case windows.ERROR_INVALID_DATA:
			return 0, errors.New("Send ring corrupt")
		}
		return 0, fmt.Errorf("Read failed: %w", err)
	}
}

func (tun *NativeTun) Write(bufs [][]byte, offset int) (int, error) {
	tun.running.Add(1)
	defer tun.running.Done()
	if tun.close.Load() {
		return 0, os.ErrClosed
	}

	for i, buf := range bufs {
		packetSize := len(buf) - offset
		tun.rate.update(uint64(packetSize))

		packet, err := tun.session.AllocateSendPacket(packetSize)
		switch err {
		case nil:
			// TODO: Explore options to eliminate this copy.
			copy(packet, buf[offset:])
			tun.session.SendPacket(packet)
			continue
		case windows.ERROR_HANDLE_EOF:
			return i, os.ErrClosed
		case windows.ERROR_BUFFER_OVERFLOW:
			continue // Dropping when ring is full.
		default:
			return i, fmt.Errorf("Write failed: %w", err)
		}
	}
	return len(bufs), nil
}

// LUID returns the Windows network interface instance ID (Locally Unique
// Identifier) for this TUN device. This can be used with other Windows APIs
// that require a LUID, such as network configuration functions.
func (tun *NativeTun) LUID() uint64 {
	tun.running.Add(1)
	defer tun.running.Done()
	if tun.close.Load() {
		return 0
	}
	return tun.wt.LUID()
}

// RunningVersion returns the running version of the Wintun driver.
func (tun *NativeTun) RunningVersion() (version uint32, err error) {
	return wintun.RunningVersion()
}

func (rate *rateJuggler) update(packetLen uint64) {
	now := nanotime()
	total := rate.nextByteCount.Add(packetLen)
	period := uint64(now - rate.nextStartTime.Load())
	if period >= rateMeasurementGranularity {
		if !rate.changing.CompareAndSwap(false, true) {
			return
		}
		rate.nextStartTime.Store(now)
		rate.current.Store(total * uint64(time.Second/time.Nanosecond) / period)
		rate.nextByteCount.Store(0)
		rate.changing.Store(false)
	}
}
