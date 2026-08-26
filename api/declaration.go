package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
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
