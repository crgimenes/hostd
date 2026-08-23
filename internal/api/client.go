package api

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/crgimenes/hostd/internal/filoconf"
)

// dialTimeout bounds how long a client waits to reach a daemon.
const dialTimeout = 10 * time.Second

// Client talks to one hostd.
type Client struct {
	conn   net.Conn
	reader *bufio.Reader
	target string
}

// DialUnix connects to a local daemon.
func DialUnix(path string) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("cannot reach hostd on %s: %w; is it running? check with systemctl status hostd", path, err)
	}
	return &Client{conn: conn, reader: bufio.NewReader(conn), target: path}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Target names the daemon this client is talking to. Every answer a person
// sees says which machine it came from: "where did I apply that?" is a
// question that costs a machine in a fleet.
func (c *Client) Target() string { return c.target }

// Do performs one operation.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(requestTimeout)
	}
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
	if err != nil {
		return Response{}, err
	}
	return resp, nil
}

// Call performs an operation and decodes a successful body into out.
func (c *Client) Call(ctx context.Context, req Request, out any) error {
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	if resp.Failed() {
		return resp.Err()
	}
	if out == nil || resp.Body == "" {
		return nil
	}
	return filoconf.Decode(ctx, "response", resp.Body, out)
}

// Follow streams log lines until the context is cancelled or the daemon closes
// the connection. The connection is consumed: a client that follows does not
// issue further requests on it.
func (c *Client) Follow(ctx context.Context, req Request, fn func(LogLine) error) error {
	req.Op = OpLogFollow
	err := c.conn.SetWriteDeadline(time.Now().Add(requestTimeout))
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
		err = c.conn.SetReadDeadline(time.Time{})
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
