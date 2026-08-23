package api

import (
	"bytes"
	"io"
	"os/user"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// This is how a request arrives from another machine: ssh runs the daemon's
// stdio mode there, and the pipes carry the same protocol the socket does.
func TestStdioCarriesTheProtocol(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api", State: supervisor.StateRunning}}

	request, toStdio := io.Pipe()
	fromStdio := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Stdio(f.socket, request, fromStdio) }()

	err := WriteMessage(toStdio, Request{Op: OpStatus})
	if err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = toStdio.Close()

	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("Stdio: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stdio proxy did not finish")
	}
	answer := fromStdio.String()
	if !strings.Contains(answer, `"code" "ok"`) || !strings.Contains(answer, "api") {
		t.Fatalf("the daemon's answer did not come back through the pipes: %q", answer)
	}
}

// A machine that is not running the daemon has to say so where the operator
// can act on it, not fail with a bare errno through an ssh pipe.
func TestStdioSaysWhenTheDaemonIsNotThere(t *testing.T) {
	err := Stdio(t.TempDir()+"/absent.sock", strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("talking to a daemon that is not there was reported as success")
	}
	if !strings.Contains(err.Error(), "systemctl status hostd") {
		t.Fatalf("the error does not say where to look: %v", err)
	}
}

// The audit answers "who", and the answer comes from the kernel rather than
// from anything the caller said about itself.
func TestTheActorIsTheAccountOnTheOtherEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("peer credentials on a unix socket are read on linux")
	}
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "api", State: supervisor.StateRunning}}

	err := call(f.client(), Request{Op: OpServiceStop, Name: "api"}, nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	recent := f.store.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("the operation was not audited: %#v", recent)
	}
	// Root through sudo carries the operator's own name alongside it.
	if recent[0].Actor != me.Username && !strings.HasPrefix(recent[0].Actor, me.Username+" ") {
		t.Fatalf("the audit names %q, expected the account that connected (%s)", recent[0].Actor, me.Username)
	}
	if recent[0].Actor == state.ActorLocal && me.Username != state.ActorLocal {
		t.Fatal("the identity fell back to the machine when the kernel could have named the account")
	}
}

// A pipe has no deadline of its own, so a client over ssh must still be able
// to ask and be answered.
type syncBuffer struct {
	mu  chan struct{}
	buf bytes.Buffer
}

func (s *syncBuffer) Write(b []byte) (int, error) {
	if s.mu == nil {
		s.mu = make(chan struct{}, 1)
	}
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	return s.buf.Write(b)
}

func (s *syncBuffer) String() string { return s.buf.String() }
