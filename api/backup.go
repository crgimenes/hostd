package api

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// What the response line carries before the bytes: the name to save under and
// how much is coming, read out of the tar the runtime handed over.
type Backup struct {
	Run   string  `filo:"run" json:"run"`
	File  string  `filo:"file" json:"file"`
	Bytes float64 `filo:"bytes" json:"bytes"`
}

// A backup of a database is allowed to take a while, but a connection that
// went quiet for an hour is not a backup, it is a hang.
const backupWait = time.Hour

// backupService runs the service's own backup_data.sh and streams the file it
// produced back over the same connection: the response line first, then the
// bytes in chunks, the mirror of how an image arrives. It owns the connection
// like push does, because the answer does not fit a line.
//
// Everything that happens is in the machine's log — every line the script
// said, and the exit code — and the audit records who asked.
func (s *Server) backupService(ctx context.Context, conn net.Conn, req Request, actor string) {
	answer := func(resp Response) {
		_ = conn.SetWriteDeadline(time.Now().Add(requestTimeout))
		_ = WriteMessage(conn, s.stamp(resp))
	}
	entry := state.Entry{Operation: OpServiceBackup, Target: req.Name, Actor: actor, OnBehalfOf: req.OnBehalfOf}

	runCtx, cancel := context.WithTimeout(ctx, backupWait)
	defer cancel()
	run, err := s.sup.Backup(runCtx, req.Name)
	if err != nil {
		code := CodeFailed
		_, unknown := errors.AsType[supervisor.ErrUnknownService](err)
		_, missing := errors.AsType[supervisor.ErrNoBackup](err)
		if unknown || missing {
			code = CodeNotFound
		}
		entry.Result = state.ResultFailed
		entry.Detail = err.Error()
		s.store.Record(entry, false)
		answer(Response{Code: code, Message: err.Error()})
		return
	}
	// The container holds the file; it leaves with it once the bytes are out,
	// and it leaves on the failure paths too — except the timeout above, where
	// supervisor.Backup deliberately keeps it for the operator to read.
	defer func() {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), requestTimeout)
		defer removeCancel()
		_ = s.runtime.Remove(removeCtx, run.Container)
	}()

	if run.Exit != 0 {
		message := fmt.Sprintf("backup run %s of %s exited %d; its output is in the log: hostctl log -service %s -run %s",
			run.Run, req.Name, run.Exit, req.Name, run.Run)
		entry.Result = state.ResultFailed
		entry.Detail = message
		s.store.Record(entry, false)
		answer(Response{Code: CodeFailed, Message: message})
		return
	}

	archive, err := s.runtime.Archive(runCtx, run.Container, run.Path)
	if err != nil {
		message := fmt.Sprintf("the script exited 0 and yet %s is not in the container: %v", run.Path, err)
		entry.Result = state.ResultFailed
		entry.Detail = message
		s.store.Record(entry, false)
		answer(Response{Code: CodeFailed, Message: message})
		return
	}
	defer func() { _ = archive.Close() }()

	// The runtime hands a tar over; what travels is the file alone, so the
	// client saves bytes instead of understanding a container's packaging.
	reader := tar.NewReader(archive)
	var header *tar.Header
	for {
		header, err = reader.Next()
		if err != nil {
			entry.Result = state.ResultFailed
			entry.Detail = "the archive holds no file: " + err.Error()
			s.store.Record(entry, false)
			answer(Response{Code: CodeFailed, Message: entry.Detail})
			return
		}
		if header.Typeflag == tar.TypeReg {
			break
		}
	}

	entry.Result = state.ResultOK
	entry.Detail = fmt.Sprintf("run %s, %d bytes", run.Run, header.Size)
	s.store.Record(entry, false)
	resp := body(Backup{Run: run.Run, File: header.FileInfo().Name(), Bytes: float64(header.Size)})
	answer(resp)
	// No deadline across the transfer: what bounds it is the chunked framing
	// and the caller's own patience, the same contract an image upload has.
	_ = conn.SetWriteDeadline(time.Time{})
	_ = WriteChunks(conn, io.LimitReader(reader, header.Size))
}
