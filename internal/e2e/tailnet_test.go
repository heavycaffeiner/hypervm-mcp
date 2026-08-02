//go:build windows

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestTailnetTunnel exposes the guest's loopback-bound nginx to the tailnet and
// checks the whole chain: the tunnel binds the host's Tailscale addresses, a
// firewall rule scoped to them appears, the page is served, and closing the
// tunnel takes the rule away again.
//
// It reaches the tunnel through the host's own tailnet address rather than from
// another machine. That proves the binding and the data path; whether a remote
// peer is permitted is a matter of tailnet ACLs, which this does not touch.
func TestTailnetTunnel(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var status map[string]any
	call(t, session, ctx, "tailnet_status", map[string]any{}, &status)
	t.Logf("tailscale: installed=%v state=%v addrs=%v name=%v",
		status["installed"], status["backend_state"], status["addresses"], status["magic_dns_name"])

	if status["backend_state"] != "Running" {
		t.Skipf("Tailscale is not connected (state %v)", status["backend_state"])
	}
	addrs, _ := status["addresses"].([]any)
	if len(addrs) == 0 {
		t.Skip("Tailscale reported no addresses")
	}

	var tun map[string]any
	call(t, session, ctx, "open_tunnel", map[string]any{
		"vm_name": rockyVM, "guest_port": 80, "host_port": 0,
		"mode": "ssh", "bind_scope": "tailnet",
		"guest_host":   os.Getenv(guestHostEnv),
		"auto_restore": false, "label": "nginx on the tailnet",
	}, &tun)
	id, _ := tun["id"].(string)
	rule, _ := tun["firewall_rule"].(string)
	t.Logf("tunnel %s listening at %v (firewall rule %q)", id, tun["listen_addrs"], rule)
	if w, ok := tun["warnings"].([]any); ok && len(w) > 0 {
		t.Logf("warnings: %v", w)
	}

	closed := false
	defer func() {
		if !closed {
			_ = tryCall(t, session, context.Background(), "close_tunnel", map[string]any{"id": id})
		}
	}()

	// Every address Tailscale reported must be bound, or some peers would reach
	// the service and others would silently fail.
	listen, _ := tun["listen_addrs"].([]any)
	if len(listen) != len(addrs) {
		t.Errorf("bound %d addresses but Tailscale reported %d: %v vs %v",
			len(listen), len(addrs), listen, addrs)
	}

	if rule == "" {
		t.Error("no firewall rule was created for a tailnet-scoped tunnel")
	} else if !firewallRuleExists(t, rule) {
		t.Errorf("firewall rule %q was reported but does not exist", rule)
	} else {
		t.Logf("firewall rule %s is present", rule)
	}

	// Fetch through the tailnet address, not loopback.
	url := firstURL(t, tun)
	if strings.Contains(url, "127.0.0.1") {
		t.Fatalf("the tunnel bound loopback rather than a tailnet address: %s", url)
	}
	body, err := httpGet(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "nginx") && !strings.Contains(lower, "rocky") {
		t.Fatalf("the response does not look like nginx's default page:\n%.300s", body)
	}
	t.Logf("served %d bytes over the tailnet address", len(body))

	call(t, session, ctx, "close_tunnel", map[string]any{"id": id}, nil)
	closed = true

	// A rule outliving its listener would leave a port allowed with nothing
	// behind it, which is exactly the mess this design exists to avoid.
	if rule != "" && firewallRuleExists(t, rule) {
		t.Errorf("firewall rule %q survived the tunnel being closed", rule)
	} else if rule != "" {
		t.Logf("firewall rule %s was removed with the tunnel", rule)
	}
}

// TestTailscaleServe puts Tailscale's HTTPS front end over a loopback tunnel and
// fetches the page at the MagicDNS name.
//
// Unlike a tailnet-bound tunnel this gives the service a name and a certificate,
// which is what you want for anything a browser will open.
func TestTailscaleServe(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var status map[string]any
	call(t, session, ctx, "tailnet_status", map[string]any{}, &status)
	if status["backend_state"] != "Running" {
		t.Skipf("Tailscale is not connected (state %v)", status["backend_state"])
	}

	// serve forwards to a loopback port, so the tunnel underneath must be one.
	var tun map[string]any
	call(t, session, ctx, "open_tunnel", map[string]any{
		"vm_name": rockyVM, "guest_port": 80, "host_port": 0,
		"mode": "ssh", "bind_scope": "loopback",
		"guest_host": os.Getenv(guestHostEnv), "auto_restore": false,
	}, &tun)
	id, _ := tun["id"].(string)
	defer func() {
		_ = tryCall(t, session, context.Background(), "close_tunnel", map[string]any{"id": id})
	}()

	const servePath = "/hypervm-e2e"
	var served map[string]any
	call(t, session, ctx, "tailscale_serve", map[string]any{
		"tunnel_id": id, "path": servePath,
	}, &served)
	url, _ := served["url"].(string)
	t.Logf("serving %v at %s", served["backend"], url)

	defer func() {
		_ = tryCall(t, session, context.Background(), "tailscale_serve",
			map[string]any{"tunnel_id": id, "path": servePath, "off": true})
	}()

	// Tailscale provisions the certificate on first request, so allow a retry.
	var body string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		body, err = httpGet(url)
		if err == nil {
			break
		}
		t.Logf("attempt %d: %v", attempt+1, err)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if lower := strings.ToLower(body); !strings.Contains(lower, "nginx") && !strings.Contains(lower, "rocky") {
		t.Fatalf("the response does not look like nginx's default page:\n%.300s", body)
	}
	t.Logf("nginx served over HTTPS at the tailnet name, %d bytes", len(body))
}

// firewallRuleExists asks Windows directly rather than trusting what the tool
// reported, since the point is to check the tool told the truth.
func firewallRuleExists(t *testing.T, name string) bool {
	t.Helper()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"if (Get-NetFirewallRule -Name '"+name+"' -ErrorAction SilentlyContinue) { 'yes' } else { 'no' }")
	out, err := cmd.Output()
	if err != nil {
		t.Logf("could not query the firewall: %v", err)
		return false
	}
	return strings.Contains(string(out), "yes")
}
