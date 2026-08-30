// Package config holds where hostd keeps its things.
//
// Every path derives from one redirectable root, which is what makes a test
// structurally unable to reach a real host's state.
package config

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/logs"
	"github.com/crgimenes/hostd/metrics"
)

const (
	DefaultConfigDir = "/etc/hostd"
	DefaultDataDir   = "/var/lib/hostd"
	DefaultRunDir    = "/run/hostd"
)

// RootEnv redirects every path under one directory. Unset, hostd behaves
// exactly as it does on a real host: this is a redirection, not a test mode.
const RootEnv = "HOSTD_ROOT"

// A host is Linux; anywhere else is an operator's own machine running hostd in
// loopback, where /etc and /var belong to root and a sandbox forbids them. The
// OS convention is the default there — the same layout, under the user's own
// config directory — so hostd and hostctl on one macOS agree on the socket
// with nothing exported (crg, 2026-08-30). Not "hostd": that directory is the
// operator's own tree of declarations, and the daemon's state mixed into it
// would read as files somebody wrote.
func fallbackRoot() string {
	if runtime.GOOS == "linux" {
		return ""
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "hostd-local")
}

type Config struct {
	LogRetentionDays float64 `filo:"log-retention-days"`
	// Caps the log regardless of age, so one very loud day cannot fill the
	// disk. Both ceilings are mandatory: unbounded accumulation is a bug.
	LogMaxRows           int64   `filo:"log-max-rows"`
	MetricsRetentionDays float64 `filo:"metrics-retention-days"`
	MetricsMaxRows       int64   `filo:"metrics-max-rows"`
}

func (c Config) LogRetention() time.Duration {
	return time.Duration(c.LogRetentionDays * float64(24*time.Hour))
}

func (c Config) MetricsRetention() time.Duration {
	return time.Duration(c.MetricsRetentionDays * float64(24*time.Hour))
}

type Paths struct {
	ConfigDir string
	DataDir   string
	RunDir    string
}

func Locate() Paths {
	root := os.Getenv(RootEnv)
	if root == "" {
		root = fallbackRoot()
	}
	if root == "" {
		return Paths{ConfigDir: DefaultConfigDir, DataDir: DefaultDataDir, RunDir: DefaultRunDir}
	}
	return Paths{
		ConfigDir: filepath.Join(root, "etc"),
		DataDir:   filepath.Join(root, "var"),
		RunDir:    filepath.Join(root, "run"),
	}
}

func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "hostd.filo") }

func (p Paths) ServicesDir() string { return filepath.Join(p.ConfigDir, "services") }

// What travels with a declaration lands beside it, so what a machine holds
// looks like what the operator's tree holds.
func (p Paths) ArtifactsDir(service string) string {
	return filepath.Join(p.ServicesDir(), service+".d")
}

func (p Paths) StateDir() string { return filepath.Join(p.DataDir, "state") }

func (p Paths) LogsDir() string { return filepath.Join(p.DataDir, "logs") }

func (p Paths) LogDatabase() string { return filepath.Join(p.LogsDir(), "logs.db") }

func (p Paths) MetricsDir() string { return filepath.Join(p.DataDir, "metrics") }

func (p Paths) MetricsDatabase() string { return filepath.Join(p.MetricsDir(), "metrics.db") }

func (p Paths) Socket() string { return filepath.Join(p.RunDir, "hostd.sock") }

// A missing file is not an error: the defaults are the configuration.
func Load(ctx context.Context, path string) (Config, error) {
	// Decoding starts from the defaults, so a file written by an older hostd
	// cannot zero a setting it does not carry.
	cfg := Config{
		LogRetentionDays:     logs.DefaultRetention.Hours() / 24,
		LogMaxRows:           logs.DefaultMaxRows,
		MetricsRetentionDays: metrics.DefaultRetention.Hours() / 24,
		MetricsMaxRows:       metrics.DefaultMaxRows,
	}
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
	if cfg.LogRetentionDays <= 0 {
		cfg.LogRetentionDays = logs.DefaultRetention.Hours() / 24
	}
	if cfg.LogMaxRows <= 0 {
		cfg.LogMaxRows = logs.DefaultMaxRows
	}
	if cfg.MetricsRetentionDays <= 0 {
		cfg.MetricsRetentionDays = metrics.DefaultRetention.Hours() / 24
	}
	if cfg.MetricsMaxRows <= 0 {
		cfg.MetricsMaxRows = metrics.DefaultMaxRows
	}
	return cfg, nil
}

func (p Paths) EnsureDirs() error {
	for _, dir := range []string{p.ConfigDir, p.ServicesDir(), p.StateDir(), p.LogsDir(), p.MetricsDir(), p.RunDir} {
		err := os.MkdirAll(dir, 0o700)
		if err != nil {
			return err
		}
	}
	return nil
}
