package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/metrics"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
	"github.com/crgimenes/hostd/version"
)

// Ceilings on what a client can cost the daemon: one that can be exhausted by
// opening sockets is one that can be taken down.
const (
	maxClients     = 64
	requestTimeout = 30 * time.Second
	idleTimeout    = 10 * time.Minute
	// How far a follower may fall behind before it loses lines instead of
	// holding up the writers.
	followBuffer = 256
)

// Declared here, so the transport stays out of the supervision logic.
type Supervisor interface {
	Status() []supervisor.Status
	StatusOf(name string) (supervisor.Status, error)
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	Plan(declared []service.Service) []supervisor.Change
	Apply(declared []service.Service) []supervisor.Change
}

type Store interface {
	Generation() uint64
	Check(expected uint64) error
	Record(e state.Entry) uint64
	Recent(limit int) []state.Entry
}

type Server struct {
	sup      Supervisor
	store    Store
	log      *logs.Store
	metrics  *metrics.Store
	services string

	sem chan struct{}
	wg  sync.WaitGroup
}

// servicesDir is where an apply reads from.
func NewServer(sup Supervisor, store Store, logStore *logs.Store, metricStore *metrics.Store, servicesDir string) *Server {
	return &Server{
		sup:      sup,
		store:    store,
		log:      logStore,
		metrics:  metricStore,
		services: servicesDir,
		sem:      make(chan struct{}, maxClients),
	}
}

// A stale socket file from a killed daemon would stop hostd from starting, so
// it is removed first. The private directory is what keeps the local socket
// from being an unauthenticated way in for every user on the machine.
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

// The shortest sun_path any target offers (macOS 104, Linux 108), minus the
// terminator: a path that binds on the developer's machine binds on the host.
const maxSocketPath = 103

// The kernel's own answer is "invalid argument", which teaches nobody anything.
func checkSocketPath(path string) error {
	if len(path) <= maxSocketPath {
		return nil
	}
	return fmt.Errorf("socket path is %d bytes and the limit is %d: %s\nset %s to a shorter directory",
		len(path), maxSocketPath, path, "HOSTD_ROOT")
}

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
			// Refusing beats queueing without a ceiling, and saying so beats
			// dropping the connection silently.
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
			if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, io.EOF) {
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

// Reads answer straight away; mutations pass the generation check first and
// are audited whether carried out or refused.
func (s *Server) dispatch(ctx context.Context, req Request) Response {
	switch req.Op {
	case OpDescribe:
		return s.stamp(s.describe())
	case OpStatus, OpServiceList:
		return s.stamp(body(s.sup.Status()))
	case OpPlan:
		return s.stamp(s.planOnly(ctx))
	case OpAudit:
		return s.stamp(body(s.store.Recent(req.Limit)))
	case OpLogSearch:
		return s.stamp(s.searchLogs(req))
	case OpMetrics:
		return s.stamp(s.readMetrics(req))
	case OpServiceStart:
		return s.mutate(req, req.Name, s.sup.Start)
	case OpServiceStop:
		return s.mutate(req, req.Name, s.sup.Stop)
	case OpServiceRestrt:
		return s.mutate(req, req.Name, s.sup.Restart)
	case OpApply:
		return s.apply(ctx, req)
	default:
		return Response{
			Code:    CodeUnknownOp,
			Message: fmt.Sprintf("this hostd does not implement %q; ask it what it supports with hostctl describe", req.Op),
		}
	}
}

// "No lines matched" and "I could not look" are different answers.
func (s *Server) searchLogs(req Request) Response {
	records, err := s.log.Search(logs.Query{
		Service: req.Service,
		Stream:  req.Stream,
		Kind:    req.Kind,
		Match:   req.Match,
		Limit:   req.Limit,
		Since:   req.Since,
	})
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	return body(toLines(records))
}

// A value older than this is not what the machine is doing now; presenting it
// as current is how somebody acts on a host that stopped answering.
const latestWindow = time.Minute

// No window asked for means "what is this machine doing", which is the newest
// value of every series rather than an empty answer.
func (s *Server) readMetrics(req Request) Response {
	if req.FromMS <= 0 {
		series, err := s.metrics.Latest(latestWindow)
		if err != nil {
			return Response{Code: CodeFailed, Message: err.Error()}
		}
		return body(series)
	}
	query := metrics.Query{
		Scope:  req.Scope,
		Name:   req.Service,
		Metric: req.Metric,
		From:   time.UnixMilli(int64(req.FromMS)),
		StepMS: int64(req.StepMS),
		Limit:  req.Limit,
	}
	if req.ToMS > 0 {
		query.To = time.UnixMilli(int64(req.ToMS))
	}
	series, err := s.metrics.Query(query)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	return body(series)
}

// So a caller always knows what to claim next.
func (s *Server) stamp(resp Response) Response {
	if resp.Generation == 0 {
		resp.Generation = s.store.Generation()
	}
	return resp
}

// Generation check, the work, then the audit entry.
func (s *Server) mutate(req Request, name string, fn func(string) error) Response {
	if name == "" {
		return s.stamp(Response{Code: CodeInvalid, Message: "this operation needs a service name"})
	}
	entry := state.Entry{Operation: req.Op, Target: name, OnBehalfOf: req.OnBehalfOf}

	err := s.store.Check(req.ExpectGeneration)
	if err != nil {
		return s.refuse(entry, CodeConflict, err)
	}
	err = fn(name)
	if err != nil {
		_, unknown := errors.AsType[supervisor.ErrUnknownService](err)
		if unknown {
			return s.refuse(entry, CodeNotFound, err)
		}
		entry.Result = state.ResultFailed
		entry.Detail = err.Error()
		generation := s.store.Record(entry)
		return Response{Code: CodeFailed, Message: err.Error(), Generation: generation}
	}
	entry.Result = state.ResultOK
	generation := s.store.Record(entry)
	resp := body(s.statusList(name))
	resp.Generation = generation
	return resp
}

// Audits an operation that was not carried out.
func (s *Server) refuse(entry state.Entry, code string, cause error) Response {
	entry.Result = state.ResultRefused
	entry.Detail = cause.Error()
	generation := s.store.Record(entry)
	return Response{Code: code, Message: cause.Error(), Generation: generation}
}

// What a client needs before it trusts an answer from an unknown release.
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
			OpPlan, OpApply, OpAudit, OpLogSearch, OpLogFollow, OpMetrics,
		},
	})
}

func (s *Server) statusList(name string) []supervisor.Status {
	st, err := s.sup.StatusOf(name)
	if err != nil {
		return nil
	}
	return []supervisor.Status{st}
}

// dry-run is a property of the design, not a convenience of the command line:
// one plan serves a person, an automation and an agent.
func (s *Server) planOnly(ctx context.Context) Response {
	declared, err := service.LoadDir(ctx, s.services)
	changes := s.sup.Plan(declared)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error(), Body: mustMarshal(changes)}
	}
	return body(changes)
}

func (s *Server) apply(ctx context.Context, req Request) Response {
	entry := state.Entry{Operation: OpApply, Target: s.services, OnBehalfOf: req.OnBehalfOf}

	err := s.store.Check(req.ExpectGeneration)
	if err != nil {
		return s.refuse(entry, CodeConflict, err)
	}

	declared, loadErr := service.LoadDir(ctx, s.services)
	changes := s.sup.Plan(declared)

	// Never inferred from an ordinary apply: an accidentally deleted file
	// must not quietly take production down.
	if supervisor.HasDestructive(changes) && !req.AllowDestructive {
		gone := supervisor.Destructive(changes)
		names := make([]string, 0, len(gone))
		for _, c := range gone {
			names = append(names, c.Service)
		}
		cause := fmt.Errorf(
			"this would stop %s, which no file declares any more; review it with hostctl plan, then run hostctl apply -allow-destructive to go ahead",
			strings.Join(names, ", "))
		resp := s.refuse(entry, CodeDestructive, cause)
		resp.Body = mustMarshal(changes)
		return resp
	}

	applied := s.sup.Apply(declared)
	if loadErr != nil {
		// The readable files still applied: one typo must not stop the
		// machine converging on everything else.
		entry.Result = state.ResultOK
		entry.Detail = loadErr.Error()
		generation := s.store.Record(entry)
		return Response{
			Code:       CodeFailed,
			Message:    loadErr.Error(),
			Generation: generation,
			Body:       mustMarshal(applied),
		}
	}
	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("%d change(s)", len(applied))
	generation := s.store.Record(entry)
	resp := body(applied)
	resp.Generation = generation
	return resp
}

func (s *Server) follow(ctx context.Context, conn net.Conn, req Request) {
	query := logs.Query{Service: req.Service, Stream: req.Stream, Match: req.Match, Since: req.Since, Limit: req.Limit}
	stream, stop := s.log.Watch(followBuffer)
	defer stop()

	// The backlog first, so a follower sees the context of what it watches.
	backlog, err := s.log.Search(query)
	if err != nil {
		_ = WriteMessage(conn, Response{Code: CodeFailed, Message: err.Error()})
		return
	}
	var last uint64
	for _, r := range backlog {
		err = s.send(conn, r)
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
			// The backlog and the live stream overlap by whatever arrived
			// between them.
			if r.Seq <= last {
				continue
			}
			last = r.Seq
			if !query.Matches(r) {
				continue
			}
			err = s.send(conn, r)
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
