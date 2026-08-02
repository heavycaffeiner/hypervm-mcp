//go:build windows

package e2e

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/ipc"
)

// nginxConf binds nginx to the guest's own loopback and nothing else.
//
// That is the point of the test. 127.0.0.1 inside the guest is not reachable
// from the host by any route, so a "direct" tunnel cannot serve this page. If
// the page comes back, an SSH channel is what carried it.
//
// This replaces the packaged nginx.conf outright rather than editing its default
// server block. Patching someone else's config with sed depends on their exact
// whitespace, and silently does nothing when it does not match — which is
// precisely the failure that would make this test pass for the wrong reason.
// There is deliberately no conf.d include, so nothing else can add a listener.
const nginxConf = `user nginx;
worker_processes auto;
error_log /var/log/nginx/error.log;
pid /run/nginx.pid;

events { worker_connections 1024; }

http {
    include       /etc/nginx/mime.types;
    default_type  application/octet-stream;
    access_log    /var/log/nginx/access.log;

    server {
        listen      127.0.0.1:80;
        server_name _;
        root        /usr/share/nginx/html;
        index       index.html;
    }
}
`

// guestHostEnv lets a run target an already-installed VM whose address Hyper-V
// cannot report, for example after a minimal install without hyperv-daemons.
const guestHostEnv = "HYPERVM_E2E_GUEST_HOST"

// TestRockyNginxTunnel installs nginx in the guest, binds it to the guest's own
// loopback, and fetches its default page through an SSH tunnel.
func TestRockyNginxTunnel(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	privateKey, _ := sshKeyPair(t)

	// The tunnel outlives this call and has no per-call credential, so the key
	// has to be on file before it can dial.
	storeCredentials(t, privateKey)

	host := os.Getenv(guestHostEnv)
	if host == "" {
		var ip map[string]any
		call(t, session, ctx, "wait_for_guest_ip",
			map[string]any{"name": rockyVM, "timeout_seconds": 300}, &ip)
		host, _ = ip["address"].(string)
		t.Logf("guest reported %s", host)
	} else {
		t.Logf("using pinned guest address %s", host)
	}

	exec := func(command string, allowFail bool) string {
		t.Helper()
		var res struct {
			Stdout      string `json:"stdout"`
			Stderr      string `json:"stderr"`
			ExitCode    int    `json:"exit_code"`
			Fingerprint string `json:"host_key_fingerprint"`
			FirstSeen   bool   `json:"host_key_first_seen"`
		}
		call(t, session, ctx, "ssh_exec", map[string]any{
			"vm_name":         rockyVM,
			"host":            host,
			"command":         command,
			"timeout_seconds": 600,
		}, &res)
		if res.FirstSeen {
			t.Logf("pinned host key %s", res.Fingerprint)
		}
		if res.ExitCode != 0 && !allowFail {
			t.Fatalf("guest command failed (exit %d): %s\n%s\n%s",
				res.ExitCode, command, res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	t.Logf("guest: %s", exec("cat /etc/rocky-release", false))

	// Hyper-V cannot report a guest IP without this agent. Installing it here
	// makes wait_for_guest_ip work for every later run against this VM.
	t.Log("installing hyperv-daemons and nginx")
	exec("sudo dnf install -y hyperv-daemons nginx", false)
	exec("sudo systemctl enable --now hypervkvpd hypervvssd hypervfcopyd || true", true)

	// Take over the configuration entirely so nothing is left listening on a
	// routable address; otherwise a direct tunnel could serve the page and the
	// test would prove nothing about SSH forwarding.
	exec("sudo rm -f /etc/nginx/conf.d/*.conf /etc/nginx/default.d/*.conf", true)
	exec("sudo tee /etc/nginx/nginx.conf >/dev/null <<'HYPERVMEOF'\n"+nginxConf+"HYPERVMEOF", false)
	exec("sudo nginx -t", false)
	exec("sudo systemctl enable --now nginx", false)
	exec("sudo systemctl restart nginx", false)

	listening := exec("sudo ss -ltnH 'sport = :80' || true", true)
	t.Logf("guest listeners on port 80:\n%s", listening)
	if !strings.Contains(listening, "127.0.0.1:80") {
		t.Fatalf("nginx is not listening on the guest's loopback:\n%s", listening)
	}
	for _, routable := range []string{"0.0.0.0:80", "*:80", "[::]:80"} {
		if strings.Contains(listening, routable) {
			t.Fatalf("nginx is also listening on %s, so reaching it would not prove SSH forwarding:\n%s",
				routable, listening)
		}
	}

	// A direct tunnel must fail against a loopback-bound service. Proving that
	// first is what makes the SSH result meaningful.
	var direct map[string]any
	call(t, session, ctx, "open_tunnel", map[string]any{
		"vm_name": rockyVM, "guest_port": 80, "host_port": 0,
		"mode": "direct", "guest_host": host, "auto_restore": false,
		"label": "negative control",
	}, &direct)
	directID, _ := direct["id"].(string)
	directURL := firstURL(t, direct)
	if body, err := httpGet(directURL); err == nil {
		t.Fatalf("direct mode reached a loopback-bound service, which should be impossible: %q", body)
	} else {
		t.Logf("direct mode failed as expected: %v", err)
	}
	call(t, session, ctx, "close_tunnel", map[string]any{"id": directID}, nil)

	// The real thing.
	var tun map[string]any
	call(t, session, ctx, "open_tunnel", map[string]any{
		"vm_name": rockyVM, "guest_port": 80, "host_port": 0,
		"mode": "ssh", "bind_scope": "loopback", "guest_host": host,
		"auto_restore": false, "label": "nginx over ssh",
	}, &tun)
	tunID, _ := tun["id"].(string)
	url := firstURL(t, tun)
	t.Logf("tunnel %s listening at %s", tunID, url)
	defer func() {
		_ = tryCall(t, session, context.Background(), "close_tunnel", map[string]any{"id": tunID})
	}()

	body, err := httpGet(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Logf("received %d bytes", len(body))

	// Rocky's packaged index page identifies itself; anything else means we
	// reached something other than the guest's nginx.
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "nginx") && !strings.Contains(lower, "rocky") {
		t.Fatalf("the response does not look like nginx's default page:\n%.500s", body)
	}
	t.Logf("nginx default page served through the SSH tunnel")

	var tunnels []map[string]any
	call(t, session, ctx, "list_tunnels", map[string]any{"vm_name": rockyVM}, &tunnels)
	for _, x := range tunnels {
		t.Logf("tunnel %v: %v conns, %v bytes down", x["id"], x["total_conns"], x["bytes_down"])
	}
}

// storeCredentials puts the guest key on file for the primary VM.
func storeCredentials(t *testing.T, privateKey string) {
	t.Helper()
	storeCredentialsFor(t, rockyVM, privateKey)
}

// storeCredentialsFor puts a guest key on file through the service's control
// channel, the same path the cred CLI uses.
//
// Credentials are keyed by VM name, so a clone needs its own entry even though
// it carries the same account and key as the image it came from. That is the
// right behaviour: the store should not assume two VMs share an identity just
// because one was copied from the other.
func storeCredentialsFor(t *testing.T, vm, privateKey string) {
	t.Helper()
	storeCredentialsAs(t, vm, rockyUser, rockyPassword, privateKey)
}

// storeCredentialsAs is storeCredentialsFor with the account spelled out, for
// guests that do not carry the Rocky test account.
func storeCredentialsAs(t *testing.T, vm, user, password, privateKey string) {
	t.Helper()
	pipePath := `\\.\pipe\` + config.DefaultPipeName()
	if cfg, err := config.Load(); err == nil {
		pipePath = cfg.PipePath()
	}
	resp, err := ipc.SendControl(context.Background(), pipePath, map[string]any{
		"op":              "cred.set",
		"vm":              vm,
		"username":        user,
		"password":        password,
		"ssh_private_key": privateKey,
		"ssh_port":        22,
	})
	if err != nil {
		t.Fatalf("store credentials for %s: %v", vm, err)
	}
	if !resp.OK {
		t.Fatalf("store credentials for %s: %s", vm, resp.Error)
	}
}

func firstURL(t *testing.T, tun map[string]any) string {
	t.Helper()
	urls, ok := tun["urls"].([]any)
	if !ok || len(urls) == 0 {
		t.Fatalf("tunnel reported no URLs: %v", tun)
	}
	s, _ := urls[0].(string)
	return s
}

func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return string(body), &httpError{resp.StatusCode}
	}
	return string(body), nil
}

type httpError struct{ code int }

func (e *httpError) Error() string { return http.StatusText(e.code) }
