package api

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

func call(client *Client, req Request, out any) error {
	resp, err := client.Do(context.Background(), req)
	if err != nil {
		return err
	}
	if resp.Failed() {
		return resp.Err()
	}
	if out == nil || resp.Body == "" {
		return nil
	}
	return filoconf.Decode(context.Background(), "response", resp.Body, out)
}

// fakeSupervisor answers like the real one, including failing on demand. A
// fake that always succeeds makes the error path impossible to exercise, and
// the error path is the one that matters when something is wrong at 3am.
type fakeSupervisor struct {
	mu          sync.Mutex
	statuses    []supervisor.Status
	calls       []string
	failWith    error
	unchanged   bool
	destructive bool
	applied     bool
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

// changed mirrors the real supervisor: an accepted command that moved nothing
// answers false, and the generation must not move for it.
func (f *fakeSupervisor) Start(name string) (bool, error) {
	err := f.act("start", name)
	return err == nil && !f.unchanged, err
}

func (f *fakeSupervisor) Stop(name string) (bool, error) {
	err := f.act("stop", name)
	return err == nil && !f.unchanged, err
}

func (f *fakeSupervisor) Restart(name string) (bool, error) {
	err := f.act("restart", name)
	return err == nil && !f.unchanged, err
}

func (f *fakeSupervisor) Plan(declared []service.Service) []supervisor.Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]supervisor.Change, 0, len(declared))
	for _, s := range declared {
		out = append(out, supervisor.Change{
			Service:     s.Name,
			Action:      supervisor.ActionAdd,
			Destructive: f.destructive,
		})
	}
	return out
}

func (f *fakeSupervisor) Apply(declared []service.Service) []supervisor.Change {
	f.mu.Lock()
	f.applied = true
	f.mu.Unlock()
	return f.Plan(declared)
}

func (f *fakeSupervisor) didApply() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied
}

type fixture struct {
	t       *testing.T
	sup     *fakeSupervisor
	store   *state.Store
	buffer  *logs.Store
	metrics *metrics.Store
	server  *Server
	socket  string
	dir     string
	cancel  context.CancelFunc
	served  chan error
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

	store, err := state.Open(context.Background(), filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	logStore, err := logs.Open(context.Background(), filepath.Join(dir, "logs.db"), logs.Options{})
	if err != nil {
		t.Fatalf("logs.Open: %v", err)
	}
	t.Cleanup(func() { _ = logStore.Close() })
	metricStore, err := metrics.Open(context.Background(), filepath.Join(dir, "metrics.db"), metrics.Options{})
	if err != nil {
		t.Fatalf("metrics.Open: %v", err)
	}
	t.Cleanup(func() { _ = metricStore.Close() })
	f := &fixture{
		t:      t,
		sup:    &fakeSupervisor{},
		store:  store,
		buffer: logStore,
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
	f.metrics = metricStore
	server := NewServer(f.sup, f.store, f.buffer, metricStore, filepath.Join(dir, "services"))
	f.server = server
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
	err := call(f.client(), Request{Op: OpDescribe}, &d)
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
	err := call(f.client(), Request{Op: OpStatus}, &got)
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
	err := call(f.client(), Request{Op: OpStatus}, &got)
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
		err := call(client, Request{Op: op, Name: "api"}, nil)
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
	err := call(f.client(), Request{Op: OpLogSearch, Service: "api"}, &lines)
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
	err := call(f.client(), Request{Op: OpLogSearch}, &lines)
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

// Every answer carries the generation, so a caller always knows what to claim
// on its next mutation without asking again.
func TestEveryAnswerCarriesTheGeneration(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api"}}
	client := f.client()

	resp, err := client.Do(context.Background(), Request{Op: OpStatus})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	before := resp.Generation

	resp, err = client.Do(context.Background(), Request{Op: OpServiceStart, Name: "api"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if resp.Generation != before+1 {
		t.Fatalf("a mutation moved the generation from %d to %d, want %d", before, resp.Generation, before+1)
	}

	// A read does not move it: only changes count.
	resp, err = client.Do(context.Background(), Request{Op: OpStatus})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if resp.Generation != before+1 {
		t.Fatalf("a read moved the generation to %d", resp.Generation)
	}
}

// Two operators, or an operator and an agent, must not overwrite each other.
func TestStaleGenerationIsRefused(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api"}}
	client := f.client()

	first, err := client.Do(context.Background(), Request{Op: OpServiceStart, Name: "api"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	stale := first.Generation - 1

	resp, err := client.Do(context.Background(), Request{
		Op: OpServiceStop, Name: "api", ExpectGeneration: stale,
	})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if resp.Code != CodeConflict {
		t.Fatalf("code = %q, want %q", resp.Code, CodeConflict)
	}
	// The refusal carries the current generation, so the caller can re-read
	// and retry without a second round trip to find out where it is.
	if resp.Generation != first.Generation {
		t.Fatalf("the refusal reports generation %d, want %d", resp.Generation, first.Generation)
	}
	if !strings.Contains(resp.Message, "hostctl status") {
		t.Fatalf("the message does not say what to do: %q", resp.Message)
	}
}

func TestMatchingGenerationIsAccepted(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api"}}
	client := f.client()

	current, err := client.Do(context.Background(), Request{Op: OpStatus})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	resp, err := client.Do(context.Background(), Request{
		Op: OpServiceStop, Name: "api", ExpectGeneration: current.Generation,
	})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("a matching generation was refused: %+v", resp)
	}
}

// Claiming no generation is allowed: optimistic control is a tool, not a toll.
func TestNoClaimIsAllowed(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api"}}
	resp, err := f.client().Do(context.Background(), Request{Op: OpServiceStop, Name: "api"})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("a call with no claim was refused: %+v", resp)
	}
}

// An accidentally deleted service file must not quietly take production down.
func TestDestructiveApplyIsRefusedWithoutAuthorisation(t *testing.T) {
	f := newFixture(t)
	f.sup.destructive = true
	err := os.MkdirAll(filepath.Join(f.dir, "services"), 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err = os.WriteFile(filepath.Join(f.dir, "services", "api.filo"),
		[]byte(`(service (tuple "name" "api") (tuple "command" "/bin/true"))`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, err := f.client().Do(context.Background(), Request{Op: OpApply})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if resp.Code != CodeDestructive {
		t.Fatalf("code = %q, want %q", resp.Code, CodeDestructive)
	}
	if f.sup.didApply() {
		t.Fatal("a refused apply changed the host anyway")
	}
	// The refusal has to name what would go and say how to go ahead.
	if !strings.Contains(resp.Message, "api") || !strings.Contains(resp.Message, "-allow-destructive") {
		t.Fatalf("the refusal does not say what or how: %q", resp.Message)
	}
	// It still carries the plan, so the operator can review before deciding.
	if !strings.Contains(resp.Body, "api") {
		t.Fatalf("the refusal does not carry the plan: %q", resp.Body)
	}

	resp, err = f.client().Do(context.Background(), Request{Op: OpApply, AllowDestructive: true})
	if err != nil {
		t.Fatalf("authorised apply: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("an authorised destructive apply was refused: %+v", resp)
	}
	if !f.sup.didApply() {
		t.Fatal("the authorised apply did not run")
	}
}

// A plan changes nothing. dry-run is a property of the design, not a courtesy
// of the command line.
func TestPlanChangesNothing(t *testing.T) {
	f := newFixture(t)
	err := os.MkdirAll(filepath.Join(f.dir, "services"), 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err = os.WriteFile(filepath.Join(f.dir, "services", "api.filo"),
		[]byte(`(service (tuple "name" "api") (tuple "command" "/bin/true"))`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	before, err := f.client().Do(context.Background(), Request{Op: OpStatus})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var changes []supervisor.Change
	err = call(f.client(), Request{Op: OpPlan}, &changes)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(changes) != 1 || changes[0].Service != "api" {
		t.Fatalf("plan = %#v", changes)
	}
	if f.sup.didApply() {
		t.Fatal("a plan applied something")
	}
	after, err := f.client().Do(context.Background(), Request{Op: OpStatus})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if after.Generation != before.Generation {
		t.Fatalf("a plan moved the generation from %d to %d", before.Generation, after.Generation)
	}
}

// "Who stopped the service at three in the morning" has to have an answer,
// including when the answer is that it was refused.
func TestAuditRecordsWhatHappenedAndWhatWasRefused(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api"}}
	client := f.client()

	done, err := client.Do(context.Background(), Request{
		Op: OpServiceStop, Name: "api", OnBehalfOf: "crg",
	})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	_, err = client.Do(context.Background(), Request{
		Op: OpServiceStart, Name: "api", ExpectGeneration: done.Generation + 99,
	})
	if err != nil {
		t.Fatalf("stale start: %v", err)
	}

	var entries []state.Entry
	err = call(client, Request{Op: OpAudit}, &entries)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit has %d entries, want 2", len(entries))
	}
	ok, refused := entries[0], entries[1]
	if ok.Operation != OpServiceStop || ok.Result != state.ResultOK || ok.Target != "api" {
		t.Fatalf("the accepted operation was not audited properly: %+v", ok)
	}
	// Delegation is recorded rather than flattened: an agent acting for a
	// person is not the same as the person acting.
	if ok.OnBehalfOf != "crg" {
		t.Fatalf("delegation was not recorded: %+v", ok)
	}
	if ok.After != ok.Before+1 {
		t.Fatalf("the accepted operation did not move the generation: %+v", ok)
	}
	if refused.Result != state.ResultRefused {
		t.Fatalf("the refused operation was not audited as refused: %+v", refused)
	}
	if refused.After != refused.Before {
		t.Fatalf("a refused operation moved the generation: %+v", refused)
	}
}

// Events carry a stable code, so a program can watch for a service that keeps
// dying without matching on a sentence that may be rewritten.
func TestLogFiltersByEventKind(t *testing.T) {
	f := newFixture(t)
	f.buffer.Append(logs.Record{Service: "api", Stream: logs.StreamEvent, Kind: logs.EventStarted, Text: "started process 1"})
	f.buffer.Append(logs.Record{Service: "api", Stream: logs.StreamEvent, Kind: logs.EventExited, Text: "exited with code 3"})
	f.buffer.Append(logs.Record{Service: "api", Stream: logs.StreamOut, Text: "hello"})

	var lines []LogLine
	err := call(f.client(), Request{Op: OpLogSearch, Kind: logs.EventExited}, &lines)
	if err != nil {
		t.Fatalf("log search: %v", err)
	}
	if len(lines) != 1 || lines[0].Kind != logs.EventExited {
		t.Fatalf("kind filter returned %#v", lines)
	}
}

// Without a window the question is "what is this machine doing now", and the
// answer is the newest value of every series; with one, the series themselves.
func TestMetricsAnswerTheLatestAndTheWindow(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	err := f.metrics.Append([]metrics.Sample{
		{Time: now.Add(-time.Minute), Scope: metrics.ScopeHost, Metric: metrics.MetricLoad1, Value: 0.25},
		{Time: now, Scope: metrics.ScopeHost, Metric: metrics.MetricLoad1, Value: 0.5},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var latest []metrics.Series
	err = call(f.client(), Request{Op: OpMetrics}, &latest)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if len(latest) != 1 || len(latest[0].Points) != 1 || latest[0].Points[0].Value != 0.5 {
		t.Fatalf("the latest answer is not the newest value: %#v", latest)
	}

	var window []metrics.Series
	err = call(f.client(), Request{
		Op:     OpMetrics,
		FromMS: float64(now.Add(-time.Hour).UnixMilli()),
		ToMS:   float64(now.UnixMilli()),
	}, &window)
	if err != nil {
		t.Fatalf("metrics window: %v", err)
	}
	if len(window) != 1 || len(window[0].Points) != 2 {
		t.Fatalf("the window answer lost points: %#v", window)
	}
	if window[0].Points[0].Value != 0.25 {
		t.Fatalf("the window is not in time order: %#v", window[0].Points)
	}
}

// A daemon of an unknown release is asked what it can do, so a new operation
// has to appear there before a client may use it.
func TestDescribeListsMetrics(t *testing.T) {
	f := newFixture(t)
	var d Description
	err := call(f.client(), Request{Op: OpDescribe}, &d)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !slices.Contains(d.Operations, OpMetrics) {
		t.Fatalf("describe does not list %q: %v", OpMetrics, d.Operations)
	}
}

// An apply with nothing to do is accepted and moves nothing: a caller holding
// the previous generation must not be refused for a change nobody made.
func TestAnApplyThatChangesNothingHoldsTheGeneration(t *testing.T) {
	f := newFixture(t)
	before := f.store.Generation()

	resp, err := f.client().Do(context.Background(), Request{Op: OpApply})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("apply failed: %v", resp.Err())
	}
	if resp.Generation != before {
		t.Fatalf("an empty apply moved the generation from %d to %d", before, resp.Generation)
	}
}

// Asking a running service to start is accepted, changes nothing, and says so
// in the audit rather than in an error.
func TestACommandThatChangesNothingHoldsTheGeneration(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api", State: supervisor.StateRunning}}
	f.sup.unchanged = true
	before := f.store.Generation()

	resp, err := f.client().Do(context.Background(), Request{Op: OpServiceStart, Name: "api"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("starting a running service failed: %v", resp.Err())
	}
	if resp.Generation != before {
		t.Fatalf("a command that changed nothing moved the generation from %d to %d", before, resp.Generation)
	}
	recent := f.store.Recent(1)
	if len(recent) != 1 || recent[0].Detail != "nothing to change" {
		t.Fatalf("the audit does not say the command changed nothing: %#v", recent)
	}
}

// A socket that opens and closes without answering is what a forwarded socket
// the far user cannot read looks like. "EOF" names the mechanism and teaches
// nothing; the message has to name what to check.
func TestAConnectionClosedWithoutAnAnswerSaysWhatToCheck(t *testing.T) {
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	if len(socket) > maxSocketPath {
		t.Skipf("temporary directory makes a socket path of %d bytes", len(socket))
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		// Reads the request and answers nothing, which is what a forwarded
		// socket does when the far side cannot open it.
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
	}()

	client, err := DialUnix(socket)
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.Do(context.Background(), Request{Op: OpStatus})
	if err == nil {
		t.Fatal("a connection closed without an answer was reported as success")
	}
	if !strings.Contains(err.Error(), "closed the connection without answering") {
		t.Fatalf("the message does not say what happened: %v", err)
	}
}

func (f *fixture) search(q logs.Query) []logs.Record {
	f.t.Helper()
	records, err := f.buffer.Search(q)
	if err != nil {
		f.t.Fatalf("Search: %v", err)
	}
	return records
}

func decodeBody(t *testing.T, body string, out any) error {
	t.Helper()
	return filoconf.Decode(context.Background(), "response", body, out)
}
