package api

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
)

// The push crosses two runtimes, so it is tested against a real one or not at
// all. Where the machine has none, or no small image to send, it skips.
func requireImage(t *testing.T) (*docker.Client, string) {
	t.Helper()
	client, err := docker.Open()
	if errors.Is(err, docker.ErrNoRuntime) {
		t.Skip("this machine has no container runtime")
	}
	if err != nil {
		t.Fatalf("docker.Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = client.Ping(ctx)
	if err != nil {
		t.Skipf("the runtime socket is there but does not answer: %v", err)
	}
	for _, image := range []string{"busybox:latest", "alpine:latest"} {
		_, err = client.ImageDigest(ctx, image)
		if err == nil {
			return client, image
		}
	}
	t.Skip("no small image on this machine; docker pull busybox to run this test")
	return nil, ""
}

// The image goes from the runtime that has it into the runtime that needs it,
// through the same connection the commands travel on. No registry sits in the
// middle, and the machine fetches nothing by itself.
func TestPushingAnImageLandsItOnTheOtherMachine(t *testing.T) {
	runtime, image := requireImage(t)
	f := newFixture(t)
	f.server.Runtime(runtime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	content, writer := io.Pipe()
	go func() { _ = writer.CloseWithError(runtime.Save(ctx, image, writer)) }()

	here, err := runtime.Image(ctx, image)
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	_ = here
	client := f.client()
	defer func() { _ = client.Close() }()
	resp, err := client.Push(ctx, image, here.Arch, content)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("the push failed: %v", resp.Err())
	}
	var received Image
	err = decodeBody(t, resp.Body, &received)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The id is the receiving machine's own: what proves the transfer is the
	// hash of the bytes, which both sides compute over the same stream.
	if received.Digest == "" {
		t.Fatal("the machine did not say what it now calls the image")
	}
	if len(received.Content) != 64 {
		t.Fatalf("the content hash came back as %q", received.Content)
	}
	if received.Bytes <= 0 {
		t.Fatalf("an image of %v bytes crossed the wire", received.Bytes)
	}

	// What arrived is worth a line in the timeline: an image nobody can trace
	// to a moment is an image nobody can explain later.
	found := false
	for _, record := range f.search(logs.Query{Kind: logs.EventImage}) {
		if strings.Contains(record.Text, image) {
			found = true
		}
	}
	if !found {
		t.Fatal("the image that arrived left no event")
	}
}

// A machine that runs no containers has to say so, and the bytes already on
// their way have to be read and dropped: leaving them in the pipe would leave
// the connection half-way through somebody's image.
func TestPushingToAMachineWithoutARuntimeSaysSo(t *testing.T) {
	f := newFixture(t)
	client := f.client()
	defer func() { _ = client.Close() }()

	resp, err := client.Push(context.Background(), "site:1", "amd64", strings.NewReader("not really a tar"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !resp.Failed() {
		t.Fatal("a machine with no runtime accepted an image")
	}
	if !strings.Contains(resp.Message, "container runtime") {
		t.Fatalf("the refusal does not say why: %s", resp.Message)
	}

	// The connection is still where the next request begins.
	answer, err := client.Do(context.Background(), Request{Op: OpStatus})
	if err != nil {
		t.Fatalf("the connection did not survive the refused upload: %v", err)
	}
	if answer.Failed() {
		t.Fatalf("the request after the upload failed: %v", answer.Err())
	}
}

// An image for another architecture loads and then fails to start with a
// sentence that explains nothing. Refusing it here says the real reason, and
// costs the transfer rather than the deploy.
func TestAnImageForAnotherArchitectureIsRefused(t *testing.T) {
	runtime, image := requireImage(t)
	f := newFixture(t)
	f.server.Runtime(runtime)

	client := f.client()
	defer func() { _ = client.Close() }()
	resp, err := client.Push(context.Background(), image, "s390x", strings.NewReader("bytes nobody will read"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !resp.Failed() {
		t.Fatal("an image for another architecture was accepted")
	}
	if !strings.Contains(resp.Message, "--platform") {
		t.Fatalf("the refusal does not say how to fix it: %s", resp.Message)
	}
}

// A machine with no container runtime cannot answer what it holds, and that is
// not the same answer as holding nothing: an empty list would read as a clean
// machine and invite a deploy onto one that cannot run it.
func TestListingImagesWithoutARuntimeSaysSo(t *testing.T) {
	f := newFixture(t)
	client := f.client()
	defer func() { _ = client.Close() }()

	resp, err := client.Do(context.Background(), Request{Op: OpImageList})
	if err != nil {
		t.Fatalf("image.list: %v", err)
	}
	if resp.Code != CodeUnavailable {
		t.Fatalf("a machine with no runtime answered %q, wanted %q", resp.Code, CodeUnavailable)
	}
}

// What a machine holds is what a rollback can still start, and the daemon is
// the only one who can say: hostctl runs on the operator's machine and reads
// no remote runtime of its own.
func TestListingImagesAnswersWithWhatTheRuntimeHolds(t *testing.T) {
	runtime, image := requireImage(t)
	f := newFixture(t)
	f.server.Runtime(runtime)

	client := f.client()
	defer func() { _ = client.Close() }()
	resp, err := client.Do(context.Background(), Request{Op: OpImageList})
	if err != nil {
		t.Fatalf("image.list: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("listing the images failed: %v", resp.Err())
	}
	var held []ImageEntry
	err = decodeBody(t, resp.Body, &held)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, entry := range held {
		if !slices.Contains(entry.Tags, image) {
			continue
		}
		// The digest is what a declaration pins to on this machine, and the
		// time is what tells two versions of one tag apart.
		if entry.Digest == "" {
			t.Errorf("%s is listed without the digest a declaration would pin to", image)
		}
		if entry.Created == 0 {
			t.Errorf("%s is listed without a creation time", image)
		}
		return
	}
	t.Fatalf("%s is on this machine and is not in the answer", image)
}
