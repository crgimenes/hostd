package api

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

// An image crosses the wire in frames, and what comes out has to be what went
// in — byte for byte, at any size, including sizes that straddle a frame.
func TestChunksSurviveTheWire(t *testing.T) {
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 7} {
		sent := make([]byte, size)
		_, err := rand.Read(sent)
		if err != nil {
			t.Fatalf("random: %v", err)
		}
		var wire bytes.Buffer
		err = WriteChunks(&wire, bytes.NewReader(sent))
		if err != nil {
			t.Fatalf("WriteChunks(%d): %v", size, err)
		}
		var got bytes.Buffer
		total, err := ReadChunks(bufio.NewReader(&wire), &got, nil)
		if err != nil {
			t.Fatalf("ReadChunks(%d): %v", size, err)
		}
		if total != int64(size) || !bytes.Equal(got.Bytes(), sent) {
			t.Fatalf("%d bytes went in and %d came out, equal=%v", size, total, bytes.Equal(got.Bytes(), sent))
		}
	}
}

// The bytes end where the marker says, so the connection is left where the
// next request begins rather than half-way through somebody's image.
func TestTheConnectionSurvivesTheUpload(t *testing.T) {
	var wire bytes.Buffer
	err := WriteChunks(&wire, strings.NewReader("an image"))
	if err != nil {
		t.Fatalf("WriteChunks: %v", err)
	}
	wire.WriteString("(list (tuple \"op\" \"status\"))\n")

	reader := bufio.NewReader(&wire)
	_, err = ReadChunks(reader, io.Discard, nil)
	if err != nil {
		t.Fatalf("ReadChunks: %v", err)
	}
	rest, err := readLimitedLine(reader)
	if err != nil {
		t.Fatalf("read what follows: %v", err)
	}
	if !strings.Contains(rest, "status") {
		t.Fatalf("the request after the upload came back as %q", rest)
	}
}

// A frame claiming more than the protocol sends is a header read out of step
// or an attack; either way the answer is to stop reading, not to allocate.
func TestAnImpossibleChunkIsRefused(t *testing.T) {
	_, err := ReadChunks(bufio.NewReader(strings.NewReader("999999999999\n")), io.Discard, nil)
	if err == nil {
		t.Fatal("a chunk larger than the protocol sends was accepted")
	}
}

func TestGarbageInsteadOfALengthIsRefused(t *testing.T) {
	_, err := ReadChunks(bufio.NewReader(strings.NewReader("not-a-number\n")), io.Discard, nil)
	if err == nil {
		t.Fatal("a frame with no length was accepted")
	}
	if !strings.Contains(err.Error(), "length of a chunk") {
		t.Fatalf("the error does not say what was expected: %v", err)
	}
}

// An upload that is cut short must fail rather than leave a truncated image
// looking like a whole one.
func TestATruncatedUploadFails(t *testing.T) {
	var wire bytes.Buffer
	wire.WriteString("16\n")
	wire.WriteString("only eight")
	_, err := ReadChunks(bufio.NewReader(&wire), io.Discard, nil)
	if err == nil {
		t.Fatal("a chunk that ended early was accepted")
	}
}
