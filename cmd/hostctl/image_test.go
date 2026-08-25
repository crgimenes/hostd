package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/service"
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

// The order is the daemon's — it sorts once, so the CLI, the panel and an agent
// read the same list. The rows here are deliberately oldest first: a client
// that sorted again would flip them, and that second rule is what must not
// exist.
func TestTheOrderTheHostGaveIsKept(t *testing.T) {
	var out bytes.Buffer
	printImages(&out, "yuki.local", []api.ImageEntry{
		{Digest: "sha256:1111111111111111", Tags: []string{"site:1"}, Bytes: 1024, Created: 1000},
		{Digest: "sha256:2222222222222222", Tags: []string{"site:2"}, Bytes: 2048, Created: 2000},
	})
	text := out.String()
	if rowOf(t, text, "site:1") > rowOf(t, text, "site:2") {
		t.Fatalf("the rows came out in an order the host did not give:\n%s", text)
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

// The number an operator decides on is what nothing holds. Counting a held
// image there would invite deleting the one a running service needs.
func TestOnlyImagesNothingHoldsAreCountedAsFree(t *testing.T) {
	var out bytes.Buffer
	printImages(&out, "yuki.local", []api.ImageEntry{
		{Digest: "sha256:aaaaaaaaaaaaaaaa", Tags: []string{"site:3"}, Bytes: 1024, Created: 3000, UsedBy: api.UsedByContainer, Managed: true},
		{Digest: "sha256:bbbbbbbbbbbbbbbb", Tags: []string{"site:2"}, Bytes: 2048, Created: 2000, UsedBy: api.UsedByDeclared, Managed: true},
		{Digest: "sha256:cccccccccccccccc", Bytes: 4096, Created: 1000, Managed: true},
		// Somebody else's, unused, and eight times the size of anything of
		// ours: it must not appear in what we offer to reclaim.
		{Digest: "sha256:dddddddddddddddd", Bytes: 32768, Created: 500},
	})
	text := out.String()
	if !strings.Contains(text, "1 of ours held by nothing, 4.0 KiB") {
		t.Fatalf("the reclaimable total is not just ours and unheld:\n%s", text)
	}
	if !strings.Contains(text, "3 put here by hostd, 7.0 KiB") {
		t.Fatalf("the count of what hostd put here is wrong:\n%s", text)
	}
	// What holds it is the difference between "stop the service and it frees"
	// and "edit the tree and it frees".
	if !strings.Contains(text, api.UsedByContainer) || !strings.Contains(text, api.UsedByDeclared) {
		t.Fatalf("the rows do not say what holds them:\n%s", text)
	}
}

// A machine where everything is held has nothing to offer a cleanup, and a
// line saying "0 held by nothing" would read as an invitation to look.
func TestNothingToFreeIsNotAnnounced(t *testing.T) {
	var out bytes.Buffer
	printImages(&out, "yuki.local", []api.ImageEntry{
		{Digest: "sha256:aaaaaaaaaaaaaaaa", Tags: []string{"site:1"}, Bytes: 1024, Created: 1000, UsedBy: api.UsedByContainer},
	})
	if strings.Contains(out.String(), "held by nothing") {
		t.Fatalf("a machine with nothing to free still talks about freeing:\n%s", out.String())
	}
}

// The screen exists to answer one question — what is on this machine that
// nothing is keeping — so that number has to be on it.
func TestTheImageScreenSaysWhatIsHeldByNothing(t *testing.T) {
	view := probePanel(t)
	view.snap.Fleet[0].Images = []api.ImageEntry{
		{Digest: "sha256:1111111111111111", Tags: []string{"site:2"}, Bytes: 4 << 20, Created: 2000, UsedBy: api.UsedByContainer, Managed: true},
		{Digest: "sha256:2222222222222222", Tags: []string{"site:hostd-abc"}, Bytes: 6 << 20, Created: 1000, Managed: true},
	}
	get(t, view, "/")

	body := get(t, view, "/act/select/images/yuki").Body.String()
	if !strings.Contains(body, "held by nothing") {
		t.Fatalf("the screen does not say what is unclaimed:\n%s", body)
	}
	if !strings.Contains(body, "6.0 MiB") {
		t.Fatalf("the reclaimable size is missing:\n%s", body)
	}
}

// Another system building images on this machine is not hostd's business. They
// are shown, because what fills a disk is worth seeing, and they are never
// counted as something to reclaim.
func TestImagesSomethingElseBuiltAreShownButNotCountedAsOurs(t *testing.T) {
	view := probePanel(t)
	view.snap.Fleet[0].Images = []api.ImageEntry{
		{Digest: "sha256:1111111111111111", Tags: []string{"ours:1"}, Bytes: 1 << 20, Created: 2000, Managed: true},
		{Digest: "sha256:2222222222222222", Bytes: 9 << 20, Created: 1000},
	}
	get(t, view, "/")

	body := get(t, view, "/act/select/images/yuki").Body.String()
	if !strings.Contains(body, "1 of 2 image(s) put here by hostd") {
		t.Fatalf("the screen does not separate ours from the rest:\n%s", body)
	}
	// Present, so a full disk can be explained...
	if !strings.Contains(body, "222222222222") {
		t.Fatalf("an image another system built is not reported at all:\n%s", body)
	}
	// ...but never offered up: 9 MiB of somebody else's is not reclaimable.
	if strings.Contains(body, "9.0 MiB</span><span class=\"of\">reclaimable") {
		t.Fatalf("somebody else's image was counted as reclaimable:\n%s", body)
	}
}

// The image screen is about what a machine is STORING, not what it is doing.
// The log and the window buttons govern nothing here.
func TestTheImageScreenCarriesNoLogAndNoWindowButtons(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	body := get(t, view, "/act/select/images/yuki").Body.String()
	if !strings.Contains(body, `data-log="off"`) {
		t.Fatalf("the image screen still asks for the log pane:\n%s", body)
	}
	if strings.Contains(body, `data-act="window/3600"`) {
		t.Fatalf("the image screen offers a window control that governs nothing:\n%s", body)
	}
}

// Before the first answer the panel knows nothing, and "this machine holds no
// images" is a claim about the machine rather than about the panel.
func TestAnImageScreenNotAnsweredYetDoesNotCallTheMachineEmpty(t *testing.T) {
	view := probePanel(t)
	get(t, view, "/")

	body := get(t, view, "/act/select/images/yuki").Body.String()
	if !strings.Contains(body, "asking") {
		t.Fatalf("a screen with no answer yet does not say it is waiting:\n%s", body)
	}
}

// Images are fetched for the machine whose screen is open and no other: a fleet
// asked every two seconds for lists nobody reads spends the round on them.
func TestOnlyTheOpenImageScreenIsAskedForImages(t *testing.T) {
	asked := imagesHost(viewState{kind: "host", host: "yuki"})
	if asked != "" {
		t.Fatalf("a machine nobody opened the images of was asked: %q", asked)
	}
	asked = imagesHost(viewState{kind: "images", host: "yuki"})
	if asked != "yuki" {
		t.Fatalf("the open screen's machine was not asked: %q", asked)
	}
}

// A daemon too old to know image.list, or a machine with no runtime, answers
// everything else perfectly well. That failure belongs on the image screen, not
// over the services and charts that did answer.
func TestAnImageFailureDoesNotBlankTheRestOfTheMachine(t *testing.T) {
	view := probePanel(t)
	view.snap.Fleet[0].ImagesError = "unknown-operation: this hostd does not implement \"image.list\""
	get(t, view, "/")

	body := get(t, view, "/act/select/images/yuki").Body.String()
	if !strings.Contains(body, "image.list") {
		t.Fatalf("the image screen does not say why it is empty:\n%s", body)
	}
	back := get(t, view, "/act/select/host/yuki").Body.String()
	if !strings.Contains(back, "caddy") {
		t.Fatalf("the machine lost its services to an image failure:\n%s", back)
	}
}

// A dry run has to be unmistakable: an operator who reads it as "done" goes
// away believing a disk was freed that was not.
func TestAPrunePlanSaysNothingHappened(t *testing.T) {
	var out bytes.Buffer
	printPrune(&out, "yuki.local", api.ImagePrune{
		Keep: 3,
		Kept: 6,
		Remove: []api.ImageChange{
			{Digest: "sha256:1111111111111111", Repository: "site", Tags: []string{"site:hostd-111"}, Bytes: 2048},
		},
	})
	text := out.String()
	if !strings.Contains(text, "nothing was removed") {
		t.Fatalf("a plan does not say it changed nothing:\n%s", text)
	}
	if !strings.Contains(text, "-allow-destructive") {
		t.Fatalf("a plan does not say how to go ahead:\n%s", text)
	}
	if !strings.Contains(text, "would remove") {
		t.Fatalf("the row does not say it is a proposal:\n%s", text)
	}
}

// What the runtime refused belongs on its own row: counting it as freed would
// report a disk that did not move.
func TestAPruneReportsWhatWouldNotGo(t *testing.T) {
	var out bytes.Buffer
	printPrune(&out, "yuki.local", api.ImagePrune{
		Keep:    3,
		Kept:    2,
		Applied: true,
		Remove: []api.ImageChange{
			{Digest: "sha256:1111111111111111", Bytes: 2048, Removed: true},
			{Digest: "sha256:2222222222222222", Bytes: 4096, Problem: "image is being used by running container abc"},
		},
	})
	text := out.String()
	if !strings.Contains(text, "being used by running container") {
		t.Fatalf("the refusal is not reported:\n%s", text)
	}
	// 2048 freed, not 6144: what did not go did not free anything.
	if !strings.Contains(text, "2.0 KiB freed") {
		t.Fatalf("the freed total counts an image that is still there:\n%s", text)
	}
}

// A machine with nothing to clean says so plainly rather than printing an empty
// table an operator has to interpret.
func TestAPruneWithNothingToDoSaysSo(t *testing.T) {
	var out bytes.Buffer
	printPrune(&out, "yuki.local", api.ImagePrune{Keep: 3, Kept: 5})
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Fatalf("an empty plan prints nothing readable:\n%s", out.String())
	}
}

// A push moves bytes and moves a tag, and changes nothing about what is
// running. Nothing else in the system says so — the next apply reports
// "nothing to change" because the declaration reads exactly as it did — so the
// command that just pushed has to say it, with the line that would.
func TestAPushSaysItIsNotADeploy(t *testing.T) {
	var out bytes.Buffer
	printNotADeploy(&out, api.Image{Name: "site:laptop", Ref: "site:hostd-325279faa69a", Digest: "sha256:1640bc"})
	text := out.String()
	for _, want := range []string{
		"not a deploy",
		service.Extension,
		`(tuple "image" "site:hostd-325279faa69a")`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the push output does not carry %q:\n%s", want, text)
		}
	}
}

// An older daemon answers without the stamp. Inventing one from the tag it came
// under would print a declaration that pins the wrong thing, which is worse
// than printing less.
func TestAPushWithoutAStampSaysNothingRatherThanGuessing(t *testing.T) {
	var out bytes.Buffer
	printNotADeploy(&out, api.Image{Name: "site:laptop", Digest: "sha256:1640bc"})
	if out.Len() != 0 {
		t.Fatalf("a push with no stamp advised anyway:\n%s", out.String())
	}
}
