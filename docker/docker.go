// Package docker speaks to the container daemon already installed on the
// machine, over its unix socket, with the standard library's HTTP client.
//
// No SDK: the calls hostd makes are a dozen, the JSON is stable, and the
// official client would bring a dependency tree larger than this daemon for
// the sake of them. Podman's socket answers the same API, so the same code
// covers it when that is what the machine has.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Pinned rather than left to the daemon's latest: an API version that changes
// under the daemon is a change nobody here asked for. Every call hostd makes
// has existed since well before this.
const apiVersion = "v1.41"

// Presence of the socket is the switch, in that order: a machine with both is
// a machine running Docker.
var socketPaths = []string{"/var/run/docker.sock", "/run/podman/podman.sock"}

// ErrNoRuntime is what a machine without a container runtime answers. It is
// not a failure of hostd: a host that runs no containers is a valid host, and
// a service declaring one has to fail with something an operator can act on.
var ErrNoRuntime = errors.New("docker: no container runtime socket on this machine")

var ErrNotFound = errors.New("docker: no such container or image")

// The ceiling on one log frame: what a service writes between two newlines
// cannot cost hostd more than this.
const maxFrameBytes = 1 << 20

type Client struct {
	http   *http.Client
	socket string
}

// Open finds the runtime's socket. The connection itself is not made here:
// what matters at start is whether the machine can run containers at all.
func Open() (*Client, error) {
	for _, path := range socketPaths {
		info, err := os.Stat(path)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		return &Client{socket: path, http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", path)
				},
			},
		}}, nil
	}
	return nil, ErrNoRuntime
}

func (c *Client) Socket() string { return c.socket }

// The daemon answers on a host name that means nothing over a unix socket, so
// any name will do; this one says where the request went.
func (c *Client) url(path string, query url.Values) string {
	out := "http://docker/" + apiVersion + path
	if len(query) > 0 {
		out += "?" + query.Encode()
	}
	return out
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, query), payload)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 400 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	// The daemon says what went wrong in its own body; passing that through is
	// more useful than a status code an operator has to look up.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, message(detail))
	}
	return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, message(detail))
}

func message(body []byte) string {
	var answer struct {
		Message string `json:"message"`
	}
	err := json.Unmarshal(body, &answer)
	if err != nil || answer.Message == "" {
		return strings.TrimSpace(string(body))
	}
	return answer.Message
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping answers whether the runtime is really there, which "the socket exists"
// does not: a stopped daemon leaves its socket behind.
func (c *Client) Ping(ctx context.Context) error {
	var version struct {
		Version string `json:"Version"`
	}
	err := c.call(ctx, http.MethodGet, "/version", nil, nil, &version)
	if err != nil {
		return fmt.Errorf("the container runtime on %s does not answer: %w", c.socket, err)
	}
	return nil
}

// What hostd declares to the runtime. Everything not named here is denied by
// omission: no host network, no host pid namespace, no devices, no added
// capabilities, no privileged mode. A container gets what the service file
// asked for and nothing else.
type Spec struct {
	Name    string
	Image   string
	Args    []string
	Env     []string
	Dir     string
	Ports   []Port
	Mounts  []Mount
	Labels  map[string]string
	Memory  int64
	NanoCPU int64
	// The network the services of this host share, and the name this one
	// answers to on it. A proxy reaches an application by its service name,
	// which is why an application needs no published port at all.
	Network string
	Alias   string
	// What the runtime should do when the container ends. Keeping it alive is
	// the runtime's job; hostd asking for the same thing at the same time is
	// how a service flaps.
	Restart string
}

// Mount is storage that outlives the container. Named storage is the runtime's
// to keep; a path from the machine is the operator's, and saying which is
// which is the whole difference.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
	Named    bool
}

func (m Mount) kind() string {
	if m.Named {
		return "volume"
	}
	return "bind"
}

type Port struct {
	// Empty binds to loopback: a port published to the world is a decision an
	// operator makes on purpose, not one they get by leaving a field out.
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
}

// Nothing is the safe reading of an empty field: a container nobody asked to
// be restarted is not restarted.
func (s Spec) restartPolicy() string {
	if s.Restart == "" {
		return "no"
	}
	return s.Restart
}

func (p Port) protocol() string {
	if p.Protocol == "" {
		return "tcp"
	}
	return p.Protocol
}

func (p Port) hostIP() string {
	if p.HostIP == "" {
		return "127.0.0.1"
	}
	return p.HostIP
}

// Create returns the container's id. Restart is hostd's job, so the runtime is
// told not to do it: two supervisors with different opinions about the same
// process is how a service ends up flapping.
func (c *Client) Create(ctx context.Context, spec Spec) (string, error) {
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for _, port := range spec.Ports {
		key := fmt.Sprintf("%d/%s", port.ContainerPort, port.protocol())
		exposed[key] = struct{}{}
		bindings[key] = append(bindings[key], map[string]string{
			"HostIp":   port.hostIP(),
			"HostPort": fmt.Sprint(port.HostPort),
		})
	}
	mounts := make([]map[string]any, 0, len(spec.Mounts))
	for _, mount := range spec.Mounts {
		mounts = append(mounts, map[string]any{
			"Type":     mount.kind(),
			"Source":   mount.Source,
			"Target":   mount.Target,
			"ReadOnly": mount.ReadOnly,
		})
	}
	body := map[string]any{
		"Image":        spec.Image,
		"Cmd":          spec.Args,
		"Env":          spec.Env,
		"Labels":       spec.Labels,
		"ExposedPorts": exposed,
		"HostConfig": map[string]any{
			"PortBindings":  bindings,
			"Mounts":        mounts,
			"RestartPolicy": map[string]any{"Name": spec.restartPolicy()},
			"Memory":        spec.Memory,
			"NanoCpus":      spec.NanoCPU,
			"Privileged":    false,
			"NetworkMode":   "bridge",
			"PidMode":       "",
			"CapAdd":        []string{},
			"SecurityOpt":   []string{"no-new-privileges"},
		},
	}
	if spec.Dir != "" {
		body["WorkingDir"] = spec.Dir
	}
	if spec.Network != "" {
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{
				spec.Network: map[string]any{"Aliases": []string{spec.Alias}},
			},
		}
	}
	var created struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {spec.Name}}, body, &created)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// UpdateRestart changes what the runtime does when a container ends, without
// touching the container. A policy that drifted from the declaration — because
// the file changed, or because a hand changed it — converges without
// interrupting the service, which is the whole point of applying a file.
func (c *Client) UpdateRestart(ctx context.Context, id, policy string) error {
	body := map[string]any{"RestartPolicy": map[string]any{"Name": policy}}
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/update", nil, body, nil)
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// Stop asks, waits, and the runtime kills what did not go. The grace is the
// service's own, so a database gets the patience it was declared with.
func (c *Client) Stop(ctx context.Context, id string, grace time.Duration) error {
	query := url.Values{"t": {fmt.Sprint(int(grace.Seconds()))}}
	err := c.call(ctx, http.MethodPost, "/containers/"+id+"/stop", query, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) Remove(ctx context.Context, id string) error {
	query := url.Values{"force": {"true"}, "v": {"false"}}
	err := c.call(ctx, http.MethodDelete, "/containers/"+id, query, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// What hostd needs to know about a container it did not start: whether it is
// the one declared, whether it is running, and what to report.
type Container struct {
	ID      string
	Name    string
	Image   string
	Digest  string
	Running bool
	PID     int
	Started time.Time
	Exit    int
	// The runtime's own word for what it is doing: running, restarting,
	// created, exited, paused or dead.
	Status string
	// How many times the runtime brought it back, and what it said the last
	// time it could not.
	Restarts int
	Error    string
	// The policy the container was created with. A container that is exited
	// under a policy that keeps things alive was stopped by a hand, because
	// the runtime would have brought anything else back.
	Restart string
	// What hostd wrote on it when it was created: which service it belongs to,
	// and which run of a job it is.
	Labels map[string]string
}

func (c *Client) Inspect(ctx context.Context, id string) (Container, error) {
	var raw struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		Image string `json:"Image"`
		State struct {
			Status     string `json:"Status"`
			Running    bool   `json:"Running"`
			Pid        int    `json:"Pid"`
			ExitCode   int    `json:"ExitCode"`
			Error      string `json:"Error"`
			StartedAt  string `json:"StartedAt"`
			FinishedAt string `json:"FinishedAt"`
		} `json:"State"`
		RestartCount int `json:"RestartCount"`
		HostConfig   struct {
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	err := c.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &raw)
	if err != nil {
		return Container{}, err
	}
	started, _ := time.Parse(time.RFC3339Nano, raw.State.StartedAt)
	return Container{
		ID:       raw.ID,
		Name:     strings.TrimPrefix(raw.Name, "/"),
		Image:    raw.Config.Image,
		Digest:   raw.Image,
		Running:  raw.State.Running,
		PID:      raw.State.Pid,
		Started:  started,
		Exit:     raw.State.ExitCode,
		Status:   raw.State.Status,
		Restarts: raw.RestartCount,
		Error:    raw.State.Error,
		Restart:  raw.HostConfig.RestartPolicy.Name,
	}, nil
}

// List answers with the containers carrying a label, running or not: a
// supervisor that only looked at what is running would leave the dead ones
// behind forever.
func (c *Client) List(ctx context.Context, label string) ([]Container, error) {
	filters := fmt.Sprintf(`{"label":[%q]}`, label)
	query := url.Values{"all": {"true"}, "filters": {filters}}
	var raw []struct {
		ID     string   `json:"Id"`
		Names  []string `json:"Names"`
		Image  string   `json:"Image"`
		ImgID  string   `json:"ImageID"`
		State  string   `json:"State"`
		Labels map[string]string
	}
	err := c.call(ctx, http.MethodGet, "/containers/json", query, nil, &raw)
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, item := range raw {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		out = append(out, Container{
			ID:      item.ID,
			Name:    name,
			Image:   item.Image,
			Digest:  item.ImgID,
			Running: item.State == "running",
			Status:  item.State,
			Labels:  item.Labels,
		})
	}
	return out, nil
}

// Wait blocks until the container ends and answers with its exit code. It is
// the container's equivalent of waiting on a child: without it, a service that
// died would only be noticed at the next tick.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	var result struct {
		StatusCode int `json:"StatusCode"`
		Error      struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, nil, &result)
	if err != nil {
		return -1, err
	}
	if result.Error.Message != "" {
		return result.StatusCode, errors.New(result.Error.Message)
	}
	return result.StatusCode, nil
}

// EnsureNetwork creates the network the host's services share, if it is not
// there. Services find each other by name on it, which is what lets a proxy
// reach an application that publishes nothing to the machine.
func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	body := map[string]any{"Name": name, "Driver": "bridge"}
	err = c.call(ctx, http.MethodPost, "/networks/create", nil, body, nil)
	// Two daemons starting at once both find it missing and both create it;
	// the second is told it exists, which is the state it wanted.
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
}

// EnsureVolume creates named storage if it is not there. It is never removed
// here: a service that goes away leaves its data behind, because deleting
// somebody's data is not something a converge loop should decide.
func (c *Client) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	err := c.call(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return c.call(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{
		"Name":   name,
		"Labels": labels,
	}, nil)
}

// ImageDigest is what the declaration is pinned to. A tag is a name that can
// be made to mean something else tomorrow; recording the digest is what makes
// "the same image" a checkable claim.
func (c *Client) ImageDigest(ctx context.Context, image string) (string, error) {
	found, err := c.Image(ctx, image)
	if err != nil {
		return "", err
	}
	return found.Digest, nil
}

// Image is what the runtime holds under a name: what it really is, and what it
// was built to run on.
type ImageInfo struct {
	Digest string
	Arch   string
}

func (c *Client) Image(ctx context.Context, image string) (ImageInfo, error) {
	var raw struct {
		ID           string `json:"Id"`
		Architecture string `json:"Architecture"`
	}
	err := c.call(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil, &raw)
	if err != nil {
		return ImageInfo{}, err
	}
	return ImageInfo{Digest: raw.ID, Arch: raw.Architecture}, nil
}

// Arch is what this machine runs. An image built for another one loads
// perfectly well and then fails to start with "exec format error", which is a
// sentence that explains nothing to whoever deployed it.
func (c *Client) Arch(ctx context.Context) (string, error) {
	var version struct {
		Arch string `json:"Arch"`
	}
	err := c.call(ctx, http.MethodGet, "/version", nil, nil, &version)
	if err != nil {
		return "", err
	}
	return version.Arch, nil
}

// Line is one thing a container wrote, with the runtime's own timestamp: a
// follower that resumes asks for what came after it.
type Line struct {
	Stream string
	Text   string
	At     time.Time
}

// Logs follows a container's output until the context ends or the container
// does. The runtime keeps the log; hostd copies it into the timeline where the
// service's own events already are, so a death and the last lines before it
// are read together.
//
// Without a TTY the runtime frames the two streams in one connection: eight
// bytes of header, the first saying which stream, the last four the size.
func (c *Client) Logs(ctx context.Context, id string, since time.Time, fn func(Line) error) error {
	query := url.Values{
		"stdout":     {"true"},
		"stderr":     {"true"},
		"follow":     {"true"},
		"timestamps": {"true"},
	}
	if !since.IsZero() {
		// Nanoseconds, so resuming does not replay the line it stopped on.
		query.Set("since", fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()))
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", query, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	header := make([]byte, 8)
	for {
		_, err = io.ReadFull(resp.Body, header)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		// A frame larger than this is not output anybody meant to write; it is
		// a header read out of step, and reading it would cost the memory the
		// ceiling exists to protect.
		if size < 0 || size > maxFrameBytes {
			return fmt.Errorf("log frame of %d bytes from container %s", size, id)
		}
		payload := make([]byte, size)
		_, err = io.ReadFull(resp.Body, payload)
		if err != nil {
			return err
		}
		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}
		for raw := range strings.SplitSeq(strings.TrimRight(string(payload), "\n"), "\n") {
			at, text := splitTimestamp(raw)
			err = fn(Line{Stream: stream, Text: text, At: at})
			if err != nil {
				return err
			}
		}
	}
}

// The runtime's own clock on each line, which is what makes resuming exact.
func splitTimestamp(raw string) (time.Time, string) {
	stamp, rest, found := strings.Cut(raw, " ")
	if !found {
		return time.Time{}, raw
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, raw
	}
	return at, rest
}

// Save streams an image out of the runtime as a tar, which is what crosses the
// wire to another machine. Nothing is written to disk here: the bytes go from
// one runtime to the other through the pipe.
func (c *Client) Save(ctx context.Context, image string, w io.Writer) error {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/get", nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(w, resp.Body)
	return err
}

// Load reads that tar into this machine's runtime. A stream that is cut short
// fails here, which is what keeps an interrupted upload from becoming an image
// somebody could declare.
func (c *Client) Load(ctx context.Context, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/images/load", url.Values{"quiet": {"1"}}), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("load the image into the runtime: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("load the image into the runtime: %s: %s", resp.Status, message(body))
	}
	// The runtime answers 200 and then says in the body that it could not read
	// the archive, so the status alone is not the answer.
	if strings.Contains(string(body), `"error"`) {
		return fmt.Errorf("load the image into the runtime: %s", message(body))
	}
	return nil
}
