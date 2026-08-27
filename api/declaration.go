package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// A declaration as it crosses the wire: the file itself, and whatever the
// operator keeps beside it. Content is base64 because an artifact is bytes —
// a certificate is not text, and a Caddyfile with a stray quote must arrive as
// written.
type Declaration struct {
	Name      string     `filo:"name"`
	Source    string     `filo:"source"`
	Artifacts []Artifact `filo:"artifacts"`
}

type Artifact struct {
	Name    string `filo:"name"`
	Content string `filo:"content-base64"`
}

// Sending is what makes the operator's versioned tree the source: the machine
// receives files it can read, keeps them where its own directory is, and
// applies them when told. Receiving is not applying — a file on disk has not
// changed what runs.
func Send(declaration service.Declaration) Declaration {
	out := Declaration{Name: declaration.Service.Name, Source: declaration.Source}
	for _, artifact := range declaration.Artifacts {
		out.Artifacts = append(out.Artifacts, Artifact{
			Name:    artifact.Name,
			Content: base64.StdEncoding.EncodeToString(artifact.Content),
		})
	}
	return out
}

// putService writes what arrived where this machine keeps its declarations. A
// file the daemon cannot read is refused before anything is written: half a
// declaration on disk is worse than none.
func (s *Server) putService(ctx context.Context, req Request, actor string) Response {
	var incoming Declaration
	err := filoconf.Decode(ctx, "declaration", req.Body, &incoming)
	if err != nil {
		return Response{Code: CodeInvalid, Message: err.Error()}
	}
	parsed, err := service.Parse(ctx, incoming.Name+service.Extension, incoming.Source)
	if err != nil {
		return Response{Code: CodeInvalid, Message: err.Error()}
	}
	if parsed.Name != incoming.Name {
		return Response{Code: CodeInvalid, Message: fmt.Sprintf(
			"the file declares %q and the service is called %q", parsed.Name, incoming.Name)}
	}
	if !service.ValidName(incoming.Name) {
		return Response{Code: CodeInvalid, Message: fmt.Sprintf("%q is not a service name", incoming.Name)}
	}

	entry := state.Entry{Operation: OpServicePut, Target: incoming.Name, Actor: actor, OnBehalfOf: req.OnBehalfOf}
	err = s.writeDeclaration(incoming)
	if err != nil {
		entry.Result = state.ResultFailed
		entry.Detail = err.Error()
		generation := s.store.Record(entry, false)
		return Response{Code: CodeFailed, Message: err.Error(), Generation: generation}
	}
	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("%d artifact(s)", len(incoming.Artifacts))
	// Receiving a file is not applying it, so the generation does not move:
	// what runs on this machine has not changed yet.
	generation := s.store.Record(entry, false)
	resp := body(incoming.Name)
	resp.Generation = generation
	return resp
}

func (s *Server) writeDeclaration(incoming Declaration) error {
	err := os.MkdirAll(s.services, 0o700)
	if err != nil {
		return err
	}
	err = writeAtomic(filepath.Join(s.services, incoming.Name+service.Extension), []byte(incoming.Source), 0o644)
	if err != nil {
		return err
	}

	artifacts := filepath.Join(s.services, incoming.Name+service.ArtifactSuffix)
	if len(incoming.Artifacts) == 0 {
		// A service that stopped needing files stops having a directory, so
		// what the machine holds is what the tree holds.
		return os.RemoveAll(artifacts)
	}
	err = os.MkdirAll(artifacts, 0o700)
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(incoming.Artifacts))
	for _, artifact := range incoming.Artifacts {
		// A name with a path in it would write outside the directory it was
		// sent for; the name is a name. ".." is the one that reads as a name
		// and is not: filepath.Base leaves it alone, and joining it lands in
		// the services directory itself. The write fails there because a
		// directory is not a file, which is luck rather than a guard.
		if !plainFileName(artifact.Name) {
			return fmt.Errorf("%q is not a file name", artifact.Name)
		}
		content, decodeErr := base64.StdEncoding.DecodeString(artifact.Content)
		if decodeErr != nil {
			return fmt.Errorf("%s: %w", artifact.Name, decodeErr)
		}
		err = writeAtomic(filepath.Join(artifacts, artifact.Name), content, 0o644)
		if err != nil {
			return err
		}
		kept = append(kept, artifact.Name)
	}
	// What the tree stopped carrying, the machine stops holding: a stale file
	// mounted into a container is a change nobody made.
	entries, err := os.ReadDir(artifacts)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if slices.Contains(kept, entry.Name()) {
			continue
		}
		err = os.Remove(filepath.Join(artifacts, entry.Name()))
		if err != nil {
			return err
		}
	}
	return nil
}

// Half a file on disk is a declaration that reads as something nobody wrote.
func plainFileName(name string) bool {
	switch name {
	case "", ".", "..":
		return false
	}
	return name == filepath.Base(name)
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()

	_, err = temporary.Write(content)
	if err != nil {
		_ = temporary.Close()
		return err
	}
	err = temporary.Chmod(mode)
	if err != nil {
		_ = temporary.Close()
		return err
	}
	// The rename is only atomic for data that reached the disk.
	err = temporary.Sync()
	if err != nil {
		_ = temporary.Close()
		return err
	}
	err = temporary.Close()
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Long enough for a local socket call that only untags an image, short enough
// that a wedged runtime does not hold the answer.
const releaseTimeout = 20 * time.Second

// What a removal did, piece by piece: the pieces have different owners and can
// end differently, and one word for the three would hide the one that stayed.
type Removal struct {
	Service   string `filo:"service"`
	Container string `filo:"container"`
	Image     string `filo:"image"`
}

// removeService takes a service off this machine: the container stops and
// goes, the declaration leaves the services directory, and the image is let
// go of when nothing else is holding it. The description still lives in the
// operator's tree, so a deploy puts the service back — data in volumes is
// never touched.
func (s *Server) removeService(ctx context.Context, req Request, actor string) Response {
	name := req.Name
	if !service.ValidName(name) {
		return Response{Code: CodeInvalid, Message: fmt.Sprintf("%q is not a service name", name)}
	}
	report := Removal{Service: name, Container: "there was none", Image: "none declared"}

	// The image the declaration names, read before the file goes.
	image := ""
	declared, err := service.ParseFile(ctx, filepath.Join(s.services, name+service.Extension))
	if err == nil {
		image = declared.Image
	}

	changed, err := s.sup.Remove(name)
	if err != nil {
		code := CodeFailed
		_, unknown := errors.AsType[supervisor.ErrUnknownService](err)
		if unknown {
			code = CodeNotFound
		}
		entry := state.Entry{Operation: OpServiceRemove, Target: name, Actor: actor,
			OnBehalfOf: req.OnBehalfOf, Result: state.ResultFailed, Detail: err.Error()}
		generation := s.store.Record(entry, false)
		return Response{Code: code, Message: err.Error(), Generation: generation}
	}
	if changed {
		report.Container = "stopped and removed"
	}

	err = os.Remove(filepath.Join(s.services, name+service.Extension))
	if err != nil && !os.IsNotExist(err) {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	err = os.RemoveAll(filepath.Join(s.services, name+service.ArtifactSuffix))
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}

	// Its own budget, deliberately: stopping the container can legitimately
	// spend the service's whole grace period, and the request's 30 seconds are
	// the SAME number as the default grace — so an image release that inherited
	// what was left always lost, and every removal of a container that ignores
	// SIGTERM left its image behind saying "context deadline exceeded".
	imageCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	report.Image = s.releaseImage(imageCtx, image)

	entry := state.Entry{Operation: OpServiceRemove, Target: name, Actor: actor,
		OnBehalfOf: req.OnBehalfOf, Result: state.ResultOK,
		Detail: fmt.Sprintf("container %s; image %s", report.Container, report.Image)}
	// Removing a service changes what this machine is meant to run, which is
	// exactly what the generation counts.
	generation := s.store.Record(entry, true)
	resp := body(report)
	resp.Generation = generation
	return resp
}

// releaseImage drops the reference a removed service held. The runtime refuses
// while any container still uses the image, and another declaration naming it
// keeps it too — a removal never takes what somebody else is standing on.
func (s *Server) releaseImage(ctx context.Context, image string) string {
	if image == "" {
		return "none declared"
	}
	if s.runtime == nil {
		return "kept: no container runtime on this machine"
	}
	declarations, _ := service.LoadDir(ctx, s.services)
	for _, other := range declarations {
		if other.Image == image {
			return fmt.Sprintf("kept: %s still declares it", other.Name)
		}
	}
	// Only what hostd put here is hostd's to take away — the same rule the
	// cleanup follows, and it has to be the same rule: a public base image
	// carries no stamp because it cannot be pushed at all, so removing a
	// service that used one would delete something this machine has to fetch
	// off the internet again on the very next deploy. Measured: fifty
	// megabytes and fifteen seconds, every time.
	held, listErr := s.runtime.Images(ctx)
	if listErr != nil {
		return "kept: cannot tell whether hostd put it here: " + listErr.Error()
	}
	if !ourImage(held, image) {
		return "kept: hostd did not put it here, so it is not hostd's to remove"
	}
	err := s.runtime.RemoveImage(ctx, image)
	if err != nil {
		return "kept: " + err.Error()
	}
	return "removed " + image
}

// Whether the machine holds this name as an image hostd stamped. A name the
// runtime does not hold at all answers false: not knowing is not permission.
func ourImage(held []docker.ImageSummary, image string) bool {
	for _, candidate := range held {
		if !slices.Contains(candidate.Tags, image) {
			continue
		}
		_, marked := managedTag(candidate.Tags)
		return marked
	}
	return false
}

// The set of services a tree carries, and what a machine stopped holding
// because of it.
type ServiceSet struct {
	Names []string `filo:"names"`
	// Asking: how many services the whole tree holds, so a machine can tell
	// "the tree declares none of mine" — which is ordinary in a fleet where
	// placement is declared — from "the tree declares nothing", which is what
	// a push from the wrong directory looks like.
	// Answering: how many declarations the machine holds now.
	Declared int `filo:"declared"`
}

// pruneServices takes away the declarations the operator's tree no longer
// carries. Removing a file is not removing a service: what runs keeps running
// until an apply says otherwise, and the plan is where somebody reviews that.
func (s *Server) pruneServices(ctx context.Context, req Request, actor string) Response {
	var keep ServiceSet
	err := filoconf.Decode(ctx, "prune", req.Body, &keep)
	if err != nil {
		return Response{Code: CodeInvalid, Message: err.Error()}
	}
	if keep.Declared == 0 {
		return Response{Code: CodeInvalid, Message: "a prune needs a tree that declares something; this one declares nothing"}
	}
	held, err := os.ReadDir(s.services)
	if os.IsNotExist(err) {
		return body(ServiceSet{})
	}
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}

	kept := 0
	var removed []string
	for _, entry := range held {
		name, is := strings.CutSuffix(entry.Name(), service.Extension)
		if !is {
			continue
		}
		if slices.Contains(keep.Names, name) {
			kept++
			continue
		}
		err = os.Remove(filepath.Join(s.services, name+service.Extension))
		if err != nil && !os.IsNotExist(err) {
			return Response{Code: CodeFailed, Message: err.Error()}
		}
		err = os.RemoveAll(filepath.Join(s.services, name+service.ArtifactSuffix))
		if err != nil {
			return Response{Code: CodeFailed, Message: err.Error()}
		}
		removed = append(removed, name)
	}
	if len(removed) == 0 {
		return body(ServiceSet{Declared: kept})
	}

	entry := state.Entry{
		Operation: OpServicePrune, Target: strings.Join(removed, ","),
		Actor: actor, OnBehalfOf: req.OnBehalfOf, Result: state.ResultOK,
		Detail: fmt.Sprintf("%d declaration(s) the tree no longer carries", len(removed)),
	}
	// What runs has not changed, so the generation does not move either.
	generation := s.store.Record(entry, false)
	resp := body(ServiceSet{Names: removed, Declared: kept})
	resp.Generation = generation
	return resp
}
