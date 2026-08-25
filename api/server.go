package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/hostd/docker"
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
	Start(name string) (bool, error)
	Stop(name string) (bool, error)
	Restart(name string) (bool, error)
	Plan(declared []service.Service) []supervisor.Change
	Apply(declared []service.Service) []supervisor.Change
}

type Store interface {
	Generation() uint64
	Check(expected uint64) error
	Record(e state.Entry, changed bool) uint64
	Recent(limit int) []state.Entry
}

type Server struct {
	sup      Supervisor
	store    Store
	log      *logs.Store
	metrics  *metrics.Store
	runtime  *docker.Client
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

// Runtime hands the server the container daemon images are pushed into.
// Without it a push fails saying this machine runs no containers, which is the
// honest answer rather than a timeout.
func (s *Server) Runtime(client *docker.Client) { s.runtime = client }

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
	err = grantGroup(path, filepath.Dir(path))
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// Membership of the hostd group is the permission to operate this machine.
// Without the group the socket stays root's alone, which is what it was before
// anybody created one.
func grantGroup(socket, dir string) error {
	found, err := user.LookupGroup(Group)
	if err != nil {
		return os.Chmod(socket, 0o600)
	}
	gid, err := strconv.Atoi(found.Gid)
	if err != nil {
		return os.Chmod(socket, 0o600)
	}
	for _, path := range []string{dir, socket} {
		err = os.Chown(path, os.Getuid(), gid)
		if err != nil {
			return err
		}
	}
	// #nosec G302 -- audited: the group is the permission to operate this
	// machine, so the socket and its directory are reachable by it and by
	// nobody else
	err = os.Chmod(dir, 0o750)
	if err != nil {
		return err
	}
	// #nosec G302 -- audited: same group, same reason
	return os.Chmod(socket, 0o660)
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

// loadImage streams what the client is sending straight into the runtime. The
// bytes are never held whole: an image is hundreds of megabytes, and a daemon
// that buffered one would be a daemon a client could exhaust.
func (s *Server) loadImage(ctx context.Context, conn net.Conn, reader *bufio.Reader, req Request) Response {
	if s.runtime == nil {
		// The bytes are already on their way, so they are read and dropped:
		// leaving them in the pipe would desynchronise the connection.
		_, _ = ReadChunks(reader, io.Discard, nil)
		return Response{Code: CodeFailed, Message: "this machine has no container runtime to load an image into"}
	}
	// An image for another architecture loads perfectly well and then fails to
	// start with "exec format error". Refusing it here costs the transfer and
	// says the real reason.
	here, err := s.runtime.Arch(ctx)
	if err == nil && req.Arch != "" && req.Arch != here {
		_, _ = ReadChunks(reader, io.Discard, nil)
		return Response{
			Code: CodeInvalid,
			Message: fmt.Sprintf("%s was built for %s and this machine is %s; build it for %s (docker build --platform linux/%s)",
				req.Name, req.Arch, here, here, here),
		}
	}
	pipeReader, pipeWriter := io.Pipe()
	loaded := make(chan error, 1)
	go func() { loaded <- s.runtime.Load(ctx, pipeReader) }()

	// The bytes are hashed as they pass. An image id is not this: two daemons
	// reading the same archive compute different ids, because the id is of the
	// config each one writes. What travelled is what can be compared.
	content := sha256.New()
	total, err := ReadChunks(reader, io.MultiWriter(pipeWriter, content), func() {
		_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	})
	_ = pipeWriter.CloseWithError(err)
	loadErr := <-loaded
	if err != nil {
		return Response{Code: CodeFailed, Message: fmt.Sprintf("the image did not arrive whole: %v", err)}
	}
	if loadErr != nil {
		return Response{Code: CodeFailed, Message: loadErr.Error()}
	}

	// What the machine now has, by digest: a tag is a name that can be made to
	// mean something else tomorrow.
	digest, err := s.runtime.ImageDigest(ctx, req.Name)
	if err != nil {
		return Response{Code: CodeFailed, Message: fmt.Sprintf("%s was loaded but cannot be found: %v", req.Name, err)}
	}
	sum := hex.EncodeToString(content.Sum(nil))
	s.log.Append(logs.Record{
		Service: "hostd",
		Stream:  logs.StreamEvent,
		Kind:    logs.EventImage,
		Text:    fmt.Sprintf("received image %s as %s, %d bytes, content sha256:%s", req.Name, digest, total, sum),
	})
	return body(Image{Name: req.Name, Digest: digest, Bytes: float64(total), Content: sum})
}

// What a push answers with, so the declaration can be pinned to what really
// arrived rather than to the tag it was called by.
type Image struct {
	Name string `filo:"name"`
	// What this machine now calls it. Two machines loading the same archive
	// arrive at different ids, so this is the one to declare here and nowhere
	// else.
	Digest string  `filo:"digest"`
	Bytes  float64 `filo:"bytes"`
	// The hash of what crossed the wire, which is the same on both sides and
	// is what proves the transfer.
	Content string `filo:"content-sha256"`
}

// One image this machine holds. The digest is the id this runtime gave it and
// means nothing on another machine, which is why it is the thing to declare
// here and the thing a rollback checks for here.
type ImageEntry struct {
	Digest string `filo:"digest" json:"digest"`
	// Empty for an image no tag names any more: a version displaced by a later
	// push, still on the disk and still startable by digest.
	Tags  []string `filo:"tags" json:"tags"`
	Bytes float64  `filo:"bytes" json:"bytes"`
	// Milliseconds, like every other time on the wire.
	Created float64 `filo:"created-ms" json:"created-ms"`
}

// listImages is how an operator sees what a machine's disk went to, and what a
// rollback would still find there. Nothing removes images today, so this only
// grows — reporting it is the first half of being able to prune it.
func (s *Server) listImages(ctx context.Context) Response {
	if s.runtime == nil {
		return Response{Code: CodeUnavailable, Message: "this machine has no container runtime to list images from"}
	}
	held, err := s.runtime.Images(ctx)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	out := make([]ImageEntry, 0, len(held))
	for _, image := range held {
		out = append(out, ImageEntry{
			Digest:  image.Digest,
			Tags:    image.Tags,
			Bytes:   float64(image.Bytes),
			Created: float64(image.Created.UnixMilli()),
		})
	}
	return body(out)
}

// Who is on the other end, from the kernel rather than from anything the
// caller said. Over ssh that is the unix account the operator logged in as,
// which is what "who stopped the service at three in the morning" needs.
func (s *Server) actor(conn net.Conn) string {
	peer := Peer(conn)
	if peer == "" {
		return state.ActorLocal
	}
	return peer
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	actor := s.actor(conn)
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
		// An image arrives as bytes after the request line, so this one reads
		// from the connection instead of only writing to it.
		if req.Op == OpImagePush {
			resp := s.loadImage(ctx, conn, reader, req)
			err = WriteMessage(conn, s.stamp(resp))
			if err != nil {
				return
			}
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		resp := s.dispatch(opCtx, req, actor)
		cancel()
		err = WriteMessage(conn, resp)
		if err != nil {
			return
		}
	}
}

// Reads answer straight away; mutations pass the generation check first and
// are audited whether carried out or refused.
func (s *Server) dispatch(ctx context.Context, req Request, actor string) Response {
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
	case OpImageList:
		return s.stamp(s.listImages(ctx))
	case OpServiceStart:
		return s.mutate(req, actor, req.Name, s.sup.Start)
	case OpServiceStop:
		return s.mutate(req, actor, req.Name, s.sup.Stop)
	case OpServiceRestrt:
		return s.mutate(req, actor, req.Name, s.sup.Restart)
	case OpApply:
		return s.apply(ctx, req, actor)
	case OpServicePut:
		return s.stamp(s.putService(ctx, req, actor))
	case OpServicePrune:
		return s.stamp(s.pruneServices(ctx, req, actor))
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
		Run:     req.Run,
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
	query := metrics.Query{
		Scope:  req.Scope,
		Name:   req.Service,
		Metric: req.Metric,
		StepMS: int64(req.StepMS),
		Limit:  req.Limit,
	}
	if req.FromMS <= 0 {
		series, err := s.metrics.Latest(query, latestWindow)
		if err != nil {
			return Response{Code: CodeFailed, Message: err.Error()}
		}
		return body(series)
	}
	query.From = time.UnixMilli(int64(req.FromMS))
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
func (s *Server) mutate(req Request, actor, name string, fn func(string) (bool, error)) Response {
	if name == "" {
		return s.stamp(Response{Code: CodeInvalid, Message: "this operation needs a service name"})
	}
	entry := state.Entry{Operation: req.Op, Target: name, Actor: actor, OnBehalfOf: req.OnBehalfOf}

	err := s.store.Check(req.ExpectGeneration)
	if err != nil {
		return s.refuse(entry, CodeConflict, err)
	}
	changed, err := fn(name)
	if err != nil {
		_, unknown := errors.AsType[supervisor.ErrUnknownService](err)
		if unknown {
			return s.refuse(entry, CodeNotFound, err)
		}
		entry.Result = state.ResultFailed
		entry.Detail = err.Error()
		generation := s.store.Record(entry, false)
		return Response{Code: CodeFailed, Message: err.Error(), Generation: generation}
	}
	entry.Result = state.ResultOK
	if !changed {
		entry.Detail = "nothing to change"
	}
	generation := s.store.Record(entry, changed)
	resp := body(s.statusList(name))
	resp.Generation = generation
	return resp
}

// Audits an operation that was not carried out.
func (s *Server) refuse(entry state.Entry, code string, cause error) Response {
	entry.Result = state.ResultRefused
	entry.Detail = cause.Error()
	generation := s.store.Record(entry, false)
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
			OpImagePush, OpImageList, OpServicePut, OpServicePrune,
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

func (s *Server) apply(ctx context.Context, req Request, actor string) Response {
	entry := state.Entry{Operation: OpApply, Target: s.services, Actor: actor, OnBehalfOf: req.OnBehalfOf}

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
		generation := s.store.Record(entry, len(applied) > 0)
		return Response{
			Code:       CodeFailed,
			Message:    loadErr.Error(),
			Generation: generation,
			Body:       mustMarshal(applied),
		}
	}
	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("%d change(s)", len(applied))
	generation := s.store.Record(entry, len(applied) > 0)
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
