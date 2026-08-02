//go:build windows

// Package tunnel forwards host TCP ports into virtual machines.
//
// Tunnels live in the service process rather than in an MCP session, for three
// reasons: "open a tunnel" means until it is closed, not until this conversation
// ends; a host port is a machine-wide resource that needs one owner; and being
// process-wide state is what lets them be written down and restored after a
// restart.
package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/logx"
	"github.com/heavycaffeiner/hypervm-mcp/internal/sshx"
)

const (
	backendDialTimeout = 10 * time.Second
	currentVersion     = 1
)

// systemPorts are ports Windows itself listens on, so a tunnel can never bind
// them. They are named here so the failure can say what to do instead.
var systemPorts = map[int]string{
	445:  "SMB, held by the host's LanmanServer",
	139:  "NetBIOS session service",
	137:  "NetBIOS name service",
	135:  "RPC endpoint mapper",
	3389: "Remote Desktop",
	5985: "WinRM over HTTP",
	5986: "WinRM over HTTPS",
}

// Firewall creates and removes the inbound rules a non-loopback tunnel needs.
type Firewall interface {
	Allow(ctx context.Context, ruleName string, port int, addresses []string) error
	Remove(ctx context.Context, ruleName string) error
}

// Deps are the collaborators the manager needs from the rest of the service.
type Deps struct {
	// ResolveGuestIP returns the address a VM currently answers on.
	ResolveGuestIP func(ctx context.Context, vmName string) (string, error)
	// SSHClient returns a live SSH connection to a VM. host, when non-empty,
	// overrides the address Hyper-V reports.
	SSHClient func(ctx context.Context, vmName, host string) (*ssh.Client, error)
	// TailnetAddrs returns the host's Tailscale addresses.
	TailnetAddrs func(ctx context.Context) ([]string, error)
	Firewall     Firewall
	Log          *logx.Logger
}

// Spec is the part of a tunnel worth writing down. Runtime state is deliberately
// excluded: a guest IP recorded now is wrong after the next reboot, so it is
// re-resolved on restore rather than restored.
type Spec struct {
	ID        string `json:"id"`
	VMName    string `json:"vm_name"`
	Mode      string `json:"mode"`       // direct | ssh
	BindScope string `json:"bind_scope"` // loopback | tailnet | all | an IP literal
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	// GuestHost pins the address to reach the VM at, instead of asking Hyper-V.
	// Hyper-V only knows what a guest agent tells it, and minimal Linux installs
	// ship without one, so this keeps tunnels usable on such guests.
	GuestHost   string `json:"guest_host,omitempty"`
	AutoRestore bool   `json:"auto_restore"`
	Label       string `json:"label,omitempty"`
	Created     string `json:"created"`
}

// Tunnel is the reported view of a running tunnel.
type Tunnel struct {
	Spec
	ListenAddrs  []string `json:"listen_addrs"`
	URLs         []string `json:"urls"`
	GuestAddress string   `json:"guest_address,omitempty"`
	ActiveConns  int64    `json:"active_conns"`
	TotalConns   int64    `json:"total_conns"`
	BytesUp      int64    `json:"bytes_up"`
	BytesDown    int64    `json:"bytes_down"`
	LastError    string   `json:"last_error,omitempty"`
	FirewallRule string   `json:"firewall_rule,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

type state struct {
	spec      Spec
	listeners []net.Listener
	addrs     []string
	fwRule    string
	warnings  []string

	connMu sync.Mutex
	conns  map[net.Conn]struct{}

	active    atomic.Int64
	total     atomic.Int64
	bytesUp   atomic.Int64
	bytesDown atomic.Int64
	lastErr   atomic.Value // string
	cachedIP  atomic.Value // string
}

// Manager owns every tunnel in the process.
type Manager struct {
	mu     sync.RWMutex
	items  map[string]*state
	nextID int
	deps   Deps
	path   string
}

func NewManager(deps Deps) *Manager {
	return &Manager{
		items:  map[string]*state{},
		nextID: 1,
		deps:   deps,
		path:   filepath.Join(config.DataDir(), "tunnels.json"),
	}
}

// Open starts a tunnel.
func (m *Manager) Open(ctx context.Context, spec Spec) (*Tunnel, error) {
	if spec.VMName == "" {
		return nil, hverr.New(hverr.InvalidArgument, "vm_name is required")
	}
	if spec.GuestPort < 1 || spec.GuestPort > 65535 {
		return nil, hverr.New(hverr.InvalidArgument, "guest_port must be between 1 and 65535")
	}
	if spec.HostPort < 0 || spec.HostPort > 65535 {
		return nil, hverr.New(hverr.InvalidArgument, "host_port must be between 0 and 65535")
	}
	switch spec.Mode {
	case "", "direct":
		spec.Mode = "direct"
	case "ssh":
	default:
		return nil, hverr.New(hverr.InvalidArgument, `mode must be "direct" or "ssh", got %q`, spec.Mode)
	}
	if spec.BindScope == "" {
		spec.BindScope = "loopback"
	}

	hosts, err := m.resolveBind(ctx, spec.BindScope)
	if err != nil {
		return nil, err
	}

	// Fail early on an unreachable guest rather than at the first connection,
	// where the cause would be far from the call that caused it.
	if spec.Mode == "ssh" {
		if _, err := m.deps.SSHClient(ctx, spec.VMName, spec.GuestHost); err != nil {
			return nil, err
		}
	}

	st := &state{spec: spec, conns: map[net.Conn]struct{}{}}
	listeners, addrs, port, err := listen(hosts, spec.HostPort)
	if err != nil {
		return nil, err
	}
	st.spec.HostPort = port
	st.listeners = listeners
	st.addrs = addrs

	m.mu.Lock()
	st.spec.ID = fmt.Sprintf("tnl-%d", m.nextID)
	m.nextID++
	st.spec.Created = time.Now().UTC().Format(time.RFC3339)
	m.items[st.spec.ID] = st
	m.mu.Unlock()

	if spec.BindScope != "loopback" {
		// Prefixed with the instance name so a development build never removes a
		// rule the installed service owns.
		rule := config.ResourcePrefix() + "-" + st.spec.ID
		if err := m.deps.Firewall.Allow(ctx, rule, port, firewallAddresses(spec.BindScope, hosts)); err != nil {
			// A missing rule may still work — the host firewall might already
			// permit the port — so this is reported, not fatal.
			st.warnings = append(st.warnings, "firewall rule not created: "+err.Error())
			m.deps.Log.Warnf("tunnel %s: %v", st.spec.ID, err)
		} else {
			st.fwRule = rule
		}
	}

	for _, ln := range listeners {
		go m.serve(context.Background(), st, ln)
	}

	m.persist()
	m.deps.Log.Infof("tunnel %s: %s -> %s:%d (%s)", st.spec.ID, addrs, spec.VMName, spec.GuestPort, spec.Mode)
	return m.view(st), nil
}

// List returns every tunnel, optionally filtered to one VM.
func (m *Manager) List(vmName string) []Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Tunnel, 0, len(m.items))
	for _, st := range m.items {
		if vmName != "" && st.spec.VMName != vmName {
			continue
		}
		out = append(out, *m.view(st))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close stops a tunnel and drops its connections immediately.
//
// In-flight connections are cut rather than drained: closing a tunnel is
// expected to take effect now, and a long-lived connection such as a WebSocket
// or a database session would make draining an unbounded wait.
func (m *Manager) Close(ctx context.Context, id string) error {
	m.mu.Lock()
	st, ok := m.items[id]
	delete(m.items, id)
	m.mu.Unlock()

	if !ok {
		return hverr.New(hverr.TunnelNotFound, "no tunnel with id %q", id)
	}
	m.shutdown(ctx, st)
	m.persist()
	m.deps.Log.Infof("tunnel %s closed", id)
	return nil
}

// CloseAll stops every tunnel. The service calls this on shutdown so that no
// firewall rule outlives the listener it was opened for.
func (m *Manager) CloseAll(ctx context.Context) {
	m.mu.Lock()
	items := m.items
	m.items = map[string]*state{}
	m.mu.Unlock()

	for _, st := range items {
		m.shutdown(ctx, st)
	}
}

// CloseForVM stops every tunnel pointing at a VM, and reports which. Used when a
// VM is deleted so nothing is left listening for it.
func (m *Manager) CloseForVM(ctx context.Context, vmName string) []string {
	var closed []string
	for _, t := range m.List(vmName) {
		if err := m.Close(ctx, t.ID); err == nil {
			closed = append(closed, t.ID)
		}
	}
	return closed
}

func (m *Manager) shutdown(ctx context.Context, st *state) {
	for _, ln := range st.listeners {
		ln.Close()
	}
	st.connMu.Lock()
	for c := range st.conns {
		c.Close()
	}
	st.conns = map[net.Conn]struct{}{}
	st.connMu.Unlock()

	if st.fwRule != "" {
		if err := m.deps.Firewall.Remove(ctx, st.fwRule); err != nil {
			m.deps.Log.Warnf("could not remove firewall rule %s: %v", st.fwRule, err)
		}
	}
}

// Restore reopens the tunnels recorded before the last shutdown.
//
// Failures are logged and skipped: a VM that no longer exists must not stop the
// service from starting.
func (m *Manager) Restore(ctx context.Context) {
	specs, next, err := m.loadSpecs()
	if err != nil {
		m.deps.Log.Warnf("could not read saved tunnels: %v", err)
		return
	}
	m.mu.Lock()
	if next > m.nextID {
		m.nextID = next
	}
	m.mu.Unlock()

	for _, spec := range specs {
		if !spec.AutoRestore {
			continue
		}
		if _, err := m.Open(ctx, spec); err != nil {
			m.deps.Log.Warnf("could not restore tunnel for %s:%d: %v", spec.VMName, spec.GuestPort, err)
		}
	}
}

// ---- data path -------------------------------------------------------------

func (m *Manager) serve(ctx context.Context, st *state, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // the listener was closed
		}
		go m.handle(ctx, st, conn)
	}
}

func (m *Manager) handle(ctx context.Context, st *state, client net.Conn) {
	st.track(client)
	defer func() { st.untrack(client); client.Close() }()

	backend, err := m.dialBackend(ctx, st)
	if err != nil {
		st.lastErr.Store(err.Error())
		m.deps.Log.Warnf("tunnel %s: %v", st.spec.ID, err)
		return
	}
	st.track(backend)
	defer func() { st.untrack(backend); backend.Close() }()

	st.active.Add(1)
	st.total.Add(1)
	defer st.active.Add(-1)

	// Counting as the bytes move, rather than when the copy returns, is what
	// makes the numbers useful: a keep-alive or WebSocket connection can stay
	// open for hours, and totals that only land at close would read as zero
	// throughout.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(counting{backend, &st.bytesUp}, client)
		halfClose(backend)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(counting{client, &st.bytesDown}, backend)
		halfClose(client)
		done <- struct{}{}
	}()

	<-done
	// One side is finished; closing both unblocks the other copy. A forwarded
	// connection has no reason to stay half-open once its peer is done.
	client.Close()
	backend.Close()
	<-done
}

func (m *Manager) dialBackend(ctx context.Context, st *state) (net.Conn, error) {
	port := strconv.Itoa(st.spec.GuestPort)

	if st.spec.Mode == "ssh" {
		client, err := m.deps.SSHClient(ctx, st.spec.VMName, st.spec.GuestHost)
		if err != nil {
			return nil, err
		}
		// The guest makes this connection, so 127.0.0.1 is the guest's loopback.
		// That is the whole reason ssh mode exists.
		return sshx.DialGuest(client, net.JoinHostPort("127.0.0.1", port))
	}

	// A pinned address is used as given, without asking Hyper-V.
	if st.spec.GuestHost != "" {
		return net.DialTimeout("tcp", net.JoinHostPort(st.spec.GuestHost, port), backendDialTimeout)
	}

	// Try the address we used last time first. Re-resolving on every connection
	// would spawn a PowerShell process per connection; never re-resolving would
	// keep dialling a dead address after the VM reboots. Retrying once on
	// failure sits between the two.
	if cached, _ := st.cachedIP.Load().(string); cached != "" {
		if conn, err := net.DialTimeout("tcp", net.JoinHostPort(cached, port), backendDialTimeout); err == nil {
			return conn, nil
		}
	}

	ip, err := m.deps.ResolveGuestIP(ctx, st.spec.VMName)
	if err != nil {
		return nil, err
	}
	st.cachedIP.Store(ip)

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, port), backendDialTimeout)
	if err != nil {
		return nil, hverr.Wrap(hverr.SSHUnreachable, err,
			"could not reach %s:%s", ip, port).
			WithDetail("The guest service may be bound to the guest's own 127.0.0.1, " +
				"which no direct route can reach. Use mode \"ssh\" for that.")
	}
	return conn, nil
}

// counting tallies bytes as they are written.
type counting struct {
	w io.Writer
	n *atomic.Int64
}

func (c counting) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// halfClose signals end-of-stream in one direction where the transport supports
// it, so a peer waiting on EOF is not left hanging.
func halfClose(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func (st *state) track(c net.Conn) {
	st.connMu.Lock()
	st.conns[c] = struct{}{}
	st.connMu.Unlock()
}

func (st *state) untrack(c net.Conn) {
	st.connMu.Lock()
	delete(st.conns, c)
	st.connMu.Unlock()
}

// ---- binding ---------------------------------------------------------------

func (m *Manager) resolveBind(ctx context.Context, scope string) ([]string, error) {
	switch scope {
	case "loopback":
		return []string{"127.0.0.1"}, nil
	case "all":
		return []string{""}, nil // an empty host binds every interface
	case "tailnet":
		addrs, err := m.deps.TailnetAddrs(ctx)
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, hverr.New(hverr.TailscaleNotRunning,
				"Tailscale reported no addresses for this host")
		}
		return addrs, nil
	default:
		if _, err := netip.ParseAddr(scope); err != nil {
			return nil, hverr.New(hverr.InvalidArgument,
				`bind_scope must be "loopback", "tailnet", "all" or an IP address, got %q`, scope)
		}
		return []string{scope}, nil
	}
}

// listen binds every address, rolling back on failure so a partly-bound tunnel
// never exists.
func listen(hosts []string, port int) ([]net.Listener, []string, int, error) {
	var listeners []net.Listener
	var addrs []string

	rollback := func() {
		for _, ln := range listeners {
			ln.Close()
		}
	}

	for _, host := range hosts {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			rollback()
			return nil, nil, 0, portError(port, host, err)
		}
		// With port 0 the OS picks; every later address must use the same one.
		if port == 0 {
			port = ln.Addr().(*net.TCPAddr).Port
		}
		listeners = append(listeners, ln)
		addrs = append(addrs, ln.Addr().String())
	}
	return listeners, addrs, port, nil
}

func portError(port int, host string, err error) error {
	e := hverr.Wrap(hverr.PortInUse, err, "could not listen on %s port %d", displayHost(host), port)
	if what, ok := systemPorts[port]; ok {
		// Suggesting another port would be useless here: a client that cannot be
		// told a port number, such as SMB, needs the guest to have its own address.
		return e.WithDetail(fmt.Sprintf(
			"Port %d is %s and cannot be tunnelled. Clients of these protocols cannot "+
				"be pointed at a different port either. Put the VM on an External switch "+
				"so it gets its own address on the physical LAN.", port, what))
	}
	return e.WithDetail("Choose another host_port, or pass 0 to let the OS pick a free one.")
}

func displayHost(host string) string {
	if host == "" {
		return "every interface"
	}
	return host
}

// firewallAddresses scopes the rule as tightly as the bind scope allows, so a
// tunnel meant for the tailnet is not also reachable from a coffee shop network.
func firewallAddresses(scope string, hosts []string) []string {
	if scope == "all" {
		return nil
	}
	return hosts
}

// ---- reporting and persistence ---------------------------------------------

func (m *Manager) view(st *state) *Tunnel {
	t := &Tunnel{
		Spec:         st.spec,
		ListenAddrs:  st.addrs,
		ActiveConns:  st.active.Load(),
		TotalConns:   st.total.Load(),
		BytesUp:      st.bytesUp.Load(),
		BytesDown:    st.bytesDown.Load(),
		FirewallRule: st.fwRule,
		Warnings:     st.warnings,
	}
	if s, ok := st.cachedIP.Load().(string); ok {
		t.GuestAddress = s
	}
	if s, ok := st.lastErr.Load().(string); ok {
		t.LastError = s
	}
	for _, a := range st.addrs {
		t.URLs = append(t.URLs, "http://"+a)
	}
	return t
}

type tunnelFile struct {
	Version int    `json:"version"`
	NextID  int    `json:"next_id"`
	Tunnels []Spec `json:"tunnels"`
}

func (m *Manager) persist() {
	m.mu.RLock()
	f := tunnelFile{Version: currentVersion, NextID: m.nextID}
	for _, st := range m.items {
		if st.spec.AutoRestore {
			f.Tunnels = append(f.Tunnels, st.spec)
		}
	}
	m.mu.RUnlock()

	sort.Slice(f.Tunnels, func(i, j int) bool { return f.Tunnels[i].ID < f.Tunnels[j].ID })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		m.deps.Log.Warnf("could not encode tunnels: %v", err)
		return
	}
	if err := config.WriteFileAtomic(m.path, append(b, '\n'), 0o600); err != nil {
		m.deps.Log.Warnf("could not save tunnels: %v", err)
	}
}

func (m *Manager) loadSpecs() ([]Spec, int, error) {
	b, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return nil, 1, nil
	}
	if err != nil {
		return nil, 1, err
	}
	var f tunnelFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, 1, err
	}
	return f.Tunnels, f.NextID, nil
}
