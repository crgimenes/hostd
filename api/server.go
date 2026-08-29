package api

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"slices"
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
	Deploy(svc service.Service) (bool, error)
	Remove(name string) (bool, error)
	RunNow(name string) (string, error)
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
	// Marked as ours before it is reported as arrived. Other things on this
	// machine build images too and those are nobody's business here; what
	// matters is that what hostd put there can always be told apart, and that
	// every version keeps a name of its own instead of becoming untagged when
	// the next push takes the tag it came under.
	mark := ManagedTag(sum)
	err = s.runtime.Tag(ctx, digest, RepositoryOf(req.Name), mark)
	if err != nil {
		return Response{Code: CodeFailed, Message: fmt.Sprintf("%s was loaded but could not be marked as ours: %v", req.Name, err)}
	}
	s.log.Append(logs.Record{
		Service: "hostd",
		Stream:  logs.StreamEvent,
		Kind:    logs.EventImage,
		Text:    fmt.Sprintf("received image %s as %s tagged %s, %d bytes, content sha256:%s", req.Name, digest, mark, total, sum),
	})
	return body(Image{
		Name:    req.Name,
		Ref:     RepositoryOf(req.Name) + ":" + mark,
		Digest:  digest,
		Bytes:   float64(total),
		Content: sum,
	})
}

// The tag hostd puts on an image it received, derived from the bytes that
// arrived rather than from the name they came under. Two pushes of one tag are
// two versions, and both stay nameable — which is what a rollback needs and
// what stops a displaced version becoming anonymous.
const ManagedTagPrefix = "hostd-"

func ManagedTag(contentSHA256 string) string {
	const shown = 12
	if len(contentSHA256) > shown {
		contentSHA256 = contentSHA256[:shown]
	}
	return ManagedTagPrefix + contentSHA256
}

// The repository half of an image reference. The tag is what follows the last
// colon, unless that colon is inside a registry's host:port, which is what the
// slash after it means.
func RepositoryOf(reference string) string {
	at := strings.LastIndex(reference, ":")
	if at < 0 || strings.Contains(reference[at+1:], "/") {
		return reference
	}
	return reference[:at]
}

// The repository hostd marked this image under, and whether hostd marked it at
// all. The mark carries the grouping a prune needs: every version pushed under
// one repository is one line of versions, however the moving tag has since
// travelled.
func markedRepository(tags []string) (string, bool) {
	tag, marked := managedTag(tags)
	if !marked {
		return "", false
	}
	return tag[:strings.LastIndex(tag, ":")], true
}

// The stamp itself, whole. It is the name a rollback writes down, because it is
// the one a later push of the same image cannot take away.
func managedTag(tags []string) (string, bool) {
	for _, tag := range tags {
		at := strings.LastIndex(tag, ":")
		if at < 0 || !strings.HasPrefix(tag[at+1:], ManagedTagPrefix) {
			continue
		}
		return tag, true
	}
	return "", false
}

// What a push answers with, so the declaration can be pinned to what really
// arrived rather than to the tag it was called by.
type Image struct {
	Name string `filo:"name" json:"name"`
	// What to write in the declaration to pin this exact version: the stamp
	// this machine put on it, derived from the bytes that arrived. The name it
	// came under is a moving tag, and a later push takes it away.
	Ref string `filo:"ref" json:"ref"`
	// What this machine now calls it. Two machines loading the same archive
	// arrive at different ids, so this is the one to declare here and nowhere
	// else.
	Digest string  `filo:"digest" json:"digest"`
	Bytes  float64 `filo:"bytes" json:"bytes"`
	// The hash of what crossed the wire, which is the same on both sides and
	// is what proves the transfer.
	Content string `filo:"content-sha256" json:"content-sha256"`
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
	// What would stop this image being removed, empty for one nothing holds.
	// Whoever is deciding what to delete is asking exactly this.
	UsedBy string `filo:"used-by" json:"used-by"`
	// Put here by hostd, and so ours to account for and one day to prune.
	// Anything else on the machine was built or pulled by something else, and
	// is reported without ever being a candidate for removal.
	Managed bool `filo:"managed" json:"managed"`
}

// What holds an image on this machine. A container is the stronger hold: the
// runtime itself refuses to remove an image one references, whether hostd
// created that container or not. A declaration is the weaker one — nothing
// stops the removal today, and the service would fail to start next time.
const (
	UsedByContainer = "container"
	UsedByDeclared  = "declared"
)

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
	// A declaration this machine cannot read is one whose image would be
	// reported as held by nothing, which is the answer that gets an image
	// deleted. Refusing to answer is the safe direction.
	holders, err := s.imageHolders(ctx)
	if err != nil {
		return Response{Code: CodeFailed, Message: fmt.Sprintf("cannot tell which images are in use, so none can be called free: %v", err)}
	}
	out := make([]ImageEntry, 0, len(held))
	for _, image := range held {
		_, marked := markedRepository(image.Tags)
		out = append(out, ImageEntry{
			Digest:  image.Digest,
			Tags:    image.Tags,
			Bytes:   float64(image.Bytes),
			Created: float64(image.Created.UnixMilli()),
			UsedBy:  heldBy(image, holders),
			Managed: marked,
		})
	}
	// Newest first, here rather than in each client: the order is part of the
	// answer, so the CLI, the panel and an agent all read the same list without
	// three implementations of one rule. The digest breaks ties so two images
	// of the same second do not swap places between calls.
	slices.SortFunc(out, func(a, b ImageEntry) int {
		if a.Created != b.Created {
			return cmp.Compare(b.Created, a.Created)
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	return body(out)
}

// imageHolders maps every name that holds an image — a digest or a tag — onto
// what holds it.
func (s *Server) imageHolders(ctx context.Context) (map[string]string, error) {
	holders := make(map[string]string)
	// Desired state, which holds an image even where nothing runs yet: a
	// service declared and not started still needs the image it names.
	declared, err := service.LoadDir(ctx, s.services)
	if err != nil {
		return nil, err
	}
	for _, def := range declared {
		if def.Image == "" {
			continue
		}
		holders[def.Image] = UsedByDeclared
	}
	// Observed state, and the stronger claim, so it is written last.
	running, err := s.runtime.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, container := range running {
		if container.Digest == "" {
			continue
		}
		holders[container.Digest] = UsedByContainer
	}
	return holders, nil
}

// A declaration names an image by tag or by digest, and both are how a service
// would be started again, so a hold on either is a hold on the image.
func heldBy(image docker.ImageSummary, holders map[string]string) string {
	reason, held := holders[image.Digest]
	if held {
		return reason
	}
	for _, tag := range image.Tags {
		reason, held = holders[tag]
		if held {
			return reason
		}
	}
	return ""
}

// How many versions of one image survive a prune by default. Enough to go back
// from a bad deploy more than once; few enough that a machine deployed to daily
// is not storing a month of them.
const DefaultImageKeep = 3

// One image a prune would remove, or did.
type ImageChange struct {
	Digest     string   `filo:"digest" json:"digest"`
	Repository string   `filo:"repository" json:"repository"`
	Tags       []string `filo:"tags" json:"tags"`
	Bytes      float64  `filo:"bytes" json:"bytes"`
	Removed    bool     `filo:"removed" json:"removed"`
	// What the runtime said when it would not go. Reporting a removal that did
	// not happen is worse than reporting a failure.
	Problem string `filo:"problem" json:"problem"`
}

type ImagePrune struct {
	Keep    int           `filo:"keep" json:"keep"`
	Kept    int           `filo:"kept" json:"kept"`
	Remove  []ImageChange `filo:"remove" json:"remove"`
	Applied bool          `filo:"applied" json:"applied"`
}

// imagesToPrune is the whole policy, in one function, so the plan and the
// removal cannot drift apart: only what hostd marked is ever a candidate,
// anything a container or a declaration holds stays whatever its age, and the
// newest few of each repository this machine still names stay so a bad deploy
// can be rolled back. Kept counts only marked images — what something else on
// this machine built is neither kept nor removed by us, it is simply not ours.
func imagesToPrune(held []docker.ImageSummary, holders map[string]string, keep int) (remove []ImageChange, kept int) {
	lines := make(map[string][]docker.ImageSummary)
	for _, image := range held {
		repo, marked := markedRepository(image.Tags)
		if !marked {
			continue
		}
		lines[repo] = append(lines[repo], image)
	}
	named := namedRepositories(held, holders)
	for _, repo := range slices.Sorted(maps.Keys(lines)) {
		versions := lines[repo]
		// Newest first, the same order the list answers in, so what an operator
		// saw at the top of the screen is what survives.
		slices.SortFunc(versions, func(a, b docker.ImageSummary) int {
			if !a.Created.Equal(b.Created) {
				return b.Created.Compare(a.Created)
			}
			return strings.Compare(a.Digest, b.Digest)
		})
		// Keeping versions is for going back, and there is nowhere to go back
		// to for a repository this machine no longer names: the last service
		// that used it was removed, and putting it back is a deploy, which
		// resolves the image on its own. Without this a repository with a
		// single version was kept for ever, which is how the image of a
		// service removed by a daemon older than 2026-08-26 stayed on disk.
		floor := keep
		if !named[repo] {
			floor = 0
		}
		for at, image := range versions {
			if at < floor || heldBy(image, holders) != "" {
				kept++
				continue
			}
			remove = append(remove, ImageChange{
				Digest:     image.Digest,
				Repository: repo,
				Tags:       image.Tags,
				Bytes:      float64(image.Bytes),
			})
		}
	}
	return remove, kept
}

// The repositories something on this machine still names: a declaration naming
// any version of one, or a container running any version of one. A declaration
// pointing at a version the machine does not hold yet still names the
// repository — the older versions of that service are exactly what a rollback
// needs, so the tag it names is enough.
func namedRepositories(held []docker.ImageSummary, holders map[string]string) map[string]bool {
	named := make(map[string]bool)
	for name, by := range holders {
		if by == UsedByDeclared {
			named[RepositoryOf(name)] = true
		}
	}
	// A container holds an image by digest, which says nothing about the
	// repository, so the marked image itself is what connects the two.
	for _, image := range held {
		repo, marked := markedRepository(image.Tags)
		if marked && heldBy(image, holders) != "" {
			named[repo] = true
		}
	}
	return named
}

// pruneImages plans, and carries the plan out only when told to. It is one
// computation either way: a dry run that ran different code would be
// decoration.
// pullImage has this machine fetch an image from its registry — what a deploy
// falls back to when the operator's machine cannot carry the image (a public
// multi-arch base never survives a save on another platform). The answer names
// what the machine now holds.
func (s *Server) pullImage(ctx context.Context, req Request, actor string) Response {
	if s.runtime == nil {
		return Response{Code: CodeUnavailable, Message: "this machine has no container runtime to pull into"}
	}
	if req.Name == "" {
		return Response{Code: CodeInvalid, Message: "a pull needs the image to fetch"}
	}
	entry := state.Entry{Operation: req.Op, Target: req.Name, Actor: actor, OnBehalfOf: req.OnBehalfOf}
	// The pull's own progress goes into this machine's timeline, so it reaches
	// whoever is watching by the same road every other line takes. Waiting in
	// front of a silent gigabyte is what makes somebody click again.
	said := req.Service
	if said == "" {
		said = "hostd"
	}
	err := s.runtime.Pull(ctx, req.Name, func(progress string) {
		s.log.Append(logs.Record{
			Service: said,
			Stream:  logs.StreamEvent,
			Kind:    logs.EventImage,
			Text:    progress,
		})
	})
	if err != nil {
		entry.Result = state.ResultFailed
		entry.Detail = err.Error()
		// An image is not desired state, so the generation stays put.
		generation := s.store.Record(entry, false)
		return Response{Code: CodeFailed, Message: err.Error(), Generation: generation}
	}
	found, err := s.runtime.Image(ctx, req.Name)
	if err != nil {
		return Response{Code: CodeFailed, Message: fmt.Sprintf("the pull reported success and yet %s is not here: %v", req.Name, err)}
	}
	entry.Result = state.ResultOK
	entry.Detail = found.Digest
	generation := s.store.Record(entry, false)
	resp := body(Image{Digest: found.Digest})
	resp.Generation = generation
	return resp
}

func (s *Server) pruneImages(ctx context.Context, req Request, actor string) Response {
	if s.runtime == nil {
		return Response{Code: CodeUnavailable, Message: "this machine has no container runtime to remove images from"}
	}
	keep := req.Keep
	if keep <= 0 {
		keep = DefaultImageKeep
	}
	held, err := s.runtime.Images(ctx)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	holders, err := s.imageHolders(ctx)
	if err != nil {
		return Response{Code: CodeFailed, Message: fmt.Sprintf("cannot tell which images are in use, so none can be removed: %v", err)}
	}
	remove, kept := imagesToPrune(held, holders, keep)
	out := ImagePrune{Keep: keep, Kept: kept, Remove: remove}
	if !req.AllowDestructive {
		return body(out)
	}

	entry := state.Entry{Operation: req.Op, Target: fmt.Sprintf("keep %d", keep), Actor: actor, OnBehalfOf: req.OnBehalfOf}
	failed := 0
	for at := range out.Remove {
		// By digest, never by tag: removing a tag from an image that has
		// several only drops the name, and the disk would not move.
		err = s.runtime.RemoveImage(ctx, out.Remove[at].Digest)
		if err != nil {
			out.Remove[at].Problem = err.Error()
			failed++
			continue
		}
		out.Remove[at].Removed = true
		s.log.Append(logs.Record{
			Service: "hostd",
			Stream:  logs.StreamEvent,
			Kind:    logs.EventImage,
			Text: fmt.Sprintf("removed image %s of %s, %.0f bytes",
				out.Remove[at].Digest, out.Remove[at].Repository, out.Remove[at].Bytes),
		})
	}
	out.Applied = true
	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("removed %d of %d, kept %d", len(out.Remove)-failed, len(out.Remove), kept)
	if failed > 0 {
		entry.Result = state.ResultFailed
	}
	// Audited but not a new generation: what a generation guards is the desired
	// state two operators could overwrite for each other, and no image was ever
	// part of that. Bumping here would refuse somebody's apply for a reason
	// that has nothing to do with their apply.
	s.store.Record(entry, false)
	if failed > 0 {
		resp := body(out)
		resp.Code = CodeFailed
		resp.Message = fmt.Sprintf("%d image(s) could not be removed", failed)
		return resp
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
		// The write budget starts when there is something to write. Set before
		// the operation, it would be spent by the operation itself: stopping a
		// container whose process ignores SIGTERM costs the runtime's whole
		// grace period, and the answer would then be dropped on a connection
		// the client is still holding open — which reads, at the far end, as a
		// daemon that did nothing, for work it in fact finished.
		err = conn.SetWriteDeadline(time.Now().Add(requestTimeout))
		if err != nil {
			return
		}
		err = WriteMessage(conn, resp)
		if err != nil {
			return
		}
	}
}

// appendLogs writes what a program on this machine sent into the same timeline
// the containers write to — same store, same retention, same query. It is how
// something running outside a container (a cron script, a service hostd does
// not supervise) gets its lines beside everything else's; a shell needs no
// library for it:
//
//	printf '(list (tuple "op" "log.append") (tuple "service" "backup") (tuple "body" "done"))\n' \
//	  | nc -U /run/hostd/hostd.sock
//
// Not audited and no generation: lines are observations, not changes to what
// this machine is meant to run.
func (s *Server) appendLogs(req Request) Response {
	if !service.ValidName(req.Service) {
		return Response{Code: CodeInvalid, Message: fmt.Sprintf("%q is not a service name", req.Service)}
	}
	stream := req.Stream
	if stream == "" {
		stream = logs.StreamOut
	}
	// Never "event": events are hostd's own facts about lifecycles, and a
	// timeline where anything can write them is one nobody can trust.
	if stream != logs.StreamOut && stream != logs.StreamErr {
		return Response{Code: CodeInvalid, Message: fmt.Sprintf(
			"stream %q is not %q or %q; events are hostd's own", stream, logs.StreamOut, logs.StreamErr)}
	}
	lines := 0
	for line := range strings.Lines(req.Body) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		s.log.Append(logs.Record{Service: req.Service, Stream: stream, Text: line})
		lines++
	}
	return body(struct {
		Lines int `filo:"lines" json:"lines"`
	}{lines})
}

// Reads answer straight away; mutations pass the generation check first and
// are audited whether carried out or refused.
func (s *Server) dispatch(ctx context.Context, req Request, actor string) Response {
	switch req.Op {
	case OpDescribe:
		return s.stamp(s.describe(ctx))
	case OpStatus, OpServiceList:
		return s.stamp(body(s.sup.Status()))
	case OpPlan:
		return s.stamp(s.planOnly(ctx))
	case OpAudit:
		return s.stamp(body(s.store.Recent(req.Limit)))
	case OpLogSearch:
		return s.stamp(s.searchLogs(req))
	case OpLogAppend:
		return s.stamp(s.appendLogs(req))
	case OpMetrics:
		return s.stamp(s.readMetrics(req))
	case OpImageList:
		return s.stamp(s.listImages(ctx))
	case OpServiceVersions:
		return s.stamp(s.serviceVersions(ctx, req))
	case OpJobRun:
		return s.stamp(s.runJob(req, actor))
	case OpImagePrune:
		return s.stamp(s.pruneImages(ctx, req, actor))
	case OpImagePull:
		return s.stamp(s.pullImage(ctx, req, actor))
	case OpServiceStart:
		return s.mutate(req, actor, req.Name, s.sup.Start)
	case OpServiceStop:
		return s.mutate(req, actor, req.Name, s.sup.Stop)
	case OpServiceRestrt:
		return s.mutate(req, actor, req.Name, s.sup.Restart)
	case OpServiceRedeploy:
		return s.mutate(req, actor, req.Name, s.redeploy(ctx))
	case OpServiceRemove:
		return s.stamp(s.removeService(ctx, req, actor))
	case OpApply:
		return s.apply(ctx, req, actor)
	case OpServicePut:
		return s.stamp(s.putService(ctx, req, actor))
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

// redeploy reads the declaration as this machine holds it ON DISK, which is
// what a deploy just put there: the supervisor's memory of a service it never
// met cannot be the gate to running one.
func (s *Server) redeploy(ctx context.Context) func(string) (bool, error) {
	return func(name string) (bool, error) {
		svc, err := service.ParseFile(ctx, filepath.Join(s.services, name+service.Extension))
		if err != nil {
			if os.IsNotExist(err) {
				return false, supervisor.ErrUnknownService{Name: name}
			}
			return false, err
		}
		return s.sup.Deploy(svc)
	}
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
	Version    string   `filo:"version" json:"version"`
	Protocol   int      `filo:"protocol" json:"protocol"`
	Schema     int      `filo:"schema" json:"schema"`
	Operations []string `filo:"operations" json:"operations"`
	// What this machine can actually run. A fleet where every machine is the
	// same does not need these; one where they differ cannot place a service
	// without them, and finding out by watching a container fail to start is
	// finding out too late.
	//
	// Arch is the runtime's own, never the daemon binary's: what has to execute
	// the image is the container, and a hostd built for one architecture can
	// perfectly well talk to a runtime on another.
	Arch string `filo:"arch" json:"arch"`
	// Empty where no container runtime answers, which is a machine that can
	// hold declarations and run nothing.
	Runtime     string  `filo:"runtime" json:"runtime"`
	CPUs        int     `filo:"cpus" json:"cpus"`
	MemoryBytes float64 `filo:"memory-bytes" json:"memory-bytes"`
}

func (s *Server) describe(ctx context.Context) Response {
	info := s.runtimeInfo(ctx)
	return body(Description{
		Version:     version.Version,
		Protocol:    version.Protocol,
		Schema:      version.Schema,
		Arch:        info.Arch,
		Runtime:     info.Version,
		CPUs:        goruntime.NumCPU(),
		MemoryBytes: s.hostMemory(),
		Operations: []string{
			OpDescribe, OpStatus, OpServiceList,
			OpServiceStart, OpServiceStop, OpServiceRestrt, OpServiceRedeploy, OpServiceRemove,
			OpPlan, OpApply, OpAudit, OpLogSearch, OpLogFollow, OpMetrics,
			OpImagePush, OpImagePull, OpImageList, OpImagePrune, OpServicePut,
			OpServiceVersions, OpJobRun,
		},
	})
}

// A machine with no runtime says so by saying nothing, rather than by claiming
// the daemon binary's architecture — which would be a confident wrong answer to
// "can this service live here".
func (s *Server) runtimeInfo(ctx context.Context) docker.ServerInfo {
	if s.runtime == nil {
		return docker.ServerInfo{}
	}
	info, err := s.runtime.Server(ctx)
	if err != nil {
		return docker.ServerInfo{}
	}
	return info
}

// The sampler already reads this every round, so describe asks the store rather
// than the kernel: one reader of /proc, and an answer that is missing on a
// machine whose sampler has not run rather than invented.
func (s *Server) hostMemory() float64 {
	if s.metrics == nil {
		return 0
	}
	series, err := s.metrics.Latest(metrics.Query{
		Scope:  metrics.ScopeHost,
		Metric: metrics.MetricMemoryTotal,
	}, latestWindow)
	if err != nil {
		return 0
	}
	for _, one := range series {
		for _, point := range one.Points {
			return point.Value
		}
	}
	return 0
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
