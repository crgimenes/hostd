package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/crgimenes/hostd/internal/logs"
	"github.com/crgimenes/hostd/internal/service"
	"github.com/crgimenes/hostd/internal/supervisor"
	"github.com/crgimenes/hostd/internal/version"
)

// Limits on what a client can cost the daemon.
const (
	// maxClients bounds concurrent connections. The local socket is not
	// exposed to the network yet, but a supervisor that can be exhausted by
	// opening sockets is a supervisor that can be taken down.
	maxClients = 64
	// requestTimeout bounds one operation, so no request can hold a
	// connection open forever.
	requestTimeout = 30 * time.Second
	// idleTimeout closes a connection that stops asking for anything.
	idleTimeout = 10 * time.Minute
	// followBuffer is how many records a follower may fall behind before it
	// starts losing lines instead of holding the writers.
	followBuffer = 256
)

// Supervisor is what the server needs from the supervisor. Declaring it here
// keeps the transport out of the supervision logic.
type Supervisor interface {
	Status() []supervisor.Status
	StatusOf(name string) (supervisor.Status, error)
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	Apply(declared []service.Service) []string
}

// Server answers operations over a listener.
type Server struct {
	sup      Supervisor
	log      *logs.Buffer
	services string

	sem chan struct{}
	wg  sync.WaitGroup
}

// NewServer builds a server. servicesDir is where an apply reads from.
func NewServer(sup Supervisor, buffer *logs.Buffer, servicesDir string) *Server {
	return &Server{
		sup:      sup,
		log:      buffer,
		services: servicesDir,
		sem:      make(chan struct{}, maxClients),
	}
}

// ListenUnix opens the local control socket.
//
// A stale socket file from a daemon that was killed would make hostd refuse to
// start, so it is removed first; the directory is private, which is what keeps
// the local socket from being an unauthenticated way in for every user on the
// machine.
func ListenUnix(path string) (net.Listener, error) {
	err := checkSocketPath(path)
	if err != nil {
		return nil, err
	}
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("another hostd is already listening on %s", path)
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	err = os.Chmod(path, 0o600)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// maxSocketPath is the shortest sun_path any target platform offers, minus the
// terminator. Linux allows 108 bytes and macOS 104; using the smaller number
// everywhere means a path that works on the developer's machine works on the
// host too.
const maxSocketPath = 103

// checkSocketPath rejects a path the kernel cannot bind. The kernel's own
// answer is "invalid argument", which tells nobody anything.
func checkSocketPath(path string) error {
	if len(path) <= maxSocketPath {
		return nil
	}
	return fmt.Errorf("socket path is %d bytes and the limit is %d: %s\nset %s to a shorter directory",
		len(path), maxSocketPath, path, "HOSTD_ROOT")
}

// Serve answers connections until the context is cancelled.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	defer s.wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case s.sem <- struct{}{}:
		default:
			// Refusing is better than queueing without a ceiling, and saying
			// so is better than dropping the connection silently.
			_ = WriteMessage(conn, Response{Code: CodeUnavailable, Message: "too many clients connected; try again"})
			_ = conn.Close()
			continue
		}
		s.wg.Go(func() {
			defer func() { <-s.sem }()
			defer func() { _ = conn.Close() }()
			s.handle(ctx, conn)
		})
	}
}

// handle answers requests on one connection until it goes away.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		err := conn.SetReadDeadline(time.Now().Add(idleTimeout))
		if err != nil {
			return
		}
		var req Request
		err = ReadMessage(ctx, reader, &req)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) && err.Error() != "EOF" {
				_ = WriteMessage(conn, Response{Code: CodeInvalid, Message: err.Error()})
			}
			return
		}
		err = conn.SetWriteDeadline(time.Now().Add(requestTimeout))
		if err != nil {
			return
		}
		// Follow owns the connection for as long as the client watches.
		if req.Op == OpLogFollow {
			s.follow(ctx, conn, req)
			return
		}
		opCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		resp := s.dispatch(opCtx, req)
		cancel()
		err = WriteMessage(conn, resp)
		if err != nil {
			return
		}
	}
}

// dispatch runs one operation.
func (s *Server) dispatch(ctx context.Context, req Request) Response {
	switch req.Op {
	case OpDescribe:
		return s.describe()
	case OpStatus, OpServiceList:
		return body(s.sup.Status())
	case OpServiceStart:
		return s.act(req.Name, s.sup.Start)
	case OpServiceStop:
		return s.act(req.Name, s.sup.Stop)
	case OpServiceRestrt:
		return s.act(req.Name, s.sup.Restart)
	case OpApply:
		return s.apply(ctx)
	case OpLogSearch:
		return body(toLines(s.log.Search(logs.Query{
			Service: req.Service,
			Stream:  req.Stream,
			Match:   req.Match,
			Limit:   req.Limit,
			Since:   req.Since,
		})))
	default:
		return Response{
			Code:    CodeUnknownOp,
			Message: fmt.Sprintf("this hostd does not implement %q; ask it what it supports with hostctl describe", req.Op),
		}
	}
}

// Description is what a client needs to know before it trusts an answer.
type Description struct {
	Version    string   `filo:"version"`
	Protocol   int      `filo:"protocol"`
	Schema     int      `filo:"schema"`
	Operations []string `filo:"operations"`
}

func (s *Server) describe() Response {
	return body(Description{
		Version:  version.Version,
		Protocol: version.Protocol,
		Schema:   version.Schema,
		Operations: []string{
			OpDescribe, OpStatus, OpServiceList,
			OpServiceStart, OpServiceStop, OpServiceRestrt,
			OpApply, OpLogSearch, OpLogFollow,
		},
	})
}

func (s *Server) act(name string, fn func(string) error) Response {
	if name == "" {
		return Response{Code: CodeInvalid, Message: "this operation needs a service name"}
	}
	err := fn(name)
	if err != nil {
		if _, ok := errors.AsType[supervisor.ErrUnknownService](err); ok {
			return Response{Code: CodeNotFound, Message: err.Error()}
		}
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	return body(s.statusList(name))
}

func (s *Server) statusList(name string) []supervisor.Status {
	st, err := s.sup.StatusOf(name)
	if err != nil {
		return nil
	}
	return []supervisor.Status{st}
}

// apply re-reads the services directory and converges on it.
func (s *Server) apply(ctx context.Context) Response {
	declared, err := service.LoadDir(ctx, s.services)
	if err != nil {
		// The valid files still load, so the answer says what was applied and
		// what was refused instead of failing the whole operation.
		changes := s.sup.Apply(declared)
		return Response{
			Code:    CodeFailed,
			Message: err.Error(),
			Body:    mustMarshal(changes),
		}
	}
	return body(s.sup.Apply(declared))
}

// follow streams records to a client until it goes away.
func (s *Server) follow(ctx context.Context, conn net.Conn, req Request) {
	query := logs.Query{Service: req.Service, Stream: req.Stream, Match: req.Match, Since: req.Since, Limit: req.Limit}
	stream, stop := s.log.Watch(followBuffer)
	defer stop()

	// Everything already recorded goes first, so a follower sees the context
	// of what it is about to watch.
	backlog := s.log.Search(query)
	var last uint64
	for _, r := range backlog {
		err := s.send(conn, r)
		if err != nil {
			return
		}
		last = r.Seq
	}
	query.Limit = 0
	for {
		select {
		case <-ctx.Done():
			return
		case r, ok := <-stream:
			if !ok {
				return
			}
			// The backlog and the live stream overlap by however many records
			// arrived between the two, so anything already sent is skipped.
			if r.Seq <= last {
				continue
			}
			last = r.Seq
			if !query.Matches(r) {
				continue
			}
			err := s.send(conn, r)
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) send(conn net.Conn, r logs.Record) error {
	err := conn.SetWriteDeadline(time.Now().Add(requestTimeout))
	if err != nil {
		return err
	}
	return WriteMessage(conn, toLine(r))
}
