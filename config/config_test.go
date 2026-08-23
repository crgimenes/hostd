package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crgimenes/hostd/logs"
)

// Without the redirection, hostd points at the real host. That is the whole
// point: the test environment is a redirection of the same paths, never a
// different code path.
func TestDefaultPathsAreTheRealHost(t *testing.T) {
	t.Setenv(RootEnv, "")
	p := Locate()
	if p.ConfigDir != DefaultConfigDir || p.DataDir != DefaultDataDir || p.RunDir != DefaultRunDir {
		t.Fatalf("unset %s changed the paths: %+v", RootEnv, p)
	}
}

// Every path a test can reach hangs off one root, so a carelessly written test
// cannot read or write a real host's state.
func TestRootRedirectsEveryPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv(RootEnv, root)
	p := Locate()
	paths := []string{
		p.ConfigDir, p.DataDir, p.RunDir,
		p.ConfigFile(), p.ServicesDir(), p.SupervisionDir(), p.SpoolDir(), p.Socket(),
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Errorf("%s escapes the redirected root", path)
		}
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(context.Background(), filepath.Join(t.TempDir(), "absent.filo"))
	if err != nil {
		t.Fatalf("a host with no configuration file is valid: %v", err)
	}
	if cfg.LogRetention() != logs.DefaultRetention {
		t.Fatalf("retention = %v, want the default %v", cfg.LogRetention(), logs.DefaultRetention)
	}
	if cfg.LogMaxRows != logs.DefaultMaxRows {
		t.Fatalf("row ceiling = %d, want the default %d", cfg.LogMaxRows, logs.DefaultMaxRows)
	}
}

func TestLoadReadsConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostd.filo")
	err := os.WriteFile(path, []byte(`(host (tuple "log-retention-days" 3) (tuple "log-max-rows" 500))`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogRetention() != 72*time.Hour {
		t.Fatalf("retention = %v, want 72h", cfg.LogRetention())
	}
	if cfg.LogMaxRows != 500 {
		t.Fatalf("row ceiling = %d, want 500", cfg.LogMaxRows)
	}
}

func TestLoadRejectsBrokenConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostd.filo")
	err := os.WriteFile(path, []byte(`(host (tuple "log-retention-days"`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = Load(context.Background(), path)
	if err == nil {
		t.Fatal("a broken configuration file was accepted")
	}
	if !strings.Contains(err.Error(), "hostd.filo") {
		t.Fatalf("the error does not name the file: %v", err)
	}
}

func TestEnsureDirsIsIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv(RootEnv, root)
	p := Locate()
	for range 2 {
		err := p.EnsureDirs()
		if err != nil {
			t.Fatalf("EnsureDirs: %v", err)
		}
	}
	for _, dir := range []string{p.ConfigDir, p.ServicesDir(), p.SupervisionDir(), p.SpoolDir(), p.RunDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("%s was not created: %v", dir, err)
			continue
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %v; hostd's own directories are private", dir, info.Mode().Perm())
		}
	}
}
