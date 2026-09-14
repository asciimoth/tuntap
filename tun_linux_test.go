package tuntap

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	gtun "github.com/asciimoth/gonnect/tun"
	"github.com/asciimoth/tuntap/rwcancel"
	"golang.org/x/sys/unix"
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

func TestNativeTunCloseAfterListenerExit(t *testing.T) {
	tun, peer := newTestNativeTun(t)
	defer unix.Close(peer)

	if err := tun.netlinkCancel.Cancel(); err != nil {
		t.Fatalf("stop netlink listener: %v", err)
	}
	waitForClosedEvents(t, tun.events)

	if err := tun.Close(); err != nil {
		t.Fatalf("Close returned an error after listener exit: %v", err)
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("second Close returned an error: %v", err)
	}
	if _, err := tun.tunFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("TUN file Stat error = %v, want %v", err, os.ErrClosed)
	}
}

func TestNativeTunCloseConcurrentWithListenerExit(t *testing.T) {
	tun, peer := newTestNativeTun(t)
	defer unix.Close(peer)

	start := make(chan struct{})
	listenerExitErr := make(chan error, 1)
	closeErr := make(chan error, 1)
	go func() {
		<-start
		listenerExitErr <- tun.netlinkCancel.Cancel()
	}()
	go func() {
		<-start
		closeErr <- tun.Close()
	}()
	close(start)

	if err := <-listenerExitErr; err != nil {
		t.Fatalf("listener cancellation returned an error: %v", err)
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
	waitForClosedEvents(t, tun.events)
}

func TestNativeTunCloseReturnsTunFileError(t *testing.T) {
	tun, peer := newTestNativeTun(t)
	defer unix.Close(peer)

	if err := tun.netlinkCancel.Cancel(); err != nil {
		t.Fatalf("stop netlink listener: %v", err)
	}
	waitForClosedEvents(t, tun.events)

	fd := int(tun.tunFile.Fd())
	if err := unix.Close(fd); err != nil {
		t.Fatalf("invalidate TUN file descriptor: %v", err)
	}

	if err := tun.Close(); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("Close error = %v, want %v", err, syscall.EBADF)
	}
}

func TestNativeTunCloseIsConcurrentSafe(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create TUN file pipe: %v", err)
	}
	defer writer.Close()

	tun := &NativeTun{
		tunFile: reader,
		events:  make(chan gtun.Event, 1),
	}

	const callers = 16
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			_ = tun.Close()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close calls blocked")
	}
}

func newTestNativeTun(t *testing.T) (*NativeTun, int) {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create TUN file pipe: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		reader.Close()
		t.Fatalf("create listener socket pair: %v", err)
	}

	cancel, err := rwcancel.NewRWCancel(sockets[0])
	if err != nil {
		reader.Close()
		unix.Close(sockets[0])
		unix.Close(sockets[1])
		t.Fatalf("create listener cancellation: %v", err)
	}

	tun := &NativeTun{
		tunFile:                 reader,
		events:                  make(chan gtun.Event, 1),
		errors:                  make(chan error, 1),
		netlinkSock:             sockets[0],
		netlinkCancel:           cancel,
		statusListenersShutdown: make(chan struct{}),
	}
	go tun.routineNetlinkListener()
	return tun, sockets[1]
}

func waitForClosedEvents(t *testing.T, events <-chan gtun.Event) {
	t.Helper()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("Events returned an event before it closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Events did not close")
	}
}
