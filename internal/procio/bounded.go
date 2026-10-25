// Package procio holds the bounded output buffer that subprocess adapters
// (gitlocal, localhelm and reposerver) give to os/exec, so a child process
// cannot make Diffaro hold more output than its limit.
//
// It imports nothing internal.
package procio

import (
	"bytes"
	"io"
)

// Bounded buffers subprocess output up to Limit bytes. A write that would
// pass Limit writes nothing, sets Exceeded and fails with io.ErrShortWrite,
// which stops os/exec copying the stream. With Truncate, such a write
// instead keeps the part that fits, discards the rest and succeeds, so the
// child is not stopped; use it for diagnostics that only inform a failure.
// Observed counts every byte offered, written or not. It has no WriteString,
// so every write is bounded.
type Bounded struct {
	Limit    int64
	Truncate bool
	Exceeded bool
	Observed int64
	buffer   bytes.Buffer
}

func (b *Bounded) Write(p []byte) (int, error) {
	b.Observed += int64(len(p))
	room := b.Limit - int64(b.buffer.Len())
	if int64(len(p)) <= room {
		return b.buffer.Write(p)
	}
	b.Exceeded = true
	if !b.Truncate {
		return 0, io.ErrShortWrite
	}
	if room > 0 {
		b.buffer.Write(p[:room])
	}
	return len(p), nil
}

// Bytes returns the buffered output.
func (b *Bounded) Bytes() []byte { return b.buffer.Bytes() }

// String returns the buffered output as a string.
func (b *Bounded) String() string { return b.buffer.String() }
