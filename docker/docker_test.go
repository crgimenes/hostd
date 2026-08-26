package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A fake that answers like the runtime does, including refusing what the real
// one refuses. A fake that always says yes makes the failure path untestable,
// and the failure path is the one that matters when a deploy goes wrong.
func fakeRuntime(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	// Short by construction: the socket path has a hard limit and macOS
	// temporary directories are already long.
	socket := filepath.Join(t.TempDir(), "d.sock")
	if len(socket) > 100 {
		t.Skipf("temporary directory makes a socket path of %d bytes", len(socket))
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second},
	}
	server.Start()
	t.Cleanup(server.Close)

	return &Client{socket: socket, http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

// What hostd asks for is what the container gets: no host network, no extra
// capabilities, no privileged mode, and the runtime told not to restart
// anything, because restarting is hostd's job and two supervisors flap.
func TestCreateDeniesWhatWasNotDeclared(t *testing.T) {
	var body map[string]any
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&body)
		if err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Id":"abc123"}`))
	}))

	id, err := client.Create(context.Background(), Spec{
		Name:  "hostd-site",
		Image: "site@sha256:abc",
		Ports: []Port{{HostPort: 8080, ContainerPort: 80}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "abc123" {
		t.Fatalf("the runtime's id came back as %q", id)
	}

	host, ok := body["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("no HostConfig in the request: %#v", body)
	}
	if host["Privileged"] != false {
		t.Fatal("the container was asked for privileged")
	}
	if host["NetworkMode"] != "bridge" {
		t.Fatalf("the network is %v, expected the container's own", host["NetworkMode"])
	}
	policy, _ := host["RestartPolicy"].(map[string]any)
	if policy["Name"] != "no" {
		t.Fatalf("the runtime was left free to restart: %v", policy)
	}
	caps, _ := host["CapAdd"].([]any)
	if len(caps) != 0 {
		t.Fatalf("capabilities were added: %v", caps)
	}
	options, _ := host["SecurityOpt"].([]any)
	if len(options) != 1 || options[0] != "no-new-privileges" {
		t.Fatalf("security options are %v", options)
	}
}

// A port with no address reaches the machine on loopback only: publishing to
// the world is a decision an operator makes on purpose.
func TestAPortWithNoAddressBindsToLoopback(t *testing.T) {
	var body map[string]any
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"Id":"abc"}`))
	}))

	_, err := client.Create(context.Background(), Spec{
		Name:  "hostd-site",
		Image: "site:1",
		Ports: []Port{
			{HostPort: 8080, ContainerPort: 80},
			{HostIP: "0.0.0.0", HostPort: 9000, ContainerPort: 9000, Protocol: "udp"},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	host, _ := body["HostConfig"].(map[string]any)
	bindings, _ := host["PortBindings"].(map[string]any)

	loopback, _ := bindings["80/tcp"].([]any)
	if len(loopback) != 1 {
		t.Fatalf("80/tcp is bound as %v", bindings["80/tcp"])
	}
	first, _ := loopback[0].(map[string]any)
	if first["HostIp"] != "127.0.0.1" || first["HostPort"] != "8080" {
		t.Fatalf("the default binding is %v, expected loopback", first)
	}

	published, _ := bindings["9000/udp"].([]any)
	if len(published) != 1 {
		t.Fatalf("9000/udp is bound as %v", bindings["9000/udp"])
	}
	second, _ := published[0].(map[string]any)
	if second["HostIp"] != "0.0.0.0" {
		t.Fatalf("an address written on purpose became %v", second)
	}
}

// The runtime says what went wrong in its own words; a status code the
// operator has to look up teaches nothing.
func TestTheRuntimesOwnMessageComesBack(t *testing.T) {
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Conflict. The container name \"/hostd-site\" is already in use"}`))
	}))

	_, err := client.Create(context.Background(), Spec{Name: "hostd-site", Image: "site:1"})
	if err == nil {
		t.Fatal("a refused create was reported as success")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("the runtime's reason was lost: %v", err)
	}
}

// A missing container or image is its own answer, so a caller can tell "not
// there" from "could not ask".
func TestAMissingThingIsNotFound(t *testing.T) {
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such image: site:1"}`))
	}))

	_, err := client.ImageDigest(context.Background(), "site:1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing image came back as %v", err)
	}
}

// Stopping something that is already gone is what the supervisor wants to
// happen: the end state is the same, and failing would make a stop that raced
// with an exit look like a problem.
func TestStoppingWhatIsAlreadyGoneIsNotAFailure(t *testing.T) {
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container"}`))
	}))

	err := client.Stop(context.Background(), "abc", time.Second)
	if err != nil {
		t.Fatalf("stopping a gone container failed: %v", err)
	}
	err = client.Remove(context.Background(), "abc")
	if err != nil {
		t.Fatalf("removing a gone container failed: %v", err)
	}
}

func frame(stream byte, text string) []byte {
	out := make([]byte, 8, 8+len(text))
	out[0] = stream
	binary.BigEndian.PutUint32(out[4:], uint32(len(text)))
	return append(out, text...)
}

// The runtime frames both streams in one connection, and the operator needs to
// know which one a line came from: stderr is where a service says it is dying.
func TestLogsSeparateTheTwoStreams(t *testing.T) {
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("timestamps") != "true" {
			t.Error("the log request did not ask for timestamps, so a resume could not be exact")
		}
		_, _ = w.Write(frame(1, "2026-08-23T10:00:00.000000001Z listening on 80\n"))
		_, _ = w.Write(frame(2, "2026-08-23T10:00:01.000000002Z cannot open the database\n"))
	}))

	var lines []Line
	err := client.Logs(context.Background(), "abc", time.Time{}, func(line Line) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %#v", len(lines), lines)
	}
	if lines[0].Stream != "stdout" || lines[0].Text != "listening on 80" {
		t.Fatalf("the first line came back as %#v", lines[0])
	}
	if lines[1].Stream != "stderr" || lines[1].Text != "cannot open the database" {
		t.Fatalf("the second line came back as %#v", lines[1])
	}
	if lines[0].At.IsZero() || !lines[1].At.After(lines[0].At) {
		t.Fatalf("the runtime's own clock was lost: %v then %v", lines[0].At, lines[1].At)
	}
}

// Resuming asks for what came after the last line already stored, or the
// restart would replay a whole log into the timeline.
func TestFollowingResumesFromWhereItStopped(t *testing.T) {
	var since string
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since = r.URL.Query().Get("since")
		_, _ = w.Write(nil)
	}))

	at := time.Unix(1787000000, 123456789)
	err := client.Logs(context.Background(), "abc", at, func(Line) error { return nil })
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if since != "1787000000.123456789" {
		t.Fatalf("resumed from %q, expected the exact instant", since)
	}
}

// A frame header read out of step would otherwise ask for whatever size the
// bytes happened to spell.
func TestAnImpossibleFrameIsRefusedRatherThanAllocated(t *testing.T) {
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		header := make([]byte, 8)
		header[0] = 1
		binary.BigEndian.PutUint32(header[4:], 1<<30)
		_, _ = w.Write(header)
	}))

	err := client.Logs(context.Background(), "abc", time.Time{}, func(Line) error { return nil })
	if err == nil {
		t.Fatal("a frame claiming a gigabyte was accepted")
	}
}

// The real runtime, where there is one. Everything above runs anywhere; this
// is the one that needs a machine with containers, and it skips rather than
// fails where there is none.
func TestTheRealRuntimeAnswers(t *testing.T) {
	client, err := Open()
	if errors.Is(err, ErrNoRuntime) {
		t.Skip("this machine has no container runtime")
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = client.Ping(ctx)
	if err != nil {
		t.Skipf("the runtime socket is there but does not answer: %v", err)
	}
	_, err = client.List(ctx, "hostd.service=")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

// Services find each other by name on the host's own network, which is what
// lets an application publish nothing to the machine and still be reachable by
// whatever answers the internet.
func TestCreateJoinsTheSharedNetworkUnderTheServiceName(t *testing.T) {
	var body map[string]any
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"Id":"abc"}`))
	}))

	_, err := client.Create(context.Background(), Spec{
		Name:    "hostd-site",
		Image:   "site:1",
		Network: "hostd",
		Alias:   "site",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	networking, ok := body["NetworkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("the container was not put on a network: %#v", body)
	}
	endpoints, _ := networking["EndpointsConfig"].(map[string]any)
	endpoint, ok := endpoints["hostd"].(map[string]any)
	if !ok {
		t.Fatalf("the endpoint is %#v", endpoints)
	}
	aliases, _ := endpoint["Aliases"].([]any)
	if len(aliases) != 1 || aliases[0] != "site" {
		t.Fatalf("the service answers to %v, expected its own name", aliases)
	}
}

// Named storage and a path from the machine are different things, and the
// request has to say which is which.
func TestMountsSayWhatTheyAre(t *testing.T) {
	var body map[string]any
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"Id":"abc"}`))
	}))

	_, err := client.Create(context.Background(), Spec{
		Name:  "hostd-site",
		Image: "site:1",
		Mounts: []Mount{
			{Source: "hostd-site-certs", Target: "/data", Named: true},
			{Source: "/srv/www", Target: "/srv/www", ReadOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	host, _ := body["HostConfig"].(map[string]any)
	mounts, _ := host["Mounts"].([]any)
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts: %#v", len(mounts), host["Mounts"])
	}
	named, _ := mounts[0].(map[string]any)
	if named["Type"] != "volume" || named["Source"] != "hostd-site-certs" || named["ReadOnly"] != false {
		t.Fatalf("named storage came out as %#v", named)
	}
	bind, _ := mounts[1].(map[string]any)
	if bind["Type"] != "bind" || bind["Target"] != "/srv/www" || bind["ReadOnly"] != true {
		t.Fatalf("a path from the machine came out as %#v", bind)
	}
}

// Two daemons starting at once both find the network missing; the one that
// loses the race is told it exists, which is the state it wanted.
func TestEnsureNetworkAcceptsLosingTheRace(t *testing.T) {
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"network hostd not found"}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"network with name hostd already exists"}`))
	}))

	err := client.EnsureNetwork(context.Background(), "hostd")
	if err != nil {
		t.Fatalf("losing the race was reported as a failure: %v", err)
	}
}

// Marking is a tag, and taking a tag back must not take the image with it while
// another name still points there. The image borrowed here is one that already
// has a name of its own, so the reference removed is only ever ours.
func TestATagCanBeAddedAndTakenBackWithoutLosingTheImage(t *testing.T) {
	client, err := Open()
	if errors.Is(err, ErrNoRuntime) {
		t.Skip("this machine has no container runtime")
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err = client.Ping(ctx)
	if err != nil {
		t.Skipf("the runtime socket is there but does not answer: %v", err)
	}
	held, err := client.Images(ctx)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	var source ImageSummary
	for _, image := range held {
		if len(image.Tags) > 0 {
			source = image
			break
		}
	}
	if source.Digest == "" {
		t.Skip("this machine holds no already-named image to borrow")
	}

	const probe = "hostd-tag-probe:1"
	err = client.Tag(ctx, source.Digest, "hostd-tag-probe", "1")
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	t.Cleanup(func() { _ = client.RemoveImage(context.Background(), probe) })

	after, err := client.Images(ctx)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if !slices.Contains(tagsOf(after, source.Digest), probe) {
		t.Fatalf("the mark is not on the image: %v", tagsOf(after, source.Digest))
	}

	err = client.RemoveImage(ctx, probe)
	if err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	again, err := client.Images(ctx)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	names := tagsOf(again, source.Digest)
	if names == nil {
		t.Fatal("removing our own tag took the whole image with it")
	}
	if slices.Contains(names, probe) {
		t.Fatalf("the mark is still there: %v", names)
	}
}

func tagsOf(held []ImageSummary, digest string) []string {
	for _, image := range held {
		if image.Digest == digest {
			if image.Tags == nil {
				return []string{}
			}
			return image.Tags
		}
	}
	return nil
}

// The stream asks only about containers carrying the label, and each
// announcement arrives with the container, the instant and the attributes —
// exitCode is where the runtime puts the code a death ended with.
func TestEventsFollowTheRuntimesAnnouncements(t *testing.T) {
	var asked url.Values
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			http.NotFound(w, r)
			return
		}
		asked = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"Type":"container","Action":"die","id":"abc123def","Actor":{"ID":"abc123def","Attributes":{"hostd.service":"caddy","exitCode":"137"}},"timeNano":1700000000000000001}` + "\n" +
				`{"Type":"container","Action":"oom","id":"abc123def","Actor":{"ID":"abc123def","Attributes":{"hostd.service":"caddy"}},"timeNano":1700000000000000002}` + "\n"))
	}))

	var seen []Event
	err := client.Events(context.Background(), time.Unix(5, 7), "hostd.service", func(e Event) error {
		seen = append(seen, e)
		return nil
	})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if !strings.Contains(asked.Get("filters"), `"hostd.service"`) {
		t.Fatalf("the stream does not filter by the label: %q", asked.Get("filters"))
	}
	if asked.Get("since") != "5.000000007" {
		t.Fatalf("a resumed stream does not say where it stopped: %q", asked.Get("since"))
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 announcements, got %d", len(seen))
	}
	first := seen[0]
	if first.Action != "die" || first.ID != "abc123def" || first.Attributes["exitCode"] != "137" {
		t.Fatalf("the announcement lost something on the way: %+v", first)
	}
	if first.At.UnixNano() != 1700000000000000001 {
		t.Fatalf("the announcement lost its instant: %v", first.At)
	}
	if seen[1].Action != "oom" {
		t.Fatalf("the second announcement is %q", seen[1].Action)
	}
}

// A pull's failure arrives INSIDE the 200 stream, so success is only the
// stream ending with no error line in it.
func TestPullReadsTheErrorOutOfTheStream(t *testing.T) {
	answer := `{"status":"Pulling from library/caddy"}` + "\n"
	client := fakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/images/create") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("fromImage") != "caddy" || r.URL.Query().Get("tag") != "2-alpine" {
			t.Errorf("the pull asks for the wrong image: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))

	err := client.Pull(context.Background(), "caddy:2-alpine")
	if err != nil {
		t.Fatalf("a clean stream was read as failure: %v", err)
	}

	answer = `{"status":"Pulling"}` + "\n" + `{"error":"pull access denied"}` + "\n"
	err = client.Pull(context.Background(), "caddy:2-alpine")
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("the error inside the stream was not read: %v", err)
	}
}
