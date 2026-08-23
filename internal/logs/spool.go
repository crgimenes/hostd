package logs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Limits on what a service can force hostd to hold.
const (
	// maxLineBytes caps one line. A service that writes a gigabyte without a
	// newline gets it split, not the supervisor's memory.
	maxLineBytes = 8 << 10
	// maxSpoolBytes is when a spool file is recycled. Output written while
	// hostd is away accumulates here, so the ceiling has to allow for an
	// absence without letting a chatty service fill the disk.
	maxSpoolBytes = 8 << 20
	// readChunk bounds a single read, so one poll cannot pull an unbounded
	// backlog into memory at once.
	readChunk = 256 << 10
)

// SpoolPath returns the file a service writes one stream to.
func SpoolPath(dir, serviceName, stream string) string {
	suffix := ".out"
	if stream == StreamErr {
		suffix = ".err"
	}
	return filepath.Join(dir, serviceName+suffix)
}

// OpenSpool opens a service's spool file for appending, creating it if needed.
// The returned file is handed to the service as stdout or stderr; hostd may
// die and be replaced without the service noticing, because nothing about this
// file depends on the supervisor being alive.
func OpenSpool(dir, serviceName, stream string) (*os.File, error) {
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return nil, err
	}
	path := SpoolPath(dir, serviceName, stream)
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- path is built from hostd's spool dir and a validated service name
}

// Tailer turns the bytes of one spool file into records.
//
// It keeps a read offset, so a restarted hostd resumes where the previous one
// stopped instead of replaying or losing what the service wrote in between.
type Tailer struct {
	path    string
	service string
	stream  string
	offset  int64
	partial []byte
}

// NewTailer reads path from the given offset. Passing 0 replays everything the
// file still holds, which is how a fresh hostd recovers the output produced
// while it was away.
func NewTailer(path, service, stream string, offset int64) *Tailer {
	return &Tailer{path: path, service: service, stream: stream, offset: offset}
}

// Offset reports where the next read will start.
func (t *Tailer) Offset() int64 {
	return t.offset
}

// Read consumes what is available and returns the complete lines found. A
// missing file is not an error: a service that has not written yet has no
// spool.
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
		// The file was recycled underneath us: start over rather than read
		// from a position that no longer means anything.
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
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	t.offset += int64(n)
	return t.consume(buf[:n], now), nil
}

// consume splits the chunk into lines, holding an unterminated tail until the
// rest of it arrives.
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
	// An unterminated line longer than the cap is emitted as-is instead of
	// being buffered forever. The service keeps running; the supervisor keeps
	// its memory.
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
	// Trailing CR comes from services that write CRLF; it would otherwise show
	// up as a stray character in every line of their output.
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
// Truncation, not rename: the service holds an open descriptor to this inode
// and appends to it. Renaming would leave it writing to a file nobody reads;
// truncating keeps its descriptor valid and its next write lands at the start.
// It only runs when everything written so far has been consumed, and reports
// how many bytes arrived in the gap so that a loss is counted rather than
// silent.
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
	if len(t.partial) > 0 {
		// Half a line is in hand; recycling now would cut it in two.
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

// RemoveSpool deletes both spool files of a service.
func RemoveSpool(dir, serviceName string) error {
	for _, stream := range []string{StreamOut, StreamErr} {
		err := os.Remove(SpoolPath(dir, serviceName, stream))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove spool: %w", err)
		}
	}
	return nil
}
