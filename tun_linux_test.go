package tuntap

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

type stubBatchWriter struct {
	writes []stubBatchWrite
	calls  int
}

type stubBatchWrite struct {
	n   int
	err error
}

func (w *stubBatchWriter) Write(p []byte) (int, error) {
	if w.calls >= len(w.writes) {
		return 0, nil
	}
	write := w.writes[w.calls]
	w.calls++
	return write.n, write.err
}

func TestWriteBatchReturnsPacketCount(t *testing.T) {
	t.Parallel()

	writer := &stubBatchWriter{
		writes: []stubBatchWrite{
			{n: 60},
			{n: 120},
		},
	}
	bufs := [][]byte{
		make([]byte, 60),
		make([]byte, 120),
	}

	n, err := writeBatch(writer, bufs, []int{0, 1}, 0)
	if err != nil {
		t.Fatalf("writeBatch returned unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("writeBatch returned %d, want packet count 2", n)
	}
}

func TestWriteBatchAggregatesErrorsByPacket(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("boom")
	writer := &stubBatchWriter{
		writes: []stubBatchWrite{
			{n: 60},
			{err: expectedErr},
			{n: 80},
		},
	}
	bufs := [][]byte{
		make([]byte, 60),
		make([]byte, 70),
		make([]byte, 80),
	}

	n, err := writeBatch(writer, bufs, []int{0, 1, 2}, 0)
	if n != 2 {
		t.Fatalf("writeBatch returned %d, want 2 successful packets", n)
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("writeBatch error = %v, want wrapped %v", err, expectedErr)
	}
}

func TestWriteBatchMapsBadFDToClosed(t *testing.T) {
	t.Parallel()

	writer := &stubBatchWriter{
		writes: []stubBatchWrite{
			{n: 60},
			{err: syscall.EBADFD},
			{n: 80},
		},
	}
	bufs := [][]byte{
		make([]byte, 60),
		make([]byte, 70),
		make([]byte, 80),
	}

	n, err := writeBatch(writer, bufs, []int{0, 1, 2}, 0)
	if n != 1 {
		t.Fatalf("writeBatch returned %d, want 1 successful packet before close", n)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("writeBatch error = %v, want %v", err, os.ErrClosed)
	}
}
