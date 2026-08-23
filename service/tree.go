package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// A service is either a file or a directory. The simple case stays one file;
// the moment a service needs something alongside it — a Caddyfile, an init
// script, a certificate — it becomes a directory and nothing else changes.
//
//	fleet/
//	├── inventory.filo      the machines
//	├── site.filo           a service that needs nothing else
//	└── caddy/              a service that does
//	    ├── init.filo       the declaration
//	    └── Caddyfile       what it needs
//
// The tree is plain files on purpose: it is meant to live in version control,
// where a change to a Caddyfile is a diff somebody can read and revert.
const (
	// The declaration inside a service directory.
	InitFile = "init.filo"
	// The fleet, not a service. It is the one name at the top of the tree that
	// does not become one.
	InventoryFile = "inventory.filo"
	// The directory a service's artifacts land in on the host, beside the
	// declaration, so what a machine holds looks like what the tree holds.
	ArtifactSuffix = ".d"
)

// Artifacts are configuration, not payload: an image is pushed as an image and
// data lives in a volume. The ceiling is what keeps somebody from moving a
// database into a declaration by accident.
const MaxArtifactBytes = 256 << 10

// Declaration is a service and whatever travels with it.
type Declaration struct {
	Service Service
	// The file as the operator wrote it. What is sent to a machine is the text
	// from the tree, never something rebuilt from the parsed value: a
	// declaration that arrives reformatted is a diff nobody made.
	Source string
	// By name, in the order they are sent, so the same tree produces the same
	// bytes every time.
	Artifacts []Artifact
}

type Artifact struct {
	Name    string
	Content []byte
}

// LoadTree reads a directory of services. A file is a service; a directory
// with an init.filo is a service with artifacts; anything else is reported
// rather than ignored, because a directory nobody reads is a change nobody
// applied.
func LoadTree(ctx context.Context, dir string) ([]Declaration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Declaration
	var problems []string
	for _, entry := range entries {
		name := entry.Name()
		if name == InventoryFile || strings.HasPrefix(name, ".") {
			continue
		}
		var declaration Declaration
		var loadErr error
		switch {
		case entry.IsDir():
			declaration, loadErr = loadServiceDir(ctx, filepath.Join(dir, name), name)
		case strings.HasSuffix(name, Extension):
			declaration, loadErr = loadServiceFile(ctx, filepath.Join(dir, name))
		default:
			continue
		}
		if loadErr != nil {
			problems = append(problems, loadErr.Error())
			continue
		}
		out = append(out, declaration)
	}
	slices.SortFunc(out, func(a, b Declaration) int {
		return strings.Compare(a.Service.Name, b.Service.Name)
	})
	if len(problems) > 0 {
		return out, fmt.Errorf("%w:\n  %s", ErrInvalid, strings.Join(problems, "\n  "))
	}
	return out, nil
}

func loadServiceFile(ctx context.Context, path string) (Declaration, error) {
	svc, err := ParseFile(ctx, path)
	if err != nil {
		return Declaration{}, err
	}
	source, err := os.ReadFile(path) // #nosec G304 -- the operator's own tree
	if err != nil {
		return Declaration{}, err
	}
	return Declaration{Service: svc, Source: string(source)}, nil
}

func loadServiceDir(ctx context.Context, dir, name string) (Declaration, error) {
	source, err := os.ReadFile(filepath.Join(dir, InitFile)) // #nosec G304 -- the operator's own tree
	if err != nil {
		if os.IsNotExist(err) {
			return Declaration{}, fmt.Errorf("%s/ has no %s, so nothing declares what it is", name, InitFile)
		}
		return Declaration{}, err
	}
	svc, err := Parse(ctx, filepath.Join(name, InitFile), string(source))
	if err != nil {
		return Declaration{}, err
	}
	if svc.Name != name {
		return Declaration{}, fmt.Errorf("%s/%s declares the name %q; the directory is the name, so call it %s or rename the directory",
			name, InitFile, svc.Name, svc.Name)
	}
	artifacts, err := readArtifacts(dir)
	if err != nil {
		return Declaration{}, fmt.Errorf("%s: %w", name, err)
	}
	return Declaration{Service: svc, Source: string(source), Artifacts: artifacts}, nil
}

// Everything beside the declaration travels with it. Subdirectories are not
// read: a service's configuration is a handful of files, and a tree inside a
// tree is a payload pretending to be configuration.
func readArtifacts(dir string) ([]Artifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Artifact
	var total int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == InitFile || strings.HasPrefix(name, ".") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- the operator's own tree
		if readErr != nil {
			return nil, readErr
		}
		total += len(content)
		if total > MaxArtifactBytes {
			return nil, fmt.Errorf("the files beside the declaration pass %d bytes; artifacts are configuration, and anything larger belongs in an image or a volume", MaxArtifactBytes)
		}
		out = append(out, Artifact{Name: name, Content: content})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// hashArtifacts reduces the files beside a declaration to one value, so a plan
// can tell that a configuration changed without reading it. Names go into the
// hash with the content: renaming a file is a change too.
func hashArtifacts(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		content, readErr := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- hostd's own services directory
		if readErr != nil {
			return "", readErr
		}
		_, _ = fmt.Fprintf(sum, "%s\n%d\n", name, len(content))
		sum.Write(content)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
