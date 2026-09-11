package tuntap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const gsoReadTestOffset = 13

type gsoReadTestCase struct {
	name       string
	hdr        virtioNetHdr
	packet     func() []byte
	wantLen    int
	isV6       bool
	protocol   uint8
	payloadLen int
}

func gsoReadTestCases() []gsoReadTestCase {
	return []gsoReadTestCase{
		{
			name: "TCPv4",
			hdr: virtioNetHdr{
				flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
				gsoType:    unix.VIRTIO_NET_HDR_GSO_TCPV4,
				gsoSize:    100,
				hdrLen:     40,
				csumStart:  20,
				csumOffset: 16,
			},
			packet: func() []byte {
				return tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck|header.TCPFlagPsh, 200, 1)
			},
			wantLen:    140,
			protocol:   unix.IPPROTO_TCP,
			payloadLen: 100,
		},
		{
			name: "TCPv6",
			hdr: virtioNetHdr{
				flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
				gsoType:    unix.VIRTIO_NET_HDR_GSO_TCPV6,
				gsoSize:    100,
				hdrLen:     60,
				csumStart:  40,
				csumOffset: 16,
			},
			packet: func() []byte {
				return tcp6Packet(ip6PortA, ip6PortB, header.TCPFlagAck|header.TCPFlagPsh, 200, 1)
			},
			wantLen:    160,
			isV6:       true,
			protocol:   unix.IPPROTO_TCP,
			payloadLen: 100,
		},
		{
			name: "UDPv4",
			hdr: virtioNetHdr{
				flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
				gsoType:    unix.VIRTIO_NET_HDR_GSO_UDP_L4,
				gsoSize:    100,
				hdrLen:     28,
				csumStart:  20,
				csumOffset: 6,
			},
			packet: func() []byte {
				return udp4Packet(ip4PortA, ip4PortB, 200)
			},
			wantLen:    128,
			protocol:   unix.IPPROTO_UDP,
			payloadLen: 100,
		},
		{
			name: "UDPv6",
			hdr: virtioNetHdr{
				flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
				gsoType:    unix.VIRTIO_NET_HDR_GSO_UDP_L4,
				gsoSize:    100,
				hdrLen:     48,
				csumStart:  40,
				csumOffset: 6,
			},
			packet: func() []byte {
				return udp6Packet(ip6PortA, ip6PortB, 200)
			},
			wantLen:    148,
			isV6:       true,
			protocol:   unix.IPPROTO_UDP,
			payloadLen: 100,
		},
	}
}

func encodeGSOReadTestPacket(t *testing.T, tc gsoReadTestCase) []byte {
	t.Helper()
	in := tc.packet()
	if err := tc.hdr.encode(in); err != nil {
		t.Fatalf("encode virtio header: %v", err)
	}
	return in
}

func filledBytes(length, capacity int, value byte) ([]byte, []byte) {
	backing := bytes.Repeat([]byte{value}, capacity)
	return backing[:length], backing
}

func TestHandleVirtioReadGSOBufferLength(t *testing.T) {
	for _, tc := range gsoReadTestCases() {
		t.Run(tc.name+" exact fit", func(t *testing.T) {
			in := encodeGSOReadTestPacket(t, tc)
			bufs := make([][]byte, 2)
			for i := range bufs {
				bufs[i] = bytes.Repeat([]byte{0xa5}, gsoReadTestOffset+tc.wantLen)
			}
			sizes := []int{-1, -1}

			n, err := handleVirtioRead(in, bufs, sizes, gsoReadTestOffset)
			if err != nil {
				t.Fatalf("handleVirtioRead: %v", err)
			}
			if n != 2 {
				t.Fatalf("packet count = %d, want 2", n)
			}
			for i := 0; i < n; i++ {
				if sizes[i] != tc.wantLen {
					t.Errorf("sizes[%d] = %d, want %d", i, sizes[i], tc.wantLen)
				}
				if !bytes.Equal(bufs[i][:gsoReadTestOffset], bytes.Repeat([]byte{0xa5}, gsoReadTestOffset)) {
					t.Errorf("output prefix %d changed", i)
				}
				verifyGSOSegment(t, bufs[i][gsoReadTestOffset:], tc, i, n)
			}
		})

		t.Run(tc.name+" one byte short with spare capacity", func(t *testing.T) {
			in := encodeGSOReadTestPacket(t, tc)
			first, firstBacking := filledBytes(gsoReadTestOffset+tc.wantLen, gsoReadTestOffset+tc.wantLen, 0xa5)
			last, lastBacking := filledBytes(gsoReadTestOffset+tc.wantLen-1, gsoReadTestOffset+tc.wantLen+16, 0x5a)
			bufs := [][]byte{first, last}
			beforeFirst := bytes.Clone(firstBacking)
			beforeLast := bytes.Clone(lastBacking)
			sizes := []int{71, 72}

			n, err := handleVirtioRead(in, bufs, sizes, gsoReadTestOffset)
			if n != 0 {
				t.Errorf("packet count = %d, want 0", n)
			}
			if !errors.Is(err, io.ErrShortBuffer) {
				t.Fatalf("error = %v, want io.ErrShortBuffer", err)
			}
			if !strings.Contains(err.Error(), "need "+strconv.Itoa(tc.wantLen)+" bytes, have "+strconv.Itoa(tc.wantLen-1)) {
				t.Errorf("error does not contain required and available sizes: %v", err)
			}
			if !bytes.Equal(firstBacking, beforeFirst) || !bytes.Equal(lastBacking, beforeLast) {
				t.Error("an output buffer or canary changed")
			}
			if sizes[0] != 71 || sizes[1] != 72 {
				t.Errorf("sizes changed: %v", sizes)
			}
		})
	}
}

func verifyGSOSegment(t *testing.T, segment []byte, tc gsoReadTestCase, segmentIndex, segmentCount int) {
	t.Helper()
	if len(segment) != tc.wantLen {
		t.Fatalf("segment length = %d, want %d", len(segment), tc.wantLen)
	}

	ipHeaderLen := int(tc.hdr.csumStart)
	if tc.isV6 {
		if got := int(binary.BigEndian.Uint16(segment[4:6])); got != len(segment)-40 {
			t.Errorf("IPv6 payload length = %d, want %d", got, len(segment)-40)
		}
	} else {
		if got := int(binary.BigEndian.Uint16(segment[2:4])); got != len(segment) {
			t.Errorf("IPv4 total length = %d, want %d", got, len(segment))
		}
		if got := checksum(segment[:ipHeaderLen], 0); got != 0xffff {
			t.Errorf("IPv4 checksum result = %#x, want 0xffff", got)
		}
	}

	if tc.protocol == unix.IPPROTO_TCP {
		if got, want := binary.BigEndian.Uint32(segment[ipHeaderLen+4:]), uint32(1+segmentIndex*tc.payloadLen); got != want {
			t.Errorf("TCP sequence = %d, want %d", got, want)
		}
		wantFlags := byte(tcpFlagACK)
		if segmentIndex == segmentCount-1 {
			wantFlags |= tcpFlagPSH
		}
		if got := segment[ipHeaderLen+tcpFlagsOffset]; got != wantFlags {
			t.Errorf("TCP flags = %#x, want %#x", got, wantFlags)
		}
	} else if got, want := int(binary.BigEndian.Uint16(segment[ipHeaderLen+4:])), len(segment)-ipHeaderLen; got != want {
		t.Errorf("UDP length = %d, want %d", got, want)
	}

	srcAddrOffset := ipv4SrcAddrOffset
	addrLen := 4
	if tc.isV6 {
		srcAddrOffset = ipv6SrcAddrOffset
		addrLen = 16
	}
	pseudo := pseudoHeaderChecksumNoFold(tc.protocol, segment[srcAddrOffset:srcAddrOffset+addrLen], segment[srcAddrOffset+addrLen:srcAddrOffset+2*addrLen], uint16(len(segment)-ipHeaderLen))
	if got := checksum(segment[ipHeaderLen:], pseudo); got != 0xffff {
		t.Errorf("transport checksum result = %#x, want 0xffff", got)
	}
}

func TestHandleVirtioReadGSOExactCapacityShortBuffer(t *testing.T) {
	tc := gsoReadTestCases()[0]
	in := encodeGSOReadTestPacket(t, tc)
	shortLen := gsoReadTestOffset + tc.wantLen - 1
	bufs := [][]byte{
		bytes.Repeat([]byte{0xa5}, shortLen),
		bytes.Repeat([]byte{0x5a}, gsoReadTestOffset+tc.wantLen),
	}
	before := bytes.Clone(bufs[0])
	sizes := []int{81, 82}

	n, err := handleVirtioRead(in, bufs, sizes, gsoReadTestOffset)
	if n != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("handleVirtioRead = (%d, %v), want (0, io.ErrShortBuffer)", n, err)
	}
	if !bytes.Equal(bufs[0], before) {
		t.Error("short output buffer changed")
	}
	if sizes[0] != 81 || sizes[1] != 82 {
		t.Errorf("sizes changed: %v", sizes)
	}
}

func TestHandleVirtioReadGSOUsesVisibleLength(t *testing.T) {
	const (
		segmentLen        = 1428
		shortVisibleLen   = 1420
		visibleCapacity   = 2038
		segmentPayloadLen = segmentLen - 40
	)
	hdr := virtioNetHdr{
		flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
		gsoType:    unix.VIRTIO_NET_HDR_GSO_TCPV4,
		gsoSize:    segmentPayloadLen,
		hdrLen:     40,
		csumStart:  20,
		csumOffset: 16,
	}
	in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, 2*segmentPayloadLen, 1)
	if err := hdr.encode(in); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		visibleLen int
		wantErr    bool
	}{
		{name: "exact fit", visibleLen: segmentLen},
		{name: "short visible length", visibleLen: shortVisibleLen, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, firstBacking := filledBytes(gsoReadTestOffset+tt.visibleLen, gsoReadTestOffset+visibleCapacity, 0xa5)
			second, secondBacking := filledBytes(gsoReadTestOffset+segmentLen, gsoReadTestOffset+visibleCapacity, 0x5a)
			beforeFirst := bytes.Clone(firstBacking)
			beforeSecond := bytes.Clone(secondBacking)
			sizes := []int{-1, -1}

			n, err := handleVirtioRead(in, [][]byte{first, second}, sizes, gsoReadTestOffset)
			if tt.wantErr {
				if n != 0 || !errors.Is(err, io.ErrShortBuffer) {
					t.Fatalf("handleVirtioRead = (%d, %v), want (0, io.ErrShortBuffer)", n, err)
				}
				if !bytes.Equal(firstBacking, beforeFirst) || !bytes.Equal(secondBacking, beforeSecond) {
					t.Fatal("an output buffer or bytes past its visible length changed")
				}
				if sizes[0] != -1 || sizes[1] != -1 {
					t.Fatalf("sizes changed: %v", sizes)
				}
				return
			}
			if err != nil || n != 2 {
				t.Fatalf("handleVirtioRead = (%d, %v), want (2, nil)", n, err)
			}
			if sizes[0] != segmentLen || sizes[1] != segmentLen {
				t.Fatalf("sizes = %v, want [%d %d]", sizes, segmentLen, segmentLen)
			}
			if !bytes.Equal(firstBacking[len(first):], beforeFirst[len(first):]) ||
				!bytes.Equal(secondBacking[len(second):], beforeSecond[len(second):]) {
				t.Fatal("bytes past an output buffer's visible length changed")
			}
		})
	}
}

func TestHandleVirtioReadGSOBatchBoundaries(t *testing.T) {
	tests := []struct {
		name         string
		segmentCount int
		bufferCount  int
		wantErr      error
	}{
		{name: "one segment", segmentCount: 1, bufferCount: 1},
		{name: "ideal batch size", segmentCount: IdealBatchSize, bufferCount: IdealBatchSize},
		{name: "more than ideal batch size", segmentCount: IdealBatchSize + 1, bufferCount: IdealBatchSize, wantErr: ErrTooManySegments},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := virtioNetHdr{
				flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
				gsoType:    unix.VIRTIO_NET_HDR_GSO_TCPV4,
				gsoSize:    1,
				hdrLen:     40,
				csumStart:  20,
				csumOffset: 16,
			}
			in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, uint32(tt.segmentCount), 1)
			if err := hdr.encode(in); err != nil {
				t.Fatal(err)
			}
			bufs := make([][]byte, tt.bufferCount)
			before := make([][]byte, tt.bufferCount)
			for i := range bufs {
				bufs[i] = bytes.Repeat([]byte{0xa5}, 41)
				before[i] = bytes.Clone(bufs[i])
			}
			sizes := make([]int, tt.bufferCount)
			for i := range sizes {
				sizes[i] = -1
			}

			n, err := handleVirtioRead(in, bufs, sizes, 0)
			if tt.wantErr != nil {
				if n != 0 || !errors.Is(err, tt.wantErr) {
					t.Fatalf("handleVirtioRead = (%d, %v), want (0, %v)", n, err, tt.wantErr)
				}
				for i := range bufs {
					if !bytes.Equal(bufs[i], before[i]) || sizes[i] != -1 {
						t.Fatalf("output %d or its size changed", i)
					}
				}
				return
			}
			if err != nil || n != tt.segmentCount {
				t.Fatalf("handleVirtioRead = (%d, %v), want (%d, nil)", n, err, tt.segmentCount)
			}
			for i := range sizes {
				if sizes[i] != 41 {
					t.Errorf("sizes[%d] = %d, want 41", i, sizes[i])
				}
			}
		})
	}
}

func TestHandleVirtioReadGSOAllOrNothingErrors(t *testing.T) {
	tc := gsoReadTestCases()[0]
	tests := []struct {
		name       string
		payloadLen int
		bufferLens []int
		sizesLen   int
		offset     int
		wantErr    error
	}{
		{name: "first buffer is short", payloadLen: 300, bufferLens: []int{139, 140, 140}, sizesLen: 3, wantErr: io.ErrShortBuffer},
		{name: "middle buffer is short", payloadLen: 300, bufferLens: []int{140, 139, 140}, sizesLen: 3, wantErr: io.ErrShortBuffer},
		{name: "last buffer is short", payloadLen: 300, bufferLens: []int{140, 140, 139}, sizesLen: 3, wantErr: io.ErrShortBuffer},
		{name: "fewer sizes than segments", payloadLen: 300, bufferLens: []int{140, 140, 140}, sizesLen: 2, wantErr: io.ErrShortBuffer},
		{name: "fewer sizes than buffers", payloadLen: 100, bufferLens: []int{140, 140}, sizesLen: 1, wantErr: io.ErrShortBuffer},
		{name: "negative offset", payloadLen: 100, bufferLens: []int{140}, sizesLen: 1, offset: -1},
		{name: "offset exceeds length", payloadLen: 100, bufferLens: []int{140}, sizesLen: 1, offset: 141},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := tc.hdr
			in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, uint32(tt.payloadLen), 1)
			if err := hdr.encode(in); err != nil {
				t.Fatal(err)
			}
			bufs := make([][]byte, len(tt.bufferLens))
			before := make([][]byte, len(tt.bufferLens))
			for i, length := range tt.bufferLens {
				bufs[i] = bytes.Repeat([]byte{byte(i + 1)}, length)
				before[i] = bytes.Clone(bufs[i])
			}
			sizes := make([]int, tt.sizesLen)
			for i := range sizes {
				sizes[i] = 99
			}

			n, err := handleVirtioRead(in, bufs, sizes, tt.offset)
			if n != 0 || err == nil {
				t.Fatalf("handleVirtioRead = (%d, %v), want (0, error)", n, err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			for i := range bufs {
				if !bytes.Equal(bufs[i], before[i]) {
					t.Errorf("output %d changed", i)
				}
			}
			for i, size := range sizes {
				if size != 99 {
					t.Errorf("sizes[%d] = %d, want 99", i, size)
				}
			}
		})
	}
}

func TestHandleVirtioReadNonGSOBufferLength(t *testing.T) {
	in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, 100, 1)
	hdr := virtioNetHdr{gsoType: unix.VIRTIO_NET_HDR_GSO_NONE}
	if err := hdr.encode(in); err != nil {
		t.Fatal(err)
	}
	packet := in[virtioNetHdrLen:]

	t.Run("exact fit with nonzero offset", func(t *testing.T) {
		out := bytes.Repeat([]byte{0xa5}, gsoReadTestOffset+len(packet))
		sizes := []int{-1}
		n, err := handleVirtioRead(in, [][]byte{out}, sizes, gsoReadTestOffset)
		if err != nil || n != 1 {
			t.Fatalf("handleVirtioRead = (%d, %v), want (1, nil)", n, err)
		}
		if sizes[0] != len(packet) || !bytes.Equal(out[gsoReadTestOffset:], packet) {
			t.Errorf("output size or packet is incorrect")
		}
	})

	for _, extraCapacity := range []int{0, 16} {
		name := "exact capacity"
		if extraCapacity != 0 {
			name = "spare capacity"
		}
		t.Run("one byte short with "+name, func(t *testing.T) {
			visibleLen := gsoReadTestOffset + len(packet) - 1
			out, backing := filledBytes(visibleLen, visibleLen+extraCapacity, 0x5a)
			before := bytes.Clone(backing)
			sizes := []int{88}
			n, err := handleVirtioRead(in, [][]byte{out}, sizes, gsoReadTestOffset)
			if n != 0 || !errors.Is(err, io.ErrShortBuffer) {
				t.Fatalf("handleVirtioRead = (%d, %v), want (0, io.ErrShortBuffer)", n, err)
			}
			if !bytes.Equal(backing, before) || sizes[0] != 88 {
				t.Error("output, canary, or size changed")
			}
		})
	}
}

func TestHandleVirtioReadMalformedInputs(t *testing.T) {
	validOut := func() [][]byte { return [][]byte{make([]byte, 256)} }
	tests := []struct {
		name  string
		input func(t *testing.T) []byte
		bufs  func() [][]byte
		sizes []int
	}{
		{
			name:  "short virtio header",
			input: func(*testing.T) []byte { return make([]byte, virtioNetHdrLen-1) },
			bufs:  validOut,
			sizes: []int{7},
		},
		{
			name: "empty GSO packet",
			input: func(t *testing.T) []byte {
				in := make([]byte, virtioNetHdrLen)
				hdr := virtioNetHdr{gsoType: unix.VIRTIO_NET_HDR_GSO_TCPV4, gsoSize: 1}
				if err := hdr.encode(in); err != nil {
					t.Fatal(err)
				}
				return in
			},
			bufs:  validOut,
			sizes: []int{7},
		},
		{
			name: "zero GSO size",
			input: func(t *testing.T) []byte {
				in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, 100, 1)
				hdr := virtioNetHdr{gsoType: unix.VIRTIO_NET_HDR_GSO_TCPV4, csumStart: 20, csumOffset: 16}
				if err := hdr.encode(in); err != nil {
					t.Fatal(err)
				}
				return in
			},
			bufs:  validOut,
			sizes: []int{7},
		},
		{
			name: "invalid checksum position",
			input: func(t *testing.T) []byte {
				in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, 100, 1)
				hdr := virtioNetHdr{flags: unix.VIRTIO_NET_HDR_F_NEEDS_CSUM, gsoType: unix.VIRTIO_NET_HDR_GSO_NONE, csumStart: 20, csumOffset: maxUint16}
				if err := hdr.encode(in); err != nil {
					t.Fatal(err)
				}
				return in
			},
			bufs:  validOut,
			sizes: []int{7},
		},
		{
			name: "empty output buffers",
			input: func(t *testing.T) []byte {
				in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, 100, 1)
				hdr := virtioNetHdr{gsoType: unix.VIRTIO_NET_HDR_GSO_NONE}
				if err := hdr.encode(in); err != nil {
					t.Fatal(err)
				}
				return in
			},
			bufs:  func() [][]byte { return nil },
			sizes: nil,
		},
		{
			name: "sizes shorter than outputs",
			input: func(t *testing.T) []byte {
				in := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck, 100, 1)
				hdr := virtioNetHdr{gsoType: unix.VIRTIO_NET_HDR_GSO_NONE}
				if err := hdr.encode(in); err != nil {
					t.Fatal(err)
				}
				return in
			},
			bufs:  func() [][]byte { return [][]byte{make([]byte, 256), make([]byte, 256)} },
			sizes: []int{7},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bufs := tt.bufs()
			before := make([][]byte, len(bufs))
			for i := range bufs {
				for j := range bufs[i] {
					bufs[i][j] = 0xa5
				}
				before[i] = bytes.Clone(bufs[i])
			}
			sizes := append([]int(nil), tt.sizes...)
			beforeSizes := append([]int(nil), sizes...)

			n, err := handleVirtioRead(tt.input(t), bufs, sizes, 0)
			if n != 0 || err == nil {
				t.Fatalf("handleVirtioRead = (%d, %v), want (0, error)", n, err)
			}
			for i := range bufs {
				if !bytes.Equal(bufs[i], before[i]) {
					t.Errorf("output %d changed", i)
				}
			}
			if !intSlicesEqual(sizes, beforeSizes) {
				t.Errorf("sizes changed from %v to %v", beforeSizes, sizes)
			}
		})
	}
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func FuzzHandleVirtioReadDoesNotExceedVisibleBuffers(f *testing.F) {
	tcp4 := tcp4Packet(ip4PortA, ip4PortB, header.TCPFlagAck|header.TCPFlagPsh, 200, 1)[virtioNetHdrLen:]
	f.Add(tcp4, uint8(unix.VIRTIO_NET_HDR_GSO_TCPV4), uint16(40), uint16(100), uint16(20), uint16(16), uint8(2), uint16(153), uint16(16), int16(13), uint8(2))
	f.Add([]byte(nil), uint8(unix.VIRTIO_NET_HDR_GSO_TCPV4), uint16(maxUint16), uint16(0), uint16(maxUint16), uint16(maxUint16), uint8(0), uint16(0), uint16(0), int16(-1), uint8(0))
	f.Add(tcp4, uint8(unix.VIRTIO_NET_HDR_GSO_NONE), uint16(0), uint16(0), uint16(20), uint16(16), uint8(1), uint16(152), uint16(16), int16(13), uint8(1))

	f.Fuzz(func(t *testing.T, packet []byte, gsoType uint8, hdrLen, gsoSize, csumStart, csumOffset uint16, rawBufferCount uint8, rawVisibleLen, rawExtraCapacity uint16, rawOffset int16, rawSizesLen uint8) {
		in := make([]byte, virtioNetHdrLen+len(packet))
		copy(in[virtioNetHdrLen:], packet)
		hdr := virtioNetHdr{
			flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
			gsoType:    gsoType,
			hdrLen:     hdrLen,
			gsoSize:    gsoSize,
			csumStart:  csumStart,
			csumOffset: csumOffset,
		}
		if err := hdr.encode(in); err != nil {
			t.Fatal(err)
		}

		bufferCount := int(rawBufferCount % 5)
		visibleLen := int(rawVisibleLen % 512)
		extraCapacity := int(rawExtraCapacity % 64)
		bufs := make([][]byte, bufferCount)
		backings := make([][]byte, bufferCount)
		before := make([][]byte, bufferCount)
		for i := range bufs {
			bufs[i], backings[i] = filledBytes(visibleLen, visibleLen+extraCapacity, byte(i+1))
			before[i] = bytes.Clone(backings[i])
		}
		sizes := make([]int, int(rawSizesLen%5))
		for i := range sizes {
			sizes[i] = -1
		}
		beforeSizes := append([]int(nil), sizes...)
		offset := int(rawOffset % 600)

		n, err := handleVirtioRead(in, bufs, sizes, offset)
		if err != nil {
			if n != 0 {
				t.Fatalf("error result reported %d packets", n)
			}
			for i := range bufs {
				if !bytes.Equal(backings[i], before[i]) {
					t.Fatalf("output backing array %d changed after an error", i)
				}
			}
			if !intSlicesEqual(sizes, beforeSizes) {
				t.Fatalf("sizes changed after an error: before %v, after %v", beforeSizes, sizes)
			}
			return
		}

		if n < 0 || n > len(bufs) || n > len(sizes) {
			t.Fatalf("packet count %d is outside output bounds", n)
		}
		if offset < 0 {
			t.Fatalf("negative offset %d succeeded", offset)
		}
		for i := 0; i < n; i++ {
			if offset > len(bufs[i]) || sizes[i] < 0 || sizes[i] > len(bufs[i])-offset {
				t.Fatalf("sizes[%d] = %d exceeds visible output length %d with offset %d", i, sizes[i], len(bufs[i]), offset)
			}
		}
	})
}
