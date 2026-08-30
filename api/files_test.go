package api

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/supervisor"
)

func declare(t *testing.T, f *fixture, name, source string) {
	t.Helper()
	err := os.MkdirAll(filepath.Join(f.dir, "services"), 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err = os.WriteFile(filepath.Join(f.dir, "services", name+service.Extension), []byte(source), 0o600)
	if err != nil {
		t.Fatalf("write declaration: %v", err)
	}
}

// Asked with only the service, the listing answers the declared volumes
// themselves — the panel's first click needs no knowledge of the declaration.
// Only NAMED volumes: a bind mount is the machine's own filesystem, and the
// artifacts are the tree's.
func TestFileListAnswersTheDeclaredVolumes(t *testing.T) {
	f := newFixture(t)
	declare(t, f, "site", `(service (tuple "name" "site") (tuple "image" "site:1")
	  (tuple "volumes" (list "data:/var/lib/site" "cache:/tmp/cache" "/etc/ssl:/ssl:ro")))`)

	var entries []FileEntry
	err := call(f.client(), Request{Op: OpFileList, Service: "site"}, &entries)
	if err != nil {
		t.Fatalf("file.list: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "data" || entries[1].Name != "cache" {
		t.Fatalf("the volumes came back as %+v", entries)
	}
	for _, entry := range entries {
		if !entry.Dir {
			t.Fatalf("a volume is a directory: %+v", entry)
		}
	}
}

// A refused upload still drains the bytes that follow the request: leaving
// them in the pipe would desynchronise the connection, and the NEXT request
// would be read out of the middle of this one's payload.
func TestARefusedUploadLeavesTheConnectionUsable(t *testing.T) {
	f := newFixture(t)
	client := f.client()
	defer func() { _ = client.Close() }()

	resp, err := client.SendFile(context.Background(), "ghost", "data/x",
		bytes.NewReader(bytes.Repeat([]byte("y"), 3<<20)))
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if !resp.Failed() {
		t.Fatal("an upload for a service with no declaration was accepted")
	}
	// The same connection must still answer.
	var entries []FileEntry
	err = call(client, Request{Op: OpFileList, Service: "ghost"}, &entries)
	if err == nil || !strings.Contains(err.Error(), "no declaration") {
		t.Fatalf("the connection did not survive the refused upload: %v", err)
	}
}

// The whole loop against a real runtime and a real volume: put, list, get,
// and the escape a container could plant. The volume mountpoint belongs to
// root, so this runs where the daemon runs — sudo ./api.test on a Linux
// machine of the bench.
func TestFileOperationsAgainstARealVolume(t *testing.T) {
	runtime, _ := requireImage(t)
	if os.Geteuid() != 0 {
		t.Skip("volume mountpoints belong to root; run with sudo on a Linux machine")
	}
	f := newFixture(t)
	f.server.Runtime(runtime)
	declare(t, f, "probe", `(service (tuple "name" "probe") (tuple "image" "probe:1")
	  (tuple "volumes" (list "data:/var/lib/probe")))`)
	ctx := context.Background()
	volume := supervisor.VolumeName("probe", "data")
	err := runtime.EnsureVolume(ctx, volume, nil)
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	client := f.client()
	defer func() { _ = client.Close() }()

	resp, err := client.SendFile(ctx, "probe", "data/notes/x.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if !resp.Failed() {
		t.Fatal("an upload into a directory that does not exist was accepted")
	}

	resp, err = client.SendFile(ctx, "probe", "data/x.txt", strings.NewReader("hello"))
	if err != nil || resp.Failed() {
		t.Fatalf("SendFile: %v %v", err, resp.Err())
	}

	var entries []FileEntry
	err = call(client, Request{Op: OpFileList, Service: "probe", Name: "data"}, &entries)
	if err != nil {
		t.Fatalf("file.list: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name == "x.txt" && !entry.Dir && entry.Bytes == 5 {
			found = true
		}
		if strings.HasSuffix(entry.Name, ".hostd-upload") {
			t.Fatalf("a temporary survived the upload: %+v", entry)
		}
	}
	if !found {
		t.Fatalf("the uploaded file is not in the listing: %+v", entries)
	}

	var got bytes.Buffer
	resp, err = client.FetchFile(ctx, "probe", "data/x.txt", func(FileTransfer) (io.Writer, error) {
		return &got, nil
	})
	if err != nil || resp.Failed() {
		t.Fatalf("FetchFile: %v %v", err, resp.Err())
	}
	if got.String() != "hello" {
		t.Fatalf("round trip lost the content: %q", got.String())
	}

	// What a container could plant: a symlink pointing out of the volume. A
	// root daemon following it would hand over the machine's own files.
	mountpoint, err := runtime.VolumeMountpoint(ctx, volume)
	if err != nil {
		t.Fatalf("VolumeMountpoint: %v", err)
	}
	err = os.Symlink("/etc/hostname", filepath.Join(mountpoint, "escape"))
	if err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}
	got.Reset()
	resp, err = client.FetchFile(ctx, "probe", "data/escape", func(FileTransfer) (io.Writer, error) {
		return &got, nil
	})
	if err != nil {
		t.Fatalf("FetchFile: %v", err)
	}
	if !resp.Failed() {
		t.Fatalf("a symlink out of the volume was followed: %q", got.String())
	}
}
