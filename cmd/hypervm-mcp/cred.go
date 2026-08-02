//go:build windows

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/ipc"
)

// Credentials are handed to the service rather than written directly.
//
// The CLI runs as the logged-in user and the service as LocalSystem, so a file
// the CLI encrypted would be one the service could not read — and the data
// directory is not writable by the user anyway. Sending them over the pipe puts
// the one account that can do the work in charge of it.
func cmdCred(args []string) int {
	if len(args) == 0 {
		return fail("cred needs a subcommand: set, list, delete")
	}
	switch args[0] {
	case "set":
		return cmdCredSet(args[1:])
	case "list":
		return cmdCredList()
	case "delete":
		return cmdCredDelete(args[1:])
	default:
		return fail("unknown cred subcommand %q", args[0])
	}
}

func cmdCredSet(args []string) int {
	fs := flag.NewFlagSet("cred set", flag.ContinueOnError)
	vm := fs.String("vm", "", "VM name these credentials belong to")
	user := fs.String("user", "", "guest OS username")
	sshKey := fs.String("ssh-key", "", "path to an SSH private key to use for this VM")
	sshPort := fs.Int("ssh-port", 0, "SSH port (default 22)")
	noPassword := fs.Bool("no-password", false, "store only a key, without prompting for a password")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *vm == "" || *user == "" {
		return fail("--vm and --user are required")
	}

	req := map[string]any{
		"op":       "cred.set",
		"vm":       *vm,
		"username": *user,
		"ssh_port": *sshPort,
	}

	if *sshKey != "" {
		// Read as the user: the key lives in their profile, where the service
		// has no business reaching.
		key, err := os.ReadFile(*sshKey)
		if err != nil {
			return fail("read %s: %v", *sshKey, err)
		}
		req["ssh_private_key"] = string(key)
	}

	if !*noPassword {
		fmt.Printf("Password for %s@%s (leave empty to skip): ", *user, *vm)
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return fail("read password: %v", err)
		}
		if len(pw) > 0 {
			req["password"] = string(pw)
		}
		// Clear our copy promptly; it has already been marshalled for sending.
		for i := range pw {
			pw[i] = 0
		}
	}

	if req["password"] == nil && req["ssh_private_key"] == nil {
		return fail("nothing to store: supply a password or --ssh-key")
	}

	resp, err := sendControl(req)
	if err != nil {
		return fail("%v", err)
	}
	if !resp.OK {
		return fail("%s", resp.Error)
	}
	fmt.Printf("Stored credentials for %s.\n", *vm)
	return 0
}

func cmdCredList() int {
	resp, err := sendControl(map[string]any{"op": "cred.list"})
	if err != nil {
		return fail("%v", err)
	}
	if !resp.OK {
		return fail("%s", resp.Error)
	}

	var list []struct {
		VMName      string `json:"vm_name"`
		Username    string `json:"username"`
		SSHPort     int    `json:"ssh_port"`
		HasPassword bool   `json:"has_password"`
		HasKey      bool   `json:"has_key"`
	}
	raw, _ := json.Marshal(resp.Data)
	if err := json.Unmarshal(raw, &list); err != nil {
		return fail("decode response: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("No credentials stored.")
		return 0
	}

	fmt.Printf("%-28s %-16s %-6s %s\n", "VM", "USER", "PORT", "AUTH")
	for _, e := range list {
		var auth []string
		if e.HasKey {
			auth = append(auth, "key")
		}
		if e.HasPassword {
			auth = append(auth, "password")
		}
		port := e.SSHPort
		if port == 0 {
			port = 22
		}
		fmt.Printf("%-28s %-16s %-6d %s\n", e.VMName, e.Username, port, strings.Join(auth, "+"))
	}
	return 0
}

func cmdCredDelete(args []string) int {
	fs := flag.NewFlagSet("cred delete", flag.ContinueOnError)
	vm := fs.String("vm", "", "VM name to forget")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *vm == "" {
		return fail("--vm is required")
	}

	resp, err := sendControl(map[string]any{"op": "cred.delete", "vm": *vm})
	if err != nil {
		return fail("%v", err)
	}
	if !resp.OK {
		return fail("%s", resp.Error)
	}
	fmt.Printf("Deleted credentials for %s.\n", *vm)
	return 0
}

func cmdTunnelList() int {
	resp, err := sendControl(map[string]any{"op": "tunnel.list"})
	if err != nil {
		return fail("%v", err)
	}
	if !resp.OK {
		return fail("%s", resp.Error)
	}

	var list []struct {
		ID          string   `json:"id"`
		VMName      string   `json:"vm_name"`
		Mode        string   `json:"mode"`
		ListenAddrs []string `json:"listen_addrs"`
		GuestPort   int      `json:"guest_port"`
		ActiveConns int64    `json:"active_conns"`
		LastError   string   `json:"last_error"`
	}
	raw, _ := json.Marshal(resp.Data)
	if err := json.Unmarshal(raw, &list); err != nil {
		return fail("decode response: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("No tunnels open.")
		return 0
	}

	fmt.Printf("%-8s %-22s %-7s %-24s %s\n", "ID", "VM", "MODE", "LISTENING", "-> GUEST")
	for _, t := range list {
		fmt.Printf("%-8s %-22s %-7s %-24s :%d  (%d active)\n",
			t.ID, t.VMName, t.Mode, strings.Join(t.ListenAddrs, ","), t.GuestPort, t.ActiveConns)
		if t.LastError != "" {
			fmt.Printf("         last error: %s\n", t.LastError)
		}
	}
	return 0
}

func sendControl(req map[string]any) (*ipc.ControlResponse, error) {
	pipePath := `\\.\pipe\` + config.DefaultPipeName()
	if cfg, err := config.Load(); err == nil {
		pipePath = cfg.PipePath()
	}
	return ipc.SendControl(context.Background(), pipePath, req)
}
