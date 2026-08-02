//go:build windows

package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/logx"
)

const (
	dialTimeout       = 15 * time.Second
	keepaliveInterval = 30 * time.Second
)

// Credential is how to authenticate to a guest.
type Credential struct {
	Username   string
	Password   string
	PrivateKey string // PEM
	Passphrase string
	Port       int
}

// Pool keeps one SSH connection per VM and hands it out to everything that needs
// the guest: command execution and every tunnel forwarding through it.
//
// Sharing matters because each connection costs a full handshake, and a tunnel
// opens a channel per TCP connection rather than a new session.
type Pool struct {
	mu      sync.Mutex
	clients map[string]*pooled
	hosts   *HostKeys
	log     *logx.Logger
}

type pooled struct {
	client *ssh.Client
	stop   chan struct{}
}

func NewPool(hosts *HostKeys, log *logx.Logger) *Pool {
	return &Pool{clients: map[string]*pooled{}, hosts: hosts, log: log}
}

// Get returns a live connection to a VM, dialling one if needed.
//
// addr is host:port. The caller resolves the guest address, because only it
// knows whether to wait for the guest to finish booting first.
func (p *Pool) Get(ctx context.Context, vmName, addr string, cred Credential, trustNewKey bool) (*ssh.Client, *Verdict, error) {
	p.mu.Lock()
	if existing, ok := p.clients[vmName]; ok {
		client := existing.client
		p.mu.Unlock()
		if alive(client) {
			return client, nil, nil
		}
		// Dead connection: drop it and fall through to a fresh dial.
		p.Drop(vmName)
		p.mu.Lock()
	}
	p.mu.Unlock()

	client, verdict, err := p.dial(ctx, vmName, addr, cred, trustNewKey)
	if err != nil {
		return nil, nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Another goroutine may have dialled while we were; keep theirs and close ours.
	if existing, ok := p.clients[vmName]; ok && alive(existing.client) {
		client.Close()
		return existing.client, verdict, nil
	}
	entry := &pooled{client: client, stop: make(chan struct{})}
	p.clients[vmName] = entry
	go p.keepalive(vmName, entry)
	return client, verdict, nil
}

func (p *Pool) dial(ctx context.Context, vmName, addr string, cred Credential, trustNewKey bool) (*ssh.Client, *Verdict, error) {
	auth, err := authMethods(cred)
	if err != nil {
		return nil, nil, err
	}

	var verdict *Verdict
	cfg := &ssh.ClientConfig{
		User: cred.Username,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			v, err := p.hosts.Check(vmName, key, trustNewKey)
			if err != nil {
				return err
			}
			verdict = v
			return nil
		},
		Timeout: dialTimeout,
	}

	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, hverr.Wrap(hverr.SSHUnreachable, err,
			"could not reach %s over TCP", addr).
			WithDetail("Check that the VM has finished booting, that sshd is running, and that the guest firewall allows the port.")
	}

	// The handshake has no context of its own, so bound it with a deadline.
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, nil, classifyHandshake(err, vmName, addr)
	}
	_ = conn.SetDeadline(time.Time{})

	client := ssh.NewClient(sc, chans, reqs)
	if verdict != nil && verdict.FirstSeen {
		p.log.Infof("pinned SSH host key for %q: %s", vmName, verdict.Fingerprint)
	}
	return client, verdict, nil
}

// authMethods builds the auth list. A key is tried before a password so that a
// guest configured for key-only auth does not burn an attempt on the password.
func authMethods(cred Credential) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if cred.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if cred.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(cred.PrivateKey), []byte(cred.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(cred.PrivateKey))
		}
		if err != nil {
			return nil, hverr.Wrap(hverr.InvalidArgument, err, "could not parse the SSH private key")
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cred.Password != "" {
		methods = append(methods, ssh.Password(cred.Password))
	}
	if len(methods) == 0 {
		return nil, hverr.New(hverr.InvalidArgument,
			"no SSH credentials: supply a password or a private key, or store one with `hypervm-mcp cred set`")
	}
	return methods, nil
}

// classifyHandshake separates the two failures worth telling apart: a rejected
// identity and a changed host key.
func classifyHandshake(err error, vmName, addr string) error {
	msg := err.Error()
	if strings.Contains(msg, "host key") {
		return hverr.Wrap(hverr.SSHHostKeyMismatch, err, "%s", msg)
	}
	if strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "no supported methods") {
		return hverr.Wrap(hverr.SSHAuthFailed, err,
			"%s rejected the credentials for %q", addr, vmName).
			WithDetail("Check the username, and that the public key is in the guest's authorized_keys.")
	}
	return hverr.Wrap(hverr.SSHUnreachable, err, "SSH handshake with %s failed", addr)
}

// alive reports whether a pooled connection still answers.
func alive(c *ssh.Client) bool {
	_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// keepalive both detects a dead connection and stops the guest's sshd from
// timing an idle tunnel out.
func (p *Pool) keepalive(vmName string, entry *pooled) {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-entry.stop:
			return
		case <-ticker.C:
			if !alive(entry.client) {
				p.log.Warnf("SSH connection to %q died; it will be redialled on demand", vmName)
				p.Drop(vmName)
				return
			}
		}
	}
}

// Drop closes and forgets the connection for a VM.
func (p *Pool) Drop(vmName string) {
	p.mu.Lock()
	entry, ok := p.clients[vmName]
	delete(p.clients, vmName)
	p.mu.Unlock()

	if ok {
		close(entry.stop)
		entry.client.Close()
	}
}

// Close tears down every pooled connection.
func (p *Pool) Close() {
	p.mu.Lock()
	entries := p.clients
	p.clients = map[string]*pooled{}
	p.mu.Unlock()

	for _, e := range entries {
		close(e.stop)
		e.client.Close()
	}
}

// ExecResult is the outcome of running one command in a guest.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Exec runs a command over an existing connection.
//
// A non-zero exit is returned in ExitCode rather than as an error: the command
// ran, and its exit status is a result the caller wants to see. Only a failure
// to run the command at all is an error.
func Exec(ctx context.Context, client *ssh.Client, command, stdin string, timeout time.Duration) (*ExecResult, error) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session, err := client.NewSession()
	if err != nil {
		return nil, hverr.Wrap(hverr.SSHUnreachable, err, "could not open an SSH session")
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if stdin != "" {
		session.Stdin = strings.NewReader(stdin)
	}

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case <-runCtx.Done():
		// Closing the session is what actually interrupts a hung command;
		// abandoning the goroutine would leave it running in the guest.
		session.Signal(ssh.SIGKILL)
		session.Close()
		return nil, hverr.New(hverr.OperationTimeout,
			"the command did not finish within %s", timeout).
			WithDetail(truncate(stdout.String()+stderr.String(), 2048))

	case err := <-done:
		res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
		if err == nil {
			return res, nil
		}
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitStatus()
			return res, nil
		}
		return nil, hverr.Wrap(hverr.SSHUnreachable, err, "the SSH session failed")
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}

// DialGuest opens a TCP connection from inside the guest.
//
// This is what makes a service bound to the guest's own 127.0.0.1 reachable: the
// guest's sshd makes the connection locally on our behalf, so loopback means the
// guest's loopback rather than ours.
func DialGuest(client *ssh.Client, address string) (net.Conn, error) {
	conn, err := client.Dial("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("guest could not connect to %s: %w", address, err)
	}
	return conn, nil
}
