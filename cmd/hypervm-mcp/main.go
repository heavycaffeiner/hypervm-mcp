//go:build windows

// Command hypervm-mcp is both the MCP bridge an MCP client launches and the CLI that
// installs and inspects the service behind it.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `hypervm-mcp - Hyper-V MCP server

Usage:
  hypervm-mcp bridge                 Relay MCP traffic to the service (run by MCP clients)
  hypervm-mcp service install        Install and start the service (prompts for elevation once)
  hypervm-mcp service uninstall      Stop and remove the service
                                   --purge  also delete stored configuration and credentials
  hypervm-mcp service start|stop     Control the installed service
  hypervm-mcp service status         Report service state and whether this user can reach it
  hypervm-mcp cred set               Store guest credentials for a VM
                                       --vm NAME --user NAME [--ssh-key PATH] [--ssh-port N]
  hypervm-mcp cred list              Show which VMs have credentials (never the secrets)
  hypervm-mcp cred delete --vm NAME  Forget a VM's credentials
  hypervm-mcp tunnel list            Show open tunnels
  hypervm-mcp doctor                 Check the setup and report what to fix
  hypervm-mcp version                Print the version

Add to a workspace's .mcp.json:

  {
    "mcpServers": {
      "hypervm-mcp": { "command": "hypervm-mcp", "args": ["bridge"] }
    }
  }
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "bridge":
		return cmdBridge(args[1:])
	case "service":
		return cmdService(args[1:])
	case "cred":
		return cmdCred(args[1:])
	case "doctor":
		return cmdDoctor()
	case "tunnel":
		if len(args) > 1 && args[1] == "list" {
			return cmdTunnelList()
		}
		return fail("tunnel needs a subcommand: list")
	case "version", "--version", "-v":
		fmt.Println(version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// fail prints an error the way the CLI should and returns an exit code.
func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	return 1
}
