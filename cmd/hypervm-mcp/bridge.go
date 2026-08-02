//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/ipc"
)

// cmdBridge relays this process's stdio to the service's pipe.
//
// It holds no MCP state of its own, so it keeps working unchanged as tools are
// added to the service.
func cmdBridge(_ []string) int {
	pipePath := `\\.\pipe\` + config.DefaultPipeName
	if cfg, err := config.Load(); err == nil {
		pipePath = cfg.PipePath()
	}

	// Ctrl+C and client shutdown both end the session cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := ipc.Bridge(ctx, pipePath, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "hypervm-mcp bridge: %v\n", err)
		return 1
	}
	return 0
}
