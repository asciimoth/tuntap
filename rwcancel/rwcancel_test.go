//go:build linux

/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package rwcancel

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func newTestRWCancel(t *testing.T) *RWCancel {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create test pipe: %v", err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})

	rw, err := NewRWCancel(int(reader.Fd()))
	if err != nil {
		t.Fatalf("create RWCancel: %v", err)
	}
	t.Cleanup(rw.Close)
	return rw
}

func TestCancel(t *testing.T) {
	rw := newTestRWCancel(t)

	if err := rw.Cancel(); err != nil {
		t.Fatalf("Cancel returned an error: %v", err)
	}

	buffer := make([]byte, 1)
	if _, err := rw.closingReader.Read(buffer); err != nil {
		t.Fatalf("read cancellation byte: %v", err)
	}
}

func TestCancelAfterReaderClose(t *testing.T) {
	rw := newTestRWCancel(t)

	if err := rw.closingReader.Close(); err != nil {
		t.Fatalf("close cancellation reader: %v", err)
	}
	if _, err := rw.closingWriter.Write([]byte{0}); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("pipe write error = %v, want %v", err, syscall.EPIPE)
	}
	if err := rw.Cancel(); err != nil {
		t.Fatalf("Cancel returned an error after reader close: %v", err)
	}
}

func TestCancelAfterClose(t *testing.T) {
	rw := newTestRWCancel(t)
	rw.Close()

	if _, err := rw.closingWriter.Write([]byte{0}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("pipe write error = %v, want %v", err, os.ErrClosed)
	}
	if err := rw.Cancel(); err != nil {
		t.Fatalf("Cancel returned an error after Close: %v", err)
	}
}

func TestCancelConcurrentWithClose(t *testing.T) {
	const iterations = 100

	for i := range iterations {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatalf("iteration %d: create test pipe: %v", i, err)
		}

		rw, err := NewRWCancel(int(reader.Fd()))
		if err != nil {
			reader.Close()
			writer.Close()
			t.Fatalf("iteration %d: create RWCancel: %v", i, err)
		}

		start := make(chan struct{})
		closeDone := make(chan struct{})
		go func() {
			<-start
			rw.Close()
			close(closeDone)
		}()
		close(start)
		cancelErr := rw.Cancel()
		<-closeDone

		reader.Close()
		writer.Close()
		if cancelErr != nil {
			t.Fatalf("iteration %d: Cancel returned an error: %v", i, cancelErr)
		}
	}
}

func TestCancelReturnsOtherErrors(t *testing.T) {
	rw := newTestRWCancel(t)

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create replacement pipe: %v", err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})

	if err := rw.closingWriter.Close(); err != nil {
		t.Fatalf("close cancellation writer: %v", err)
	}
	rw.closingWriter = reader

	err = rw.Cancel()
	if !errors.Is(err, syscall.EBADF) {
		t.Fatalf("Cancel error = %v, want %v", err, syscall.EBADF)
	}
}
