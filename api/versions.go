package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crgimenes/hostd/docker"
	"github.com/crgimenes/hostd/service"
)

// A version of the image one service can run, and the reference that pins it.
//
// Ref is never the moving tag the declaration usually carries. That tag is what
// the next push takes away, so writing it down again is precisely what does not
// roll anything back — the whole reason going back needed a command at all.
type ServiceVersion struct {
	Digest  string   `filo:"digest" json:"digest"`
	Ref     string   `filo:"ref" json:"ref"`
	Tags    []string `filo:"tags" json:"tags"`
	Bytes   float64  `filo:"bytes" json:"bytes"`
	Created float64  `filo:"created-ms" json:"created-ms"`
	// What a container of this service is on right now, and what the
	// declaration resolves to today. Usually the same image, always different
	// questions: a declaration edited and not applied makes them disagree, and
	// that disagreement is the thing worth seeing before going anywhere.
	Running  bool `filo:"running" json:"running"`
	Declared bool `filo:"declared" json:"declared"`
}

type ServiceVersions struct {
	Service string `filo:"service" json:"service"`
	// The image reference the declaration carries, as written.
	Image string `filo:"image" json:"image"`
	// Empty when the declaration pins a digest and nothing on the machine says
	// which line of versions that digest belongs to.
	Repository string           `filo:"repository" json:"repository"`
	Versions   []ServiceVersion `filo:"versions" json:"versions"`
}

// serviceVersions answers what this service could be put back on. The question
// only has an answer on the machine holding the images, and the grouping is the
// stamp's: every version pushed under one repository is one line, however far
// the moving tag has since travelled.
func (s *Server) serviceVersions(ctx context.Context, req Request) Response {
	if req.Name == "" {
		return Response{Code: CodeInvalid, Message: "which service's versions?"}
	}
	if s.runtime == nil {
		return Response{Code: CodeUnavailable, Message: "this machine has no container runtime, so it holds no version to go back to"}
	}
	declared, err := service.LoadDir(ctx, s.services)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	at := slices.IndexFunc(declared, func(def service.Service) bool { return def.Name == req.Name })
	if at < 0 {
		return Response{Code: CodeNotFound, Message: fmt.Sprintf("no service named %q is declared here", req.Name)}
	}
	held, err := s.runtime.Images(ctx)
	if err != nil {
		return Response{Code: CodeFailed, Message: err.Error()}
	}
	// A service between runs, or one nothing runs, has no digest and that is an
	// answer: no row is marked running, rather than a row marked wrongly.
	running := ""
	status, err := s.sup.StatusOf(req.Name)
	if err == nil {
		running = status.Digest
	}
	return body(versionsOf(declared[at], held, running))
}

// versionsOf is pure so the rule is one thing to read and one thing to test:
// which images belong to this service's line, which of them the declaration
// names, and what to write down to pin each one.
func versionsOf(svc service.Service, held []docker.ImageSummary, running string) ServiceVersions {
	answer := ServiceVersions{
		Service:    svc.Name,
		Image:      svc.Image,
		Repository: repositoryFor(svc.Image, held),
	}
	for _, image := range held {
		isRunning := running != "" && image.Digest == running
		isDeclared := namesImage(svc.Image, image)
		// A running image outside the line is not hidden. It means the
		// declaration changed after the container started, which is exactly
		// what somebody about to move versions needs to know.
		if !inRepository(image, answer.Repository) && !isRunning && !isDeclared {
			continue
		}
		answer.Versions = append(answer.Versions, ServiceVersion{
			Digest:   image.Digest,
			Ref:      refFor(image),
			Tags:     image.Tags,
			Bytes:    float64(image.Bytes),
			Created:  float64(image.Created.UnixMilli()),
			Running:  isRunning,
			Declared: isDeclared,
		})
	}
	slices.SortFunc(answer.Versions, func(a, b ServiceVersion) int {
		if a.Created != b.Created {
			return cmp.Compare(b.Created, a.Created)
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	return answer
}

// Which line of versions the declaration is on. Read from the stamp of the
// image it resolves to rather than from the reference itself, because a
// declaration already rolled back names a stamp, and one pinned to a digest
// names no repository at all — the digest is the whole name.
func repositoryFor(declared string, held []docker.ImageSummary) string {
	for _, image := range held {
		if !namesImage(declared, image) {
			continue
		}
		repo, marked := markedRepository(image.Tags)
		if marked {
			return repo
		}
		if len(image.Tags) > 0 {
			return RepositoryOf(image.Tags[0])
		}
		return ""
	}
	if isDigest(declared) {
		return ""
	}
	return RepositoryOf(declared)
}

func inRepository(image docker.ImageSummary, repository string) bool {
	if repository == "" {
		return false
	}
	for _, tag := range image.Tags {
		if RepositoryOf(tag) == repository {
			return true
		}
	}
	return false
}

// A declaration names an image by tag or by digest, and a digest may be written
// short: twelve characters is what the runtime prints and what a hand copies.
func namesImage(declared string, image docker.ImageSummary) bool {
	if declared == "" {
		return false
	}
	if slices.Contains(image.Tags, declared) {
		return true
	}
	if !isDigest(declared) {
		return false
	}
	return strings.HasPrefix(bareDigest(image.Digest), bareDigest(declared))
}

// What to write in the tree to pin this version: the stamp when there is one,
// because it is the name a later push cannot take away; the digest otherwise,
// because it is immutable even though it means nothing on another machine.
func refFor(image docker.ImageSummary) string {
	tag, marked := managedTag(image.Tags)
	if marked {
		return tag
	}
	return image.Digest
}

const digestAlgorithm = "sha256:"

// Long enough not to swallow a tag, short enough to be what an operator pasted
// out of a listing.
const shortDigestLength = 12

func isDigest(reference string) bool {
	if strings.HasPrefix(reference, digestAlgorithm) {
		return true
	}
	if len(reference) < shortDigestLength {
		return false
	}
	return strings.IndexFunc(reference, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) < 0
}

func bareDigest(reference string) string {
	return strings.TrimPrefix(reference, digestAlgorithm)
}
