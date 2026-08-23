package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"time"
)

const dialTimeout = 10 * time.Second

type Client struct {
	conn   net.Conn
	reader *bufio.Reader
	target string

	// Set to divert runtime diagnostics: one key=value line per request, for
	// an agent that greps rather than reads.
	Debug io.Writer
}

func DialUnix(path string) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("cannot reach hostd on %s: %w; is it running? check with systemctl status hostd", path, err)
	}
	return &Client{conn: conn, reader: bufio.NewReader(conn), target: path}, nil
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

// Consumes the connection: a client that follows issues no further requests
// on it.
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
