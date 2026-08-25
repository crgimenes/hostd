package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/api"
)

func rowOf(t *testing.T, out string, want string) int {
	t.Helper()
	for i, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return i
		}
	}
	t.Fatalf("no line carries %q:\n%s", want, out)
	return -1
}

// The rows are versions of the same things, so the newest comes first: right
// after a deploy, the image the operator is looking for is the one that just
// arrived.
func TestImagesArePrintedNewestFirst(t *testing.T) {
	var out bytes.Buffer
	printImages(&out, "yuki.local", []api.ImageEntry{
		{Digest: "sha256:1111111111111111", Tags: []string{"site:1"}, Bytes: 1024, Created: 1000},
		{Digest: "sha256:2222222222222222", Tags: []string{"site:2"}, Bytes: 2048, Created: 2000},
	})
	text := out.String()
	if rowOf(t, text, "site:2") > rowOf(t, text, "site:1") {
		t.Fatalf("the older image is printed first:\n%s", text)
	}
	// What a machine spent its disk on is the reason to look at this at all.
	if !strings.Contains(text, "2 images, 3.0 KiB") {
		t.Fatalf("the total is missing:\n%s", text)
	}
}

// An image no tag names is still startable by digest, which is exactly what a
// declaration pinned to one does. Printing an empty cell would read as a broken
// row rather than as the previous version it is.
func TestAnUntaggedImageIsPrintedAsAVersion(t *testing.T) {
	var out bytes.Buffer
	printImages(&out, "yuki.local", []api.ImageEntry{
		{Digest: "sha256:33333333333333333333", Bytes: 512, Created: 1000},
	})
	text := out.String()
	if !strings.Contains(text, "<untagged>") {
		t.Fatalf("an untagged image is printed without saying so:\n%s", text)
	}
	// Short enough to read, long enough to name one image on one machine.
	if !strings.Contains(text, "333333333333") {
		t.Fatalf("the digest is missing, so the row names nothing:\n%s", text)
	}
	if strings.Contains(text, "sha256:") {
		t.Fatalf("the digest carries a prefix every row would repeat:\n%s", text)
	}
}

// A machine that holds nothing says so: an empty table reads as a failure to
// answer.
func TestAMachineHoldingNoImagesSaysSo(t *testing.T) {
	var out bytes.Buffer
	printImages(&out, "yuki.local", nil)
	if !strings.Contains(out.String(), "no images") {
		t.Fatalf("an empty answer prints nothing an operator can read:\n%s", out.String())
	}
}
