package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crgimenes/hostd/service"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// One entry of a service's data, as the operator's file panel shows it. Times
// in milliseconds, like every other time on the wire.
type FileEntry struct {
	Name  string  `filo:"name" json:"name"`
	Dir   bool    `filo:"dir" json:"dir"`
	Bytes float64 `filo:"bytes" json:"bytes"`
	MS    float64 `filo:"modified-ms" json:"modified-ms"`
}

// What file.get announces before the bytes, and what file.put answers.
type FileTransfer struct {
	Path  string  `filo:"path" json:"path"`
	Bytes float64 `filo:"bytes" json:"bytes"`
}

// The files of a service are the NAMED volumes its declaration mounts — the
// data. Never the artifacts (configuration comes from the operator's tree, and
// a hand edit on the machine would be a change nobody committed) and never a
// bind mount (that is the machine's own filesystem, not the service's).
//
// The path on the wire is "volume/inside/path": the first segment picks the
// declared volume, the rest walks inside it. Everything is resolved under an
// os.Root of the volume's mountpoint, so a symlink a container planted cannot
// walk a root daemon out of the volume.
func (s *Server) fileRoot(ctx context.Context, svcName, wirePath string) (*os.Root, string, error) {
	if s.runtime == nil {
		return nil, "", errors.New("this machine has no container runtime, so no service has volumes here")
	}
	declared, err := service.ParseFile(ctx, s.declarationPath(svcName))
	if err != nil {
		return nil, "", fmt.Errorf("no declaration for %q on this machine: %w", svcName, err)
	}
	volume, inside, _ := strings.Cut(strings.Trim(wirePath, "/"), "/")
	names, err := namedVolumes(declared)
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(names, volume) {
		if len(names) == 0 {
			return nil, "", fmt.Errorf("%s declares no named volume, so it has no files here", svcName)
		}
		return nil, "", fmt.Errorf("%s has no volume %q; it has %s", svcName, volume, strings.Join(names, ", "))
	}
	mountpoint, err := s.runtime.VolumeMountpoint(ctx, supervisor.VolumeName(svcName, volume))
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(mountpoint)
	if err != nil {
		return nil, "", err
	}
	if inside == "" {
		inside = "."
	}
	return root, path.Clean(inside), nil
}

func (s *Server) declarationPath(name string) string {
	return filepath.Join(s.services, name+service.Extension)
}

func namedVolumes(declared service.Service) ([]string, error) {
	mounts, err := declared.Mounts()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, mount := range mounts {
		if mount.Named {
			names = append(names, mount.Source)
		}
	}
	return names, nil
}

// listFiles answers one directory of one volume. Asked with only the service,
// it answers the volumes themselves, so the panel's first click needs no
// knowledge of the declaration.
func (s *Server) listFiles(ctx context.Context, req Request) Response {
	if !service.ValidName(req.Service) {
		return Response{Code: CodeInvalid, Message: fmt.Sprintf("%q is not a service name", req.Service)}
	}
	if strings.Trim(req.Name, "/") == "" {
		return s.listVolumes(ctx, req.Service)
	}
	root, inside, err := s.fileRoot(ctx, req.Service, req.Name)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), inside)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	out := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		held := FileEntry{Name: entry.Name(), Dir: entry.IsDir()}
		if !entry.IsDir() {
			held.Bytes = float64(info.Size())
			held.MS = float64(info.ModTime().UnixMilli())
		}
		out = append(out, held)
	}
	slices.SortFunc(out, func(a, b FileEntry) int {
		if a.Dir != b.Dir {
			// Directories first, the way every file panel reads.
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return body(out)
}

// The volumes a declaration names, presented as directories: the top level of
// a service's data.
func (s *Server) listVolumes(ctx context.Context, svcName string) Response {
	declared, err := service.ParseFile(ctx, s.declarationPath(svcName))
	if err != nil {
		return Response{Code: CodeFailed, Message: fmt.Sprintf("no declaration for %q on this machine: %v", svcName, err)}
	}
	names, err := namedVolumes(declared)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	out := []FileEntry{}
	for _, name := range names {
		out = append(out, FileEntry{Name: name, Dir: true})
	}
	return body(out)
}

// deleteFile removes ONE file of a service's data, by name — never a
// directory, and never anything a redeploy would bring back: this is the undo
// of an upload, not a cleanup tool. Audited, because "who deleted that file"
// is exactly what an audit is for.
func (s *Server) deleteFile(ctx context.Context, req Request, actor string) Response {
	entry := state.Entry{Operation: OpFileDelete, Target: req.Service + ":" + req.Name, Actor: actor, OnBehalfOf: req.OnBehalfOf}
	fail := func(resp Response) Response {
		entry.Result = state.ResultFailed
		entry.Detail = resp.Message
		resp.Generation = s.store.Record(entry, false)
		return resp
	}
	if !service.ValidName(req.Service) {
		return fail(Response{Code: CodeInvalid, Message: fmt.Sprintf("%q is not a service name", req.Service)})
	}
	root, inside, err := s.fileRoot(ctx, req.Service, req.Name)
	if err != nil {
		return fail(Response{Code: CodeFailed, Message: err.Error()})
	}
	defer func() { _ = root.Close() }()
	if inside == "." {
		return fail(Response{Code: CodeInvalid, Message: "name the file to delete, not the volume"})
	}
	info, err := root.Lstat(inside)
	if err != nil {
		return fail(Response{Code: CodeFailed, Message: err.Error()})
	}
	if info.IsDir() {
		return fail(Response{Code: CodeInvalid, Message: fmt.Sprintf("%s is a directory, and this deletes one file by name", req.Name)})
	}
	err = root.Remove(inside)
	if err != nil {
		return fail(Response{Code: CodeFailed, Message: err.Error()})
	}
	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("%d bytes", info.Size())
	generation := s.store.Record(entry, false)
	resp := body(FileTransfer{Path: req.Name, Bytes: float64(info.Size())})
	resp.Generation = generation
	return resp
}

// getFile owns the connection like a backup does: the response line announces
// path and size, the bytes follow in chunks. Audited — who took which file is
// the question an audit answers.
func (s *Server) getFile(ctx context.Context, conn net.Conn, req Request, actor string) {
	answer := func(resp Response) {
		_ = conn.SetWriteDeadline(time.Now().Add(requestTimeout))
		_ = WriteMessage(conn, s.stamp(resp))
	}
	entry := state.Entry{Operation: OpFileGet, Target: req.Service + ":" + req.Name, Actor: actor, OnBehalfOf: req.OnBehalfOf}
	refuse := func(resp Response) {
		entry.Result = state.ResultFailed
		entry.Detail = resp.Message
		s.store.Record(entry, false)
		answer(resp)
	}
	if !service.ValidName(req.Service) {
		refuse(Response{Code: CodeInvalid, Message: fmt.Sprintf("%q is not a service name", req.Service)})
		return
	}
	root, inside, err := s.fileRoot(ctx, req.Service, req.Name)
	if err != nil {
		refuse(Response{Code: CodeFailed, Message: err.Error()})
		return
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(inside)
	if err != nil {
		refuse(Response{Code: CodeFailed, Message: err.Error()})
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		refuse(Response{Code: CodeFailed, Message: err.Error()})
		return
	}
	if info.IsDir() {
		refuse(Response{Code: CodeInvalid, Message: fmt.Sprintf("%s is a directory; name a file in it", req.Name)})
		return
	}
	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("%d bytes", info.Size())
	s.store.Record(entry, false)
	answer(body(FileTransfer{Path: req.Name, Bytes: float64(info.Size())}))
	_ = conn.SetWriteDeadline(time.Time{})
	// The size announced is the size sent: a file growing under the read would
	// otherwise desynchronise the framing against what the client expects.
	_ = WriteChunks(conn, io.LimitReader(file, info.Size()))
}

// putFile reads the bytes that follow the request the way an image push does,
// and lands them atomically: half an upload must never be readable under the
// final name, because the service beside it reads that name.
func (s *Server) putFile(ctx context.Context, reader *bufio.Reader, req Request, actor string) Response {
	entry := state.Entry{Operation: OpFilePut, Target: req.Service + ":" + req.Name, Actor: actor, OnBehalfOf: req.OnBehalfOf}
	refuse := func(resp Response) Response {
		// The bytes are already on their way; leaving them in the pipe would
		// desynchronise the connection.
		_, _ = ReadChunks(reader, io.Discard, nil)
		entry.Result = state.ResultFailed
		entry.Detail = resp.Message
		resp.Generation = s.store.Record(entry, false)
		return resp
	}
	if !service.ValidName(req.Service) {
		return refuse(Response{Code: CodeInvalid, Message: fmt.Sprintf("%q is not a service name", req.Service)})
	}
	root, inside, err := s.fileRoot(ctx, req.Service, req.Name)
	if err != nil {
		return refuse(Response{Code: CodeFailed, Message: err.Error()})
	}
	defer func() { _ = root.Close() }()
	if inside == "." || strings.HasSuffix(req.Name, "/") {
		return refuse(Response{Code: CodeInvalid, Message: "name the file to write, not a directory"})
	}

	temporary := inside + ".hostd-upload"
	file, err := root.Create(temporary)
	if err != nil {
		return refuse(Response{Code: CodeFailed, Message: err.Error()})
	}
	received, err := ReadChunks(reader, file, nil)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = root.Rename(temporary, inside)
	}
	if err != nil {
		_ = root.Remove(temporary)
		entry.Result = state.ResultFailed
		entry.Detail = err.Error()
		generation := s.store.Record(entry, false)
		return Response{Code: CodeFailed, Message: err.Error(), Generation: generation}
	}
	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("%d bytes", received)
	// Data changed, not what the machine is meant to run: the generation
	// stays, the audit carries who and how much.
	generation := s.store.Record(entry, false)
	resp := body(FileTransfer{Path: req.Name, Bytes: float64(received)})
	resp.Generation = generation
	return resp
}
