package supervisor

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/service"
)

// The artifact whose presence says a service knows how to back its data up.
// What the script does is the operator's; what hostd does is run it, record
// every line and the exit code, and hand over the file it left.
const BackupScript = "backup_data.sh"

// A run that hangs must not hold a place for ever, and a backup declares no
// schedule to borrow a run-timeout from unless the operator set one.
const defaultBackupLimit = 30 * time.Minute

type ErrNoBackup struct{ Name string }

func (e ErrNoBackup) Error() string {
	return fmt.Sprintf("%s has no %s among its files, so it does not say how to back its data up", e.Name, BackupScript)
}

// What a finished backup hands the caller: where the file is, and which
// container still holds it. The container outlives the run on purpose — the
// file lives in its filesystem, and removing it is the caller's job once the
// bytes are out.
type BackupRun struct {
	Run       string
	Container string
	// The path inside the container the script was told to write.
	Path string
	Exit int
}

// Backup runs the service's own script in a fresh container — same image, same
// volumes, same network, so pg_dump reaches the live server by name and a file
// database is on the same mount — and waits for it. Everything the script says
// lands in the service's timeline, stdout and stderr alike, and the exit code
// is recorded whether it worked or not.
func (s *Supervisor) Backup(ctx context.Context, name string) (BackupRun, error) {
	svc, declared := s.declaration(name)
	if !declared {
		return BackupRun{}, ErrUnknownService{Name: name}
	}
	if s.client() == nil {
		return BackupRun{}, docker.ErrNoRuntime
	}
	_, err := os.Stat(filepath.Join(s.services, name+service.ArtifactSuffix, BackupScript))
	if err != nil {
		return BackupRun{}, ErrNoBackup{Name: name}
	}
	// The script reaches the container through the config mount, so a service
	// that carries the script but mounts it nowhere cannot run it.
	if svc.Config == "" {
		return BackupRun{}, fmt.Errorf(
			"%s carries %s but declares no config mount, so the script has no path inside the container", name, BackupScript)
	}

	slot := s.now()
	run := runID(slot)
	target := fmt.Sprintf("/tmp/%s_%s.backup", name, slot.UTC().Format("20060102T150405"))
	// The declaration's own command is replaced by the script's, and nothing
	// else changes: the data the script needs is where the service's mounts
	// put it.
	svc.Args = []string{"/bin/sh", path.Join(svc.Config, BackupScript), target}

	limit := defaultBackupLimit
	if svc.IsJob() {
		if declared := svc.RunLimit(); declared > 0 {
			limit = declared
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	id, err := s.createRun(runCtx, svc, slot)
	if err != nil {
		return BackupRun{}, err
	}
	s.logRun(name, run, logs.EventBackup, fmt.Sprintf("backup %s started as container %s, running %s", run, short(id), BackupScript))
	s.startFollowing(held{ID: id, Name: runName(name, slot), Running: true, Service: name})

	code, err := s.client().Wait(runCtx, id)
	if err != nil {
		// The container stays for the operator to look at; a backup that died
		// of a timeout is exactly the one worth reading.
		s.logRun(name, run, logs.EventBackup, fmt.Sprintf("backup %s ended and could not be read: %v", run, err))
		return BackupRun{Run: run, Container: id, Path: target, Exit: -1}, fmt.Errorf("backup of %s: %w", name, err)
	}
	// The reader is still draining what the script wrote as it ended.
	time.Sleep(500 * time.Millisecond)
	took := s.now().Sub(slot).Truncate(time.Millisecond)
	s.logRun(name, run, logs.EventBackup, fmt.Sprintf("backup %s finished with exit %d after %s", run, code, took))
	return BackupRun{Run: run, Container: id, Path: target, Exit: code}, nil
}
