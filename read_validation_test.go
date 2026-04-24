package tuntap

import (
	"errors"
	"io"
	"testing"
)

func TestValidateReadBuffers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		batchSize int
		bufs      [][]byte
		sizes     []int
		wantErr   error
	}{
		{
			name:      "accepts exact batch size",
			batchSize: 1,
			bufs:      make([][]byte, 1),
			sizes:     make([]int, 1),
		},
		{
			name:      "accepts larger slices",
			batchSize: 2,
			bufs:      make([][]byte, 3),
			sizes:     make([]int, 4),
		},
		{
			name:      "rejects short bufs",
			batchSize: 2,
			bufs:      make([][]byte, 1),
			sizes:     make([]int, 2),
			wantErr:   io.ErrShortBuffer,
		},
		{
			name:      "rejects short sizes",
			batchSize: 2,
			bufs:      make([][]byte, 2),
			sizes:     make([]int, 1),
			wantErr:   io.ErrShortBuffer,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateReadBuffers(tt.batchSize, tt.bufs, tt.sizes)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateReadBuffers(%d, len(bufs)=%d, len(sizes)=%d) error = %v, want %v", tt.batchSize, len(tt.bufs), len(tt.sizes), err, tt.wantErr)
			}
		})
	}
}
