package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"time"
)

// The group that may reach the control socket. Membership is the permission:
// an operator in it works without sudo, and the audit records who they are
// rather than "root". A machine without the group falls back to root only.
const Group = "hostd"

// The socket is where the daemon listens and ssh is how a client reaches it,
// so there is no port on the internet to defend. What sshd already does —
// authenticate a key, refuse a stranger, log the attempt — is not written
// again here.

// Peer is the identity behind a connection, taken from the kernel rather than
// from anything the caller said: a client cannot claim to be somebody else.
func Peer(conn net.Conn) string {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return ""
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return ""
	}
	uid, ok := peerUID(raw)
	if !ok {
		return ""
	}
	// The number is the fallback when the machine cannot resolve the name.
	found, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return strconv.Itoa(uid)
	}
	// Through sudo the kernel sees root; the operator's own name is the one
	// worth recording, and forging it needs root already.
	if found.Username == "root" {
		original := os.Getenv("SUDO_USER")
		if original != "" {
			return original + " (root)"
		}
	}
	return found.Username
}

// Stdio serves one client over a pipe, which is how a request arrives from
// another machine: ssh runs `hostd -stdio` there, and the two ends speak the
// same protocol they would over the socket.
//
// The daemon holding the state is the one already running, so this connects to
// its socket and copies bytes: a second process cannot supervise anything, and
// pretending otherwise would give two answers to the same question.
func Stdio(socket string, in io.Reader, out io.Writer) error {
	conn, err := net.DialTimeout("unix", socket, dialTimeout)
	if errors.Is(err, os.ErrPermission) {
		// Being in the group is not enough if this login predates being added
		// to it, which a multiplexed ssh connection makes easy to hit.
		return fmt.Errorf(
			"cannot open %s: %w; this account is not in the %s group on this machine, or this session started before it was added — open a new one (ssh -O exit <host> if you multiplex)",
			socket, err, Group)
	}
	if err != nil {
		return fmt.Errorf("cannot reach hostd on %s: %w; is it running? check with systemctl status hostd", socket, err)
	}
	defer func() { _ = conn.Close() }()

	go func() {
		_, _ = io.Copy(conn, in)
		// Half-close: the daemon sees the end of the request instead of
		// waiting on a client that has already said everything.
		unixConn, ok := conn.(*net.UnixConn)
		if ok {
			_ = unixConn.CloseWrite()
		}
	}()
	// Ends when the daemon has finished talking, not when the request has
	// finished arriving.
	_, err = io.Copy(out, conn)
	return err
}

// DialSSH reaches a daemon on another machine the way the operator already
// reaches that machine. There is no key to exchange, no port to open and no
// handshake to write: ssh authenticates, and a key restricted with a forced
// command in authorized_keys is a permission this program did not have to
// invent.
// How long ssh may spend reaching a machine before giving up. The default is
// the kernel's, which for a machine that is switched off is tens of seconds of
// a window with nothing in it; ten is long enough for a slow link and short
// enough that a person watching does not conclude the program hung.
const connectTimeout = 10

// SSHArguments builds the argument list, with the machine's name after a "--"
// that ends option parsing.
//
// Without it a name beginning with a dash IS an option: ssh reads
// "-oProxyCommand=..." as one and runs whatever it says, on this machine. The
// names do not all come from the operator's keyboard — -all and -tag read them
// out of the inventory FILE, which travels in a tree and may have been written
// by somebody else. That file is a trust boundary, and this is where it is
// crossed.
func SSHArguments(host string, remote []string) []string {
	return append([]string{
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", connectTimeout),
		"--",
		host,
	}, remote...)
}

func DialSSH(ctx context.Context, host string, remote []string) (*Client, error) {
	// #nosec G204 -- the remote command comes from the operator's own flag, and
	// the host cannot be read as an option: see SSHArguments
	cmd := exec.CommandContext(ctx, "ssh", SSHArguments(host, remote)...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// ssh's own complaints are diagnostics and belong where diagnostics go.
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("cannot run ssh: %w", err)
	}
	return newClient(&pipe{in: in, out: out, cmd: cmd}, host), nil
}

// A pair of pipes to another machine, closed by ending the process that owns
// them. There is no deadline to set: what bounds a request is the context the
// command was started with, and cancelling it kills ssh.
type pipe struct {
	in  io.WriteCloser
	out io.ReadCloser
	cmd *exec.Cmd
}

func (p *pipe) Read(b []byte) (int, error) { return p.out.Read(b) }

func (p *pipe) Write(b []byte) (int, error) { return p.in.Write(b) }

func (p *pipe) Close() error {
	_ = p.in.Close()
	_ = p.out.Close()
	// Reaped rather than left: a client that ran a command on twenty machines
	// must not leave twenty processes behind.
	_ = p.cmd.Wait()
	return nil
}

func (p *pipe) SetDeadline(time.Time) error { return nil }
