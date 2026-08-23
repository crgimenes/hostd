package api

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crgimenes/hostd/internal/logs"
	"github.com/crgimenes/hostd/internal/service"
	"github.com/crgimenes/hostd/internal/supervisor"
)

// fakeSupervisor answers like the real one, including failing on demand. A
// fake that always succeeds makes the error path impossible to exercise, and
// the error path is the one that matters when something is wrong at 3am.
type fakeSupervisor struct {
	mu       sync.Mutex
	statuses []supervisor.Status
	calls    []string
	failWith error
}

func (f *fakeSupervisor) Status() []supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.Status(nil), f.statuses...)
}

func (f *fakeSupervisor) StatusOf(name string) (supervisor.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.statuses {
		if s.Name == name {
			return s, nil
		}
	}
	return supervisor.Status{}, supervisor.ErrUnknownService{Name: name}
}

func (f *fakeSupervisor) act(op, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op+" "+name)
	if f.failWith != nil {
		return f.failWith
	}
	for _, s := range f.statuses {
		if s.Name == name {
			return nil
		}
	}
	return supervisor.ErrUnknownService{Name: name}
}

func (f *fakeSupervisor) Start(name string) error   { return f.act("start", name) }
func (f *fakeSupervisor) Stop(name string) error    { return f.act("stop", name) }
func (f *fakeSupervisor) Restart(name string) error { return f.act("restart", name) }

func (f *fakeSupervisor) Apply(declared []service.Service) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(declared))
	for _, s := range declared {
		out = append(out, s.Name+": applied")
	}
	return out
}

type fixture struct {
	t      *testing.T
	sup    *fakeSupervisor
	buffer *logs.Buffer
	socket string
	dir    string
	cancel context.CancelFunc
	served chan error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// The socket path has to stay under the kernel's limit, and TempDir on
	// macOS is already long, so the shortest possible name is used here.
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	f := &fixture{
		t:      t,
		sup:    &fakeSupervisor{},
		buffer: logs.NewBuffer(100),
		socket: filepath.Join(dir, "s"),
		dir:    dir,
		served: make(chan error, 1),
	}
	if len(f.socket) > maxSocketPath {
		t.Skipf("temporary directory makes a socket path of %d bytes, over the %d limit", len(f.socket), maxSocketPath)
	}
	listener, err := ListenUnix(f.socket)
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	server := NewServer(f.sup, f.buffer, filepath.Join(dir, "services"))
	go func() { f.served <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.served:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	return f
}

func (f *fixture) client() *Client {
	f.t.Helper()
	c, err := DialUnix(f.socket)
	if err != nil {
		f.t.Fatalf("DialUnix: %v", err)
	}
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDescribeReportsCapabilities(t *testing.T) {
	f := newFixture(t)
	var d Description
	err := f.client().Call(context.Background(), Request{Op: OpDescribe}, &d)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if d.Protocol == 0 || d.Schema == 0 {
		t.Fatalf("describe does not report versions: %+v", d)
	}
	// A client of a different release has to be able to ask what a daemon
	// understands before it depends on it.
	if len(d.Operations) == 0 {
		t.Fatal("describe reports no operations")
	}
}

func TestStatusRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{
		{Name: "api", State: supervisor.StateRunning, PID: 42, Restarts: 2},
		{Name: "web", State: supervisor.StateStopped},
	}
	var got []supervisor.Status
	err := f.client().Call(context.Background(), Request{Op: OpStatus}, &got)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(got) != 2 || got[0].Name != "api" || got[0].PID != 42 || got[0].Restarts != 2 {
		t.Fatalf("status did not survive the round trip: %#v", got)
	}
}

// An empty result is the most ordinary result there is, and it has to decode
// like any other.
func TestEmptyStatusRoundTrip(t *testing.T) {
	f := newFixture(t)
	var got []supervisor.Status
	err := f.client().Call(context.Background(), Request{Op: OpStatus}, &got)
	if err != nil {
		t.Fatalf("status with no services: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d statuses from a host with none", len(got))
	}
}

func TestUnknownOperationSaysHowToAsk(t *testing.T) {
	f := newFixture(t)
	resp, err := f.client().Do(context.Background(), Request{Op: "service.teleport"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.Code != CodeUnknownOp {
		t.Fatalf("code = %q, want %q", resp.Code, CodeUnknownOp)
	}
	if !strings.Contains(resp.Message, "hostctl describe") {
		t.Fatalf("the message does not say how to find out what exists: %q", resp.Message)
	}
}

func TestUnknownServiceIsNotFound(t *testing.T) {
	f := newFixture(t)
	resp, err := f.client().Do(context.Background(), Request{Op: OpServiceStart, Name: "ghost"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// The code is what a program branches on; the message is for a person.
	if resp.Code != CodeNotFound {
		t.Fatalf("code = %q, want %q", resp.Code, CodeNotFound)
	}
}

func TestOperationNeedingANameSaysSo(t *testing.T) {
	f := newFixture(t)
	resp, err := f.client().Do(context.Background(), Request{Op: OpServiceStop})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.Code != CodeInvalid {
		t.Fatalf("code = %q, want %q", resp.Code, CodeInvalid)
	}
}

// A failing supervisor must surface as a failure, not as an empty success.
func TestSupervisorFailureSurfaces(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api"}}
	f.sup.failWith = errors.New("the disk is full")
	resp, err := f.client().Do(context.Background(), Request{Op: OpServiceRestrt, Name: "api"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.Code != CodeFailed || !strings.Contains(resp.Message, "disk is full") {
		t.Fatalf("failure did not surface: %+v", resp)
	}
}

func TestServiceActionsReachTheSupervisor(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api"}}
	client := f.client()
	for _, op := range []string{OpServiceStart, OpServiceStop, OpServiceRestrt} {
		err := client.Call(context.Background(), Request{Op: op, Name: "api"}, nil)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	f.sup.mu.Lock()
	defer f.sup.mu.Unlock()
	got := strings.Join(f.sup.calls, ",")
	if got != "start api,stop api,restart api" {
		t.Fatalf("supervisor saw %q", got)
	}
}

func TestLogSearch(t *testing.T) {
	f := newFixture(t)
	f.buffer.Append(logs.Record{Service: "api", Stream: logs.StreamOut, Text: "listening"})
	f.buffer.Append(logs.Record{Service: "api", Stream: logs.StreamErr, Text: "connection timeout"})
	f.buffer.Append(logs.Record{Service: "web", Stream: logs.StreamOut, Text: "started"})

	var lines []LogLine
	err := f.client().Call(context.Background(), Request{Op: OpLogSearch, Service: "api"}, &lines)
	if err != nil {
		t.Fatalf("log search: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0].Text != "listening" {
		t.Fatalf("lines are out of order: %#v", lines)
	}
	// A timestamp that arrives rounded is a timestamp that will be wrong in a
	// report later.
	if lines[0].At().IsZero() {
		t.Fatal("line has no time")
	}
}

func TestLogTimeSurvivesTheWire(t *testing.T) {
	f := newFixture(t)
	when := time.Date(2026, 8, 22, 15, 4, 5, 123_000_000, time.UTC)
	f.buffer.Append(logs.Record{Time: when, Service: "api", Text: "x"})

	var lines []LogLine
	err := f.client().Call(context.Background(), Request{Op: OpLogSearch}, &lines)
	if err != nil {
		t.Fatalf("log search: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	if !lines[0].At().Equal(when) {
		t.Fatalf("time changed on the wire: sent %s, got %s", when, lines[0].At().UTC())
	}
}

func TestFollowSendsBacklogThenLiveLines(t *testing.T) {
	f := newFixture(t)
	f.buffer.Append(logs.Record{Service: "api", Stream: logs.StreamOut, Text: "before"})

	ctx := t.Context()

	received := make(chan string, 8)
	go func() {
		_ = f.client().Follow(ctx, Request{Service: "api"}, func(l LogLine) error {
			received <- l.Text
			return nil
		})
	}()

	// The backlog comes first, so a follower sees the context of what it is
	// about to watch.
	select {
	case got := <-received:
		if got != "before" {
			t.Errorf("first line was %q, want the backlog line", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no backlog was sent")
	}

	deadline := time.After(10 * time.Second)
	for {
		f.buffer.Append(logs.Record{Service: "api", Stream: logs.StreamOut, Text: "after"})
		select {
		case got := <-received:
			if got == "after" {
				return
			}
		case <-deadline:
			t.Fatal("no live line arrived")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestFollowFiltersByService(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	received := make(chan string, 8)
	go func() {
		_ = f.client().Follow(ctx, Request{Service: "api"}, func(l LogLine) error {
			received <- l.Text
			return nil
		})
	}()

	deadline := time.After(10 * time.Second)
	for {
		f.buffer.Append(logs.Record{Service: "web", Text: "not this one"})
		f.buffer.Append(logs.Record{Service: "api", Text: "this one"})
		select {
		case got := <-received:
			if got != "this one" {
				t.Fatalf("follower received a line from another service: %q", got)
			}
			return
		case <-deadline:
			t.Fatal("nothing arrived")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestApplyReportsBrokenFilesAsPartial(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.dir, "services")
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err = os.WriteFile(filepath.Join(dir, "good.filo"),
		[]byte(`(service (tuple "name" "good") (tuple "command" "/bin/true"))`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	err = os.WriteFile(filepath.Join(dir, "bad.filo"),
		[]byte(`(service (tuple "name" "bad"))`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, err := f.client().Do(context.Background(), Request{Op: OpApply})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The readable file still applies and the broken one is reported: one typo
	// must not stop the machine from converging on everything else.
	if resp.Code != CodeFailed {
		t.Fatalf("code = %q, want a failure that still carries what was applied", resp.Code)
	}
	if !strings.Contains(resp.Message, "bad") {
		t.Fatalf("the broken file was not named: %q", resp.Message)
	}
	if !strings.Contains(resp.Body, "good") {
		t.Fatalf("the valid file was not applied: %q", resp.Body)
	}
}

func TestOversizedMessageIsRefused(t *testing.T) {
	f := newFixture(t)
	conn, err := net.Dial("unix", f.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// A client that never sends a newline must cost the daemon a bounded
	// amount of memory, not everything it can write.
	_, err = conn.Write([]byte(strings.Repeat("x", maxRequestBytes+1024)))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	err = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err != nil {
		t.Fatalf("deadline: %v", err)
	}
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	if n > 0 && !strings.Contains(string(buf[:n]), CodeInvalid) {
		t.Fatalf("daemon answered %q", string(buf[:n]))
	}
}

func TestSecondDaemonIsRefused(t *testing.T) {
	f := newFixture(t)
	// Starting a second hostd on a socket another one is serving must fail
	// loudly, not silently take the socket over and leave two supervisors
	// fighting for the same processes.
	_, err := ListenUnix(f.socket)
	if err == nil {
		t.Fatal("a second daemon was allowed to take over the socket")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "s")
	if len(path) > maxSocketPath {
		t.Skip("temporary directory makes the socket path too long")
	}
	// A daemon that was killed leaves the socket file behind. Refusing to
	// start because of it would turn a crash into an outage.
	err = os.WriteFile(path, nil, 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	listener, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("a stale socket file stopped the daemon: %v", err)
	}
	_ = listener.Close()
}

func TestSocketPathTooLongExplainsTheFix(t *testing.T) {
	long := "/" + strings.Repeat("a", maxSocketPath) + "/hostd.sock"
	_, err := ListenUnix(long)
	if err == nil {
		t.Fatal("an unbindable path was accepted")
	}
	// "invalid argument" is what the kernel says. It teaches nobody anything.
	if !strings.Contains(err.Error(), "HOSTD_ROOT") {
		t.Fatalf("the error does not say how to fix it: %v", err)
	}
}
