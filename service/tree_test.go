package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		err = os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// The simple case is one file; the moment a service needs something beside it,
// it becomes a directory and nothing else about it changes.
func TestATreeHoldsBothShapes(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"site.filo":       `(service (tuple "name" "site") (tuple "image" "site:1"))`,
		"caddy/init.filo": `(service (tuple "name" "caddy") (tuple "image" "caddy:2") (tuple "config" "/etc/caddy"))`,
		"caddy/Caddyfile": ":80 {\n\treverse_proxy site:80\n}\n",
		"inventory.filo":  `(inventory (host (tuple "name" "yuki.local")))`,
		".hidden":         "ignored",
		"notes.txt":       "ignored",
	})

	declared, err := LoadTree(context.Background(), dir)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(declared) != 2 {
		t.Fatalf("got %d services: %#v", len(declared), declared)
	}
	// Sorted, so the same tree produces the same plan every time.
	if declared[0].Service.Name != "caddy" || declared[1].Service.Name != "site" {
		t.Fatalf("not sorted: %q %q", declared[0].Service.Name, declared[1].Service.Name)
	}
	if len(declared[0].Artifacts) != 1 || declared[0].Artifacts[0].Name != "Caddyfile" {
		t.Fatalf("the files beside the declaration did not travel: %#v", declared[0].Artifacts)
	}
	if len(declared[1].Artifacts) != 0 {
		t.Fatal("a service that is one file carries nothing")
	}
}

// The fleet is not a service, and it is the one name at the top that does not
// become one.
func TestTheInventoryIsNotAService(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"inventory.filo": `(inventory (host (tuple "name" "yuki.local")))`,
	})
	declared, err := LoadTree(context.Background(), dir)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(declared) != 0 {
		t.Fatalf("the inventory was read as a service: %#v", declared)
	}
}

// A directory nobody reads is a change nobody applied, so it is reported.
func TestADirectoryWithNoDeclarationIsReported(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"caddy/Caddyfile": ":80 {}",
		"site.filo":       `(service (tuple "name" "site") (tuple "image" "site:1"))`,
	})
	declared, err := LoadTree(context.Background(), dir)
	if err == nil {
		t.Fatal("a directory with no declaration was ignored")
	}
	if !strings.Contains(err.Error(), InitFile) {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
	if len(declared) != 1 {
		t.Fatalf("the services that are fine should still load: %#v", declared)
	}
}

// The directory is the name: a declaration that says otherwise is a file
// somebody moved without reading it.
func TestTheDirectoryIsTheName(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"caddy/init.filo": `(service (tuple "name" "proxy") (tuple "image" "caddy:2"))`,
	})
	_, err := LoadTree(context.Background(), dir)
	if err == nil {
		t.Fatal("a declaration naming something else was accepted")
	}
}

// Artifacts are configuration; a payload pretending to be configuration is
// refused where it is written.
func TestArtifactsHaveACeiling(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"big/init.filo": `(service (tuple "name" "big") (tuple "image" "big:1") (tuple "config" "/etc/big"))`,
		"big/blob":      strings.Repeat("x", MaxArtifactBytes+1),
	})
	_, err := LoadTree(context.Background(), dir)
	if err == nil {
		t.Fatal("a megabyte of payload was accepted as configuration")
	}
	if !strings.Contains(err.Error(), "volume") {
		t.Fatalf("the error does not say where it belongs: %v", err)
	}
}
