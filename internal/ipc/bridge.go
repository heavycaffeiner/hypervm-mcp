//go:build windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
)

// Bridge connects an MCP client's stdio to the service's pipe.
//
// It copies bytes in both directions and never parses the protocol. That keeps
// the unprivileged half of the system free of protocol state, and means adding
// MCP tools to the service does not require redeploying the bridge.
func Bridge(ctx context.Context, pipePath string, stdin io.Reader, stdout io.Writer) error {
	conn, err := Dial(ctx, pipePath)
	if err != nil {
		return dialDiagnostic(pipePath, err)
	}
	defer conn.Close()

	// Either direction ending means the session is over. Closing the connection
	// unblocks whichever copy is still waiting.
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, stdin)
		conn.Close()
		done <- err
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		done <- err
	}()

	first := <-done
	conn.Close()
	<-done

	if first != nil && !errors.Is(first, net.ErrClosed) && !errors.Is(first, io.EOF) {
		return first
	}
	return nil
}

// dialDiagnostic turns a connection failure into something a person can act on.
// Bridge stderr is what an MCP client surfaces when a server will not start, so
// this is often the only message anyone sees.
func dialDiagnostic(pipePath string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("the HyperVM-MCP service is not running (%s does not exist).\n"+
			"Start it with: hypervm-mcp service start", pipePath)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("access to %s was denied.\n"+
			"The service only accepts the account it was installed for. "+
			"Reinstall as this user with: hypervm-mcp service install", pipePath)
	default:
		return fmt.Errorf("could not reach the HyperVM-MCP service on %s: %w\n"+
			"Check the service log under %%ProgramData%%\\HyperVM-MCP\\logs", pipePath, err)
	}
}
