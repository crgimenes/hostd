package logs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Ceilings on what a service can force hostd to hold: a service that writes a
// gigabyte without a newline must cost it a split line, not its memory.
const (
	maxLineBytes = 8 << 10
	// Output written while hostd is away accumulates here, so this allows for
	// an absence without letting a chatty service fill the disk.
	maxSpoolBytes = 8 << 20
	readChunk     = 256 << 10
)

func SpoolPath(dir, serviceName, stream string) string {
	suffix := ".out"
	if stream == StreamErr {
		suffix = ".err"
	}
	return filepath.Join(dir, serviceName+suffix)
}

// The file is handed to the service as stdout or stderr, and nothing about it
// depends on hostd being alive.
func OpenSpool(dir, serviceName, stream string) (*os.File, error) {
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return nil, err
	}
	path := SpoolPath(dir, serviceName, stream)
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- path is built from hostd's spool dir and a validated service name
}

// Keeps a read offset, so a restarted hostd resumes instead of replaying or
// losing what was written while it was away.
type Tailer struct {
	path    string
	service string
	stream  string
	offset  int64
	partial []byte
}

// Offset 0 replays everything the file still holds.
func NewTailer(path, service, stream string, offset int64) *Tailer {
	return &Tailer{path: path, service: service, stream: stream, offset: offset}
}

func (t *Tailer) Offset() int64 {
	return t.offset
}

// Read consumes what is available and returns the complete lines found. A
// missing file is not an error: a service that has not written yet has no spool.
func (t *Tailer) Read(now time.Time) ([]Record, error) {
	f, err := os.Open(t.path) // #nosec G304 -- path is built from hostd's spool dir and a validated service name
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < t.offset {
		// Recycled underneath us; the old position means nothing now.
		t.offset = 0
		t.partial = nil
	}
	if info.Size() == t.offset {
		return nil, nil
	}

	_, err = f.Seek(t.offset, io.SeekStart)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, min(readChunk, int(info.Size()-t.offset)))
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	t.offset += int64(n)
	return t.consume(buf[:n], now), nil
}

// consume holds an unterminated tail until the rest of it arrives.
func (t *Tailer) consume(chunk []byte, now time.Time) []Record {
	var records []Record
	data := append(t.partial, chunk...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		records = append(records, t.record(data[:i], now))
		data = data[i+1:]
	}
	// Emitted rather than buffered forever: the supervisor keeps its memory.
	for len(data) > maxLineBytes {
		records = append(records, t.record(data[:maxLineBytes], now))
		data = data[maxLineBytes:]
	}
	t.partial = append(t.partial[:0], data...)
	return records
}

func (t *Tailer) record(line []byte, now time.Time) Record {
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes]
	}
	// Services that write CRLF would otherwise show a stray character on
	// every line.
	line = bytes.TrimSuffix(line, []byte("\r"))
	return Record{
		Time:    now,
		Service: t.service,
		Stream:  t.stream,
		Text:    string(line),
	}
}

// Recycle empties the spool file once it grows past the ceiling.
//
// Truncation, not rename: the service holds an open descriptor to this inode,
// and renaming would leave it writing to a file nobody reads. Bytes that
// arrived in the gap are reported so a loss is counted rather than silent.
func (t *Tailer) Recycle() (lost int64, err error) {
	info, err := os.Stat(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if info.Size() <= maxSpoolBytes {
		return 0, nil
	}
	// Half a line in hand; recycling now would cut it in two.
	if len(t.partial) > 0 {
		return 0, nil
	}
	lost = info.Size() - t.offset
	err = os.Truncate(t.path, 0)
	if err != nil {
		return 0, err
	}
	t.offset = 0
	return lost, nil
}

func RemoveSpool(dir, serviceName string) error {
	for _, stream := range []string{StreamOut, StreamErr} {
		err := os.Remove(SpoolPath(dir, serviceName, stream))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove spool: %w", err)
		}
	}
	return nil
}
