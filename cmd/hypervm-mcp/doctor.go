//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/svcmgr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winsec"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// cmdDoctor reports what is and is not working.
//
// It checks the client side itself — the parts that fail before the service is
// even reachable — and asks the service for everything that needs Hyper-V. That
// split matters: the most common problems are exactly the ones that stop the two
// halves talking, and a diagnostic that needs the connection to report on the
// connection would be useless.
func cmdDoctor() int {
	var checks []check
	worst := "ok"
	note := func(c check) {
		checks = append(checks, c)
		if c.Status == "fail" || (c.Status == "warn" && worst == "ok") {
			worst = c.Status
		}
	}

	info, err := svcmgr.Status(context.Background())
	if err != nil {
		note(check{"Service", "fail", err.Error(), ""})
	} else {
		switch {
		case !info.Installed:
			note(check{"Service", "fail", "not installed", "Run: hypervm-mcp service install"})
		case info.State != "Running":
			note(check{"Service", "fail", "state " + info.State, "Run: hypervm-mcp service start"})
		default:
			note(check{"Service", "ok", fmt.Sprintf("running (pid %d)", info.PID), ""})
		}

		if !info.ConfigPresent {
			note(check{"Configuration", "fail", "missing", "Run: hypervm-mcp service install"})
		} else {
			note(check{"Configuration", "ok", config.ConfigPath(), ""})
		}

		switch {
		case info.PipeReachable:
			note(check{"Pipe", "ok", info.PipePath, ""})
		case info.AllowedSID != "" && info.AllowedSID != info.CurrentSID:
			note(check{"Pipe", "fail",
				"this account is not the one the service was installed for",
				"Run `hypervm-mcp service install` as this user."})
		default:
			note(check{"Pipe", "fail", info.PipeError, ""})
		}
	}

	if winsec.IsElevated() {
		// Worth saying because it changes what a permission error means: an
		// elevated shell can reach Hyper-V directly, so a failure below is the
		// service's, not a privilege problem.
		note(check{"Shell", "ok", "elevated", ""})
	}

	// Everything below needs the service; there is nothing to ask if the pipe
	// is not usable.
	if info != nil && info.PipeReachable {
		resp, err := sendControl(map[string]any{"op": "doctor"})
		switch {
		case err != nil:
			note(check{"Service checks", "fail", err.Error(), ""})
		case !resp.OK:
			note(check{"Service checks", "fail", resp.Error, ""})
		default:
			var remote []check
			raw, _ := json.Marshal(resp.Data)
			if err := json.Unmarshal(raw, &remote); err != nil {
				note(check{"Service checks", "fail", "could not decode the response: " + err.Error(), ""})
			}
			for _, c := range remote {
				note(c)
			}
		}
	}

	for _, c := range checks {
		fmt.Printf("%s  %-18s %s\n", symbol(c.Status), c.Name, c.Detail)
		if c.Hint != "" {
			fmt.Printf("                        %s\n", c.Hint)
		}
	}

	fmt.Println()
	switch worst {
	case "fail":
		fmt.Println("Something is broken; see the failures above.")
		return 1
	case "warn":
		fmt.Println("Working, with notes.")
	default:
		fmt.Println("All good.")
	}
	return 0
}

func symbol(status string) string {
	switch status {
	case "fail":
		return "[FAIL]"
	case "warn":
		return "[WARN]"
	default:
		return "[ ok ]"
	}
}
