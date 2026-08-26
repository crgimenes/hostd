package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const dialTimeout = 10 * time.Second

// ErrNoAnswer is a machine that ssh reached and where no hostd answered — the
// state an install resolves, which is why it is worth telling apart.
var ErrNoAnswer = errors.New("no hostd answered")

// What a client talks over: the socket on this machine, or the pipes of an ssh
// running the daemon's stdio mode on another. The protocol is the same either
// way, which is what lets the transport be somebody else's problem.
type transport interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

type Client struct {
	conn   transport
	reader *bufio.Reader
	target string
	// One request owns the stream from first byte to last: two requests
	// interleaved on one pipe corrupt the protocol for both, and the caller
	// sees a machine that stopped answering rather than its own concurrency.
	mu sync.Mutex

	// Set to divert runtime diagnostics: one key=value line per request, for
	// an agent that greps rather than reads.
	Debug io.Writer
}

func DialUnix(path string) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("cannot reach hostd on %s: %w; is it running? check with systemctl status hostd", path, err)
	}
	return newClient(conn, path), nil
}

func newClient(conn transport, target string) *Client {
	return &Client{conn: conn, reader: bufio.NewReader(conn), target: target}
}

func (c *Client) Close() error { return c.conn.Close() }

// Printed with every answer: "which machine did I just do that to?" is a
// question that costs a machine in a fleet.
func (c *Client) Target() string { return c.target }

func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	if c.Debug == nil {
		return c.do(ctx, req)
	}
	start := time.Now()
	resp, err := c.do(ctx, req)
	code := resp.Code
	if err != nil {
		code = "no-answer"
	}
	_, _ = fmt.Fprintf(c.Debug, "debug op=%s target=%s elapsed-ms=%.1f code=%s body-bytes=%d\n",
		req.Op, c.target, float64(time.Since(start).Microseconds())/1000, code, len(resp.Body))
	return resp, err
}

func (c *Client) do(ctx context.Context, req Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(requestTimeout)
	}
	// A pipe has no deadline of its own; what bounds it is the context ssh was
	// started with, and cancelling that kills the command.
	err := c.conn.SetDeadline(deadline)
	if err != nil {
		return Response{}, err
	}
	err = WriteMessage(c.conn, req)
	if err != nil {
		return Response{}, err
	}
	var resp Response
	err = ReadMessage(ctx, c.reader, &resp)
	if errors.Is(err, io.EOF) {
		// The bare "EOF" the reader returns names the mechanism and teaches
		// nothing: what the operator needs is where to look. Typed, so a
		// caller can offer the install that fixes the commonest cause.
		return Response{}, fmt.Errorf(
			"hostd on %s closed the connection without answering; it is not installed there, just restarted, or this user cannot open its socket: %w",
			c.target, ErrNoAnswer)
	}
	if err != nil {
		return Response{}, err
	}
	return resp, nil
}

// Push sends an image the caller is streaming out of its own runtime. The
// answer names the digest the other machine ended up with, which is what a
// declaration should be pinned to.
func (c *Client) Push(ctx context.Context, image, arch string, content io.Reader) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// No deadline across the whole upload: what bounds it is silence, which
	// the other end enforces per chunk, and the context the caller holds.
	err := c.conn.SetDeadline(time.Time{})
	if err != nil {
		return Response{}, err
	}
	err = WriteMessage(c.conn, Request{Op: OpImagePush, Name: image, Arch: arch})
	if err != nil {
		return Response{}, err
	}
	err = WriteChunks(c.conn, content)
	if err != nil {
		return Response{}, err
	}
	var resp Response
	err = ReadMessage(ctx, c.reader, &resp)
	if errors.Is(err, io.EOF) {
		// The bytes went and the answer did not come back. Said the same way a
		// request says it, because the cause is the same: nothing is listening
		// on the other end of this pipe any more.
		return Response{}, fmt.Errorf(
			"hostd on %s took the image and closed the connection without answering; it restarted, or this pipe was already dead: %w",
			c.target, ErrNoAnswer)
	}
	if err != nil {
		return Response{}, err
	}
	return resp, nil
}

// Consumes the connection: a client that follows issues no further requests
// on it.
func (c *Client) Follow(ctx context.Context, req Request, fn func(LogLine) error) error {
	req.Op = OpLogFollow
	err := c.conn.SetDeadline(time.Now().Add(requestTimeout))
	if err != nil {
		return err
	}
	err = WriteMessage(c.conn, req)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		// Closing is what unblocks the read: a follower waits with no
		// deadline, because waiting is the whole operation.
		_ = c.conn.Close()
	}()
	for {
		err = c.conn.SetDeadline(time.Time{})
		if err != nil {
			return err
		}
		var line LogLine
		err = ReadMessage(ctx, c.reader, &line)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		err = fn(line)
		if err != nil {
			return err
		}
	}
}
