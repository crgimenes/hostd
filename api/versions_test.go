package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/service"
)

func heldImage(digest string, minutes int, tags ...string) docker.ImageSummary {
	return docker.ImageSummary{
		Digest:  "sha256:" + digest,
		Tags:    tags,
		Bytes:   1024,
		Created: time.Date(2026, 8, 25, 10, minutes, 0, 0, time.UTC),
	}
}

func refs(found ServiceVersions) []string {
	out := make([]string, 0, len(found.Versions))
	for _, version := range found.Versions {
		out = append(out, version.Ref)
	}
	return out
}

// The whole point of the command: the declaration names a tag, the tag moved,
// and what has to be written down to go back is the stamp — never the tag,
// which is what re-declaring would achieve nothing.
func TestTheVersionToDeclareIsTheStampAndNotTheMovingTag(t *testing.T) {
	held := []docker.ImageSummary{
		heldImage("bbbbbbbbbbbb", 30, "site:laptop", "site:hostd-bbbbbbbbbbbb"),
		heldImage("aaaaaaaaaaaa", 10, "site:hostd-aaaaaaaaaaaa"),
	}
	found := versionsOf(service.Service{Name: "site", Image: "site:laptop"}, held, "sha256:bbbbbbbbbbbb")

	want := []string{"site:hostd-bbbbbbbbbbbb", "site:hostd-aaaaaaaaaaaa"}
	if !slices.Equal(refs(found), want) {
		t.Fatalf("refs = %v, want %v", refs(found), want)
	}
	if found.Repository != "site" {
		t.Fatalf("repository = %q, want site", found.Repository)
	}
	if !found.Versions[0].Declared || !found.Versions[0].Running {
		t.Fatalf("the newest version is the one declared and running: %+v", found.Versions[0])
	}
	if found.Versions[1].Declared || found.Versions[1].Running {
		t.Fatalf("the older version is neither declared nor running: %+v", found.Versions[1])
	}
}

// A version a later push displaced keeps the stamp and loses the moving tag.
// That is the version a rollback is for, so it has to stay in the list.
func TestADisplacedVersionIsStillOnTheList(t *testing.T) {
	held := []docker.ImageSummary{
		heldImage("cccccccccccc", 40, "site:laptop", "site:hostd-cccccccccccc"),
		heldImage("dddddddddddd", 20, "site:hostd-dddddddddddd"),
	}
	found := versionsOf(service.Service{Name: "site", Image: "site:laptop"}, held, "")

	if len(found.Versions) != 2 {
		t.Fatalf("both versions belong to the line: %v", refs(found))
	}
	if slices.ContainsFunc(found.Versions, func(v ServiceVersion) bool { return v.Running }) {
		t.Fatal("nothing is running, so no version may be marked running")
	}
}

// Once a rollback has happened the declaration names a stamp. The line of
// versions must still be found, or the way back from the way back is gone.
func TestARolledBackDeclarationStillFindsItsLine(t *testing.T) {
	held := []docker.ImageSummary{
		heldImage("ffffffffffff", 50, "site:laptop", "site:hostd-ffffffffffff"),
		heldImage("eeeeeeeeeeee", 25, "site:hostd-eeeeeeeeeeee"),
	}
	found := versionsOf(service.Service{Name: "site", Image: "site:hostd-eeeeeeeeeeee"}, held, "")

	if found.Repository != "site" {
		t.Fatalf("repository = %q, want site", found.Repository)
	}
	if len(found.Versions) != 2 {
		t.Fatalf("both versions belong to the line: %v", refs(found))
	}
	if !found.Versions[1].Declared {
		t.Fatalf("the older version is the one declared: %+v", found.Versions[1])
	}
}

// A declaration pinned to a digest names no repository of its own. The line is
// recovered from the stamp of the image that digest resolves to, which is what
// the stamp was put there to carry.
func TestADigestDeclarationFindsItsLineThroughTheStamp(t *testing.T) {
	held := []docker.ImageSummary{
		heldImage("111111111111", 50, "site:laptop", "site:hostd-111111111111"),
		heldImage("222222222222", 25, "site:hostd-222222222222"),
	}
	found := versionsOf(service.Service{Name: "site", Image: "222222222222"}, held, "")

	if found.Repository != "site" {
		t.Fatalf("repository = %q, want site", found.Repository)
	}
	if !found.Versions[1].Declared {
		t.Fatalf("the short digest names the older version: %+v", found.Versions[1])
	}
}

// An image running under a repository the declaration no longer names is the
// declaration having changed since the container started. Hiding it would hide
// exactly what somebody about to move versions has to know.
func TestAnImageRunningOutsideTheLineIsStillReported(t *testing.T) {
	held := []docker.ImageSummary{
		heldImage("333333333333", 50, "site:laptop", "site:hostd-333333333333"),
		heldImage("444444444444", 25, "other:1", "other:hostd-444444444444"),
	}
	found := versionsOf(service.Service{Name: "site", Image: "site:laptop"}, held, "sha256:444444444444")

	at := slices.IndexFunc(found.Versions, func(v ServiceVersion) bool { return v.Running })
	if at < 0 {
		t.Fatalf("the running image is missing: %v", refs(found))
	}
	if found.Versions[at].Declared {
		t.Fatal("what is running is not what is declared, and the two must not be conflated")
	}
}

// A declaration naming an image this machine does not hold marks no row. The
// client reads that as "a start would fail", which is true and is better found
// here than at the next apply.
func TestADeclaredImageThatIsNotHereMarksNothing(t *testing.T) {
	held := []docker.ImageSummary{heldImage("555555555555", 50, "site:hostd-555555555555")}
	found := versionsOf(service.Service{Name: "site", Image: "site:laptop"}, held, "")

	if len(found.Versions) != 1 {
		t.Fatalf("the version on the machine is still listed: %v", refs(found))
	}
	if found.Versions[0].Declared {
		t.Fatal("nothing here is what the declaration names")
	}
}

// An image that predates the stamp carries no mark, and the only immutable name
// left for it is the digest. Offering the moving tag instead would offer a
// rollback that does not roll back.
func TestAnUnstampedVersionIsPinnedByDigest(t *testing.T) {
	held := []docker.ImageSummary{heldImage("666666666666", 50, "site:laptop")}
	found := versionsOf(service.Service{Name: "site", Image: "site:laptop"}, held, "")

	if found.Versions[0].Ref != "sha256:666666666666" {
		t.Fatalf("ref = %q, want the digest", found.Versions[0].Ref)
	}
	if found.Repository != "site" {
		t.Fatalf("repository = %q, want site", found.Repository)
	}
}

func TestTheStampIsReadWholeAndAsARepository(t *testing.T) {
	tags := []string{"registry.example:5000/site:laptop", "registry.example:5000/site:hostd-abc123abc123"}
	tag, marked := managedTag(tags)
	if !marked || tag != "registry.example:5000/site:hostd-abc123abc123" {
		t.Fatalf("managedTag = %q, %v", tag, marked)
	}
	repo, marked := markedRepository(tags)
	if !marked || repo != "registry.example:5000/site" {
		t.Fatalf("markedRepository = %q, %v", repo, marked)
	}
}

// The whole op over the wire, against a real runtime: the declaration is read
// from disk, the images from the runtime, and what comes back is the stamp to
// write down rather than the tag the file carries.
func TestAskingAServiceForItsVersionsAnswersWithTheStamp(t *testing.T) {
	runtime, image := requireImage(t)
	f := newFixture(t)
	f.server.Runtime(runtime)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	digest, err := runtime.ImageDigest(ctx, image)
	if err != nil {
		t.Fatalf("ImageDigest: %v", err)
	}
	repo := RepositoryOf(image)
	stamp := ManagedTag("abcdef0123456789")
	err = runtime.Tag(ctx, digest, repo, stamp)
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	t.Cleanup(func() {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer removeCancel()
		_ = runtime.RemoveImage(removeCtx, repo+":"+stamp)
	})

	services := filepath.Join(f.dir, "services")
	err = os.MkdirAll(services, 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err = os.WriteFile(filepath.Join(services, "probe.filo"),
		[]byte(fmt.Sprintf(`(service (tuple "name" "probe") (tuple "image" %q))`, image)), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	client := f.client()
	defer func() { _ = client.Close() }()
	resp, err := client.Do(ctx, Request{Op: OpServiceVersions, Name: "probe"})
	if err != nil {
		t.Fatalf("service.versions: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("asking for the versions failed: %v", resp.Err())
	}
	var found ServiceVersions
	err = decodeBody(t, resp.Body, &found)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if found.Repository != repo {
		t.Fatalf("repository = %q, want %q", found.Repository, repo)
	}
	at := slices.IndexFunc(found.Versions, func(v ServiceVersion) bool { return v.Declared })
	if at < 0 {
		t.Fatalf("the declared image is on this machine and no version is marked declared: %+v", found)
	}
	if found.Versions[at].Ref != repo+":"+stamp {
		t.Fatalf("ref = %q, want the stamp %q", found.Versions[at].Ref, repo+":"+stamp)
	}
}

// A service nothing declares is not an empty list of versions: an empty list
// would read as "this service has nowhere to go back to", which is a different
// and much more alarming answer than "no such service".
func TestAskingAboutAServiceThatIsNotDeclaredSaysSo(t *testing.T) {
	runtime, _ := requireImage(t)
	f := newFixture(t)
	f.server.Runtime(runtime)

	client := f.client()
	defer func() { _ = client.Close() }()
	resp, err := client.Do(context.Background(), Request{Op: OpServiceVersions, Name: "nothing-declares-this"})
	if err != nil {
		t.Fatalf("service.versions: %v", err)
	}
	if resp.Code != CodeNotFound {
		t.Fatalf("code = %q, want %q", resp.Code, CodeNotFound)
	}
}
