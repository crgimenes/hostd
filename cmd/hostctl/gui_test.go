package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crgimenes/glaze"
	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/supervisor"
)

// What the window shows is what this returns, so the shape is worth pinning
// even where no machine answers: the page reads fields by name, and a rename
// here is a blank panel there.
func TestTheFleetAnswerCarriesWhatThePageReads(t *testing.T) {
	view := &panel{
		opt:     options{remote: "hostd -stdio"},
		hosts:   []string{"machine.invalid"},
		clients: map[string]*api.Client{},
	}
	answer := view.Fleet(300, 0, 0, map[string]uint64{})
	if len(answer) != 1 {
		t.Fatalf("got %d machines, expected one entry per machine asked about", len(answer))
	}
	// A machine that did not answer says so in its own entry: which host is
	// missing is part of the picture, not an error that hides the rest.
	if answer[0].Host != "machine.invalid" || answer[0].Error == "" {
		t.Fatalf("a machine that cannot be reached came back as %#v", answer[0])
	}

	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("the page could not be given the answer: %v", err)
	}
	for _, field := range []string{`"host"`, `"error"`, `"services"`, `"metrics"`, `"lines"`, `"since"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("the answer has no %s, which the page reads by name: %s", field, encoded)
		}
	}
}

// The types inside the answer are the daemon's own, and they are named for the
// Filo wire. Without a json name to match, the page would be handed Go field
// names and read every one of them as undefined — a panel that renders, blank.
func TestWhatIsInsideTheAnswerIsNamedTheWayThePageReadsIt(t *testing.T) {
	encoded, err := json.Marshal(fleetHost{
		Host:     "yuki",
		Services: []supervisor.Status{{Name: "caddy", State: "running", Since: 1, Every: "10s"}},
		Metrics:  []metrics.Series{{Scope: "host", Metric: "cpu-percent", Points: []metrics.Point{{TimeMS: 1, Value: 2}}}},
		Lines:    []api.LogLine{{Seq: 1, Time: 2, Service: "caddy", Stream: "out", Run: "3", Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		`"name"`, `"state"`, `"since-ms"`, `"runs"`, `"every"`, `"orphan"`, `"restarts"`, `"image"`,
		`"scope"`, `"metric"`, `"points"`, `"time-ms"`, `"value"`,
		`"seq"`, `"service"`, `"stream"`, `"run"`, `"text"`,
	} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("the answer has no %s, which the page reads by name: %s", field, encoded)
		}
	}
}

// The panel is served from memory: there is no path on the operator's machine
// it can reach, because there is no filesystem behind it.
func TestTheUIIsServedFromWhatIsEmbedded(t *testing.T) {
	page := serveUI(&glaze.SchemeRequest{URL: uiOrigin + "index.html"})
	if page == nil || !strings.Contains(string(page.Body), "hostd") {
		t.Fatal("the panel's own page is not served")
	}
	if page.MIMEType != "text/html; charset=utf-8" {
		t.Fatalf("the page is served as %q", page.MIMEType)
	}
	if serveUI(&glaze.SchemeRequest{URL: uiOrigin + "../../etc/passwd"}) != nil {
		t.Fatal("a request climbed out of what is embedded")
	}
	if serveUI(&glaze.SchemeRequest{URL: uiOrigin + "nothing.js"}) != nil {
		t.Fatal("a file that does not exist was answered")
	}
}

// Every file the page asks for has to be in the binary: a panel that works on
// the machine it was built on and nowhere else is a panel nobody can ship.
func TestEveryAssetThePageAsksForIsEmbedded(t *testing.T) {
	index, err := ui.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	for _, name := range []string{"app.css", "app.js"} {
		if !strings.Contains(string(index), name) {
			t.Fatalf("the page does not reference %s", name)
		}
		_, err = ui.ReadFile(filepath.Join("ui", name))
		if err != nil {
			t.Fatalf("%s is referenced and not embedded: %v", name, err)
		}
	}
	// Nothing from the network: the panel is one binary, and a page that
	// fetches a stylesheet, a font or a script from somebody's CDN is a page
	// that breaks on the laptop that is offline when a machine goes down.
	for _, name := range []string{"ui/index.html", "ui/app.css", "ui/app.js"} {
		content, readErr := ui.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		for _, forbidden := range []string{"http://", "https://", "//cdn"} {
			if strings.Contains(string(content), forbidden) {
				t.Fatalf("%s reaches outside the binary: %s", name, forbidden)
			}
		}
	}
}
