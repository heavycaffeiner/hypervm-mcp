//go:build windows

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestExternalSwitchGuard checks that creating an External switch is refused
// until it is confirmed, and that the refusal explains what would happen to this
// host rather than warning in the abstract.
//
// It never creates a switch, so it does not disturb host networking.
func TestExternalSwitchGuard(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var adapters []map[string]any
	callList(t, session, ctx, "list_physical_adapters", map[string]any{}, &adapters)
	var adapter string
	for _, a := range adapters {
		if a["status"] == "Up" {
			adapter, _ = a["name"].(string)
			break
		}
	}
	if adapter == "" {
		t.Skip("no connected physical adapter")
	}

	var report map[string]any
	call(t, session, ctx, "preflight_external_switch",
		map[string]any{"net_adapter_name": adapter}, &report)

	t.Logf("adapter %v: wireless=%v only-uplink=%v dhcp=%v profile=%v addresses=%v",
		report["adapter_name"], report["is_wireless"], report["is_only_uplink"],
		report["uses_dhcp"], report["network_profile"], report["addresses"])
	risks, _ := report["risks"].([]any)
	for i, r := range risks {
		t.Logf("risk %d: %v", i+1, r)
	}
	if len(risks) == 0 {
		t.Fatal("the preflight reported no risks at all, which cannot be right")
	}

	// Without confirmation the call must refuse, and carry the same report.
	err := tryCall(t, session, ctx, "create_switch", map[string]any{
		"name": "hypervm-mcp-guard-check", "switch_type": "External",
		"net_adapter_name": adapter,
	})
	if err == nil {
		t.Fatal("an External switch was created without confirm_disruption")
	}
	t.Logf("refused: %v", err)
	for _, want := range []string{"confirm_disruption", adapter} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q", want)
		}
	}

	// The switch must not exist.
	var switches []map[string]any
	callList(t, session, ctx, "list_switches", map[string]any{}, &switches)
	for _, sw := range switches {
		if sw["name"] == "hypervm-mcp-guard-check" {
			t.Fatal("the refused switch was created anyway")
		}
		if sw["switch_type"] == "External" {
			t.Errorf("an External switch exists that this test did not expect: %v", sw["name"])
		}
	}
	t.Logf("no External switch exists; host networking untouched")
}
