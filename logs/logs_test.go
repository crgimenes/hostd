package logs

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSpoolSurvivesAndResumes(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenSpool(dir, "api", StreamOut)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer func() { _ = f.Close() }()

	_, err = f.WriteString("first\nsecond\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	tail := NewTailer(SpoolPath(dir, "api", StreamOut), "api", StreamOut, 0)
	records, err := tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 2 || records[0].Text != "first" || records[1].Text != "second" {
		t.Fatalf("unexpected records: %#v", records)
	}

	// What the service writes while hostd is away is still there when a new
	// tailer resumes from the recorded offset. This is the property that lets
	// the supervisor restart without losing output.
	_, err = f.WriteString("while hostd was away\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	resumed := NewTailer(SpoolPath(dir, "api", StreamOut), "api", StreamOut, tail.Offset())
	records, err = resumed.Read(time.Now())
	if err != nil {
		t.Fatalf("Read after resume: %v", err)
	}
	if len(records) != 1 || records[0].Text != "while hostd was away" {
		t.Fatalf("resume returned %#v", records)
	}
}

func TestTailerHoldsPartialLine(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenSpool(dir, "api", StreamOut)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer func() { _ = f.Close() }()

	tail := NewTailer(SpoolPath(dir, "api", StreamOut), "api", StreamOut, 0)
	_, err = f.WriteString("half a li")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	records, err := tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("an unterminated line was emitted early: %#v", records)
	}
	_, err = f.WriteString("ne\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	records, err = tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 1 || records[0].Text != "half a line" {
		t.Fatalf("line was not rejoined: %#v", records)
	}
}

func TestTailerSplitsOversizedLine(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenSpool(dir, "api", StreamOut)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer func() { _ = f.Close() }()

	// A service that never writes a newline must not grow hostd's memory.
	_, err = f.WriteString(strings.Repeat("x", maxLineBytes*3))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	tail := NewTailer(SpoolPath(dir, "api", StreamOut), "api", StreamOut, 0)
	records, err := tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("oversized line was not split: %d records", len(records))
	}
	for _, r := range records {
		if len(r.Text) > maxLineBytes {
			t.Fatalf("record of %d bytes exceeds the cap", len(r.Text))
		}
	}
}

func TestTailerStripsCarriageReturn(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenSpool(dir, "api", StreamOut)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("windows line\r\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	tail := NewTailer(SpoolPath(dir, "api", StreamOut), "api", StreamOut, 0)
	records, err := tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 1 || records[0].Text != "windows line" {
		t.Fatalf("carriage return was not stripped: %#v", records)
	}
}

func TestTailerHandlesRecycledFile(t *testing.T) {
	dir := t.TempDir()
	path := SpoolPath(dir, "api", StreamOut)
	f, err := OpenSpool(dir, "api", StreamOut)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer func() { _ = f.Close() }()

	_, err = f.WriteString("before recycle\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	tail := NewTailer(path, "api", StreamOut, 0)
	_, err = tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// A file that shrank means it was recycled; reading from the old offset
	// would return garbage or nothing at all.
	err = os.Truncate(path, 0)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, err = f.WriteString("after recycle\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	records, err := tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read after recycle: %v", err)
	}
	if len(records) != 1 || records[0].Text != "after recycle" {
		t.Fatalf("did not recover from a recycled file: %#v", records)
	}
}

func TestTailerMissingFileIsNotAnError(t *testing.T) {
	tail := NewTailer(SpoolPath(t.TempDir(), "api", StreamOut), "api", StreamOut, 0)
	records, err := tail.Read(time.Now())
	if err != nil {
		t.Fatalf("a service that has not written yet is not an error: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("got %d records from a missing file", len(records))
	}
}

func TestRecycleOnlyPastTheCeiling(t *testing.T) {
	dir := t.TempDir()
	path := SpoolPath(dir, "api", StreamOut)
	f, err := OpenSpool(dir, "api", StreamOut)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer func() { _ = f.Close() }()

	_, err = f.WriteString("small\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	tail := NewTailer(path, "api", StreamOut, 0)
	_, err = tail.Read(time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	lost, err := tail.Recycle()
	if err != nil {
		t.Fatalf("Recycle: %v", err)
	}
	if lost != 0 {
		t.Fatalf("lost %d bytes recycling a small file", lost)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("a file below the ceiling was recycled")
	}

	line := strings.Repeat("y", 1024) + "\n"
	for range (maxSpoolBytes / len(line)) + 1 {
		_, err = f.WriteString(line)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for {
		records, readErr := tail.Read(time.Now())
		if readErr != nil {
			t.Fatalf("Read: %v", readErr)
		}
		if len(records) == 0 {
			break
		}
	}
	_, err = tail.Recycle()
	if err != nil {
		t.Fatalf("Recycle: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("file past the ceiling was not recycled: %d bytes", info.Size())
	}
	if tail.Offset() != 0 {
		t.Fatalf("offset was not reset after recycling: %d", tail.Offset())
	}
}

func TestRemoveSpool(t *testing.T) {
	dir := t.TempDir()
	for _, stream := range []string{StreamOut, StreamErr} {
		f, err := OpenSpool(dir, "api", stream)
		if err != nil {
			t.Fatalf("OpenSpool: %v", err)
		}
		_ = f.Close()
	}
	err := RemoveSpool(dir, "api")
	if err != nil {
		t.Fatalf("RemoveSpool: %v", err)
	}
	// Removing what is already gone is not a failure.
	err = RemoveSpool(dir, "api")
	if err != nil {
		t.Fatalf("RemoveSpool is not idempotent: %v", err)
	}
}
