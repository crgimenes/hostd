// Package config holds where hostd keeps its things.
//
// Every path is derived from one root, and the root can be redirected. That is
// what makes a test structurally unable to reach a real host's state: the
// redirection is part of how the paths are built, not a flag someone remembers
// to pass.
package config

import (
	"context"
	"os"
	"path/filepath"

	"github.com/crgimenes/hostd/internal/filoconf"
)

// Default locations on a host.
const (
	DefaultConfigDir = "/etc/hostd"
	DefaultDataDir   = "/var/lib/hostd"
	DefaultRunDir    = "/run/hostd"
)

// RootEnv redirects every path under one directory.
//
// The name carries the project's own prefix and the value has no default: a
// process that does not set it behaves exactly as it does on a real host,
// which is the property that keeps this from becoming a mode that changes
// behaviour.
const RootEnv = "HOSTD_ROOT"

// Config is the daemon's own configuration.
type Config struct {
	// LogBuffer is how many captured records are kept in memory. It exists so
	// the ceiling is a decision rather than an accident.
	LogBuffer int `filo:"log-buffer"`
}

// DefaultLogBuffer is the ceiling used when the configuration does not set one.
const DefaultLogBuffer = 10_000

// Paths are the directories and files hostd uses.
type Paths struct {
	ConfigDir string
	DataDir   string
	RunDir    string
}

// Locate builds the paths, honouring the redirection environment variable.
func Locate() Paths {
	root := os.Getenv(RootEnv)
	if root == "" {
		return Paths{ConfigDir: DefaultConfigDir, DataDir: DefaultDataDir, RunDir: DefaultRunDir}
	}
	return Paths{
		ConfigDir: filepath.Join(root, "etc"),
		DataDir:   filepath.Join(root, "var"),
		RunDir:    filepath.Join(root, "run"),
	}
}

// ConfigFile is the daemon's configuration file.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "hostd.filo") }

// ServicesDir holds one file per service.
func (p Paths) ServicesDir() string { return filepath.Join(p.ConfigDir, "services") }

// SupervisionDir holds the identity of running processes, for adoption. It is
// under the persistent data directory on purpose: losing it would leave a
// running service with no supervisor able to prove which process it was.
func (p Paths) SupervisionDir() string { return filepath.Join(p.DataDir, "supervision") }

// SpoolDir holds what services write to stdout and stderr.
func (p Paths) SpoolDir() string { return filepath.Join(p.DataDir, "spool") }

// Socket is the local control socket.
func (p Paths) Socket() string { return filepath.Join(p.RunDir, "hostd.sock") }

// Load reads the daemon configuration. A host with no configuration file is a
// valid host: the defaults are the configuration.
func Load(ctx context.Context, path string) (Config, error) {
	cfg := Config{LogBuffer: DefaultLogBuffer}
	data, err := os.ReadFile(path) // #nosec G304 -- the path is hostd's own configuration file
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	err = filoconf.Decode(ctx, filepath.Base(path), string(data), &cfg)
	if err != nil {
		return Config{}, err
	}
	if cfg.LogBuffer <= 0 {
		cfg.LogBuffer = DefaultLogBuffer
	}
	return cfg, nil
}

// EnsureDirs creates the directories hostd owns.
func (p Paths) EnsureDirs() error {
	for _, dir := range []string{p.ConfigDir, p.ServicesDir(), p.SupervisionDir(), p.SpoolDir(), p.RunDir} {
		err := os.MkdirAll(dir, 0o700)
		if err != nil {
			return err
		}
	}
	return nil
}
