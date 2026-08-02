//go:build windows

package tunnel

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/logx"
)

func testManager() *Manager {
	return NewManager(Deps{Log: logx.NewWriter(nil, logx.LevelError)})
}

func TestResolveBind(t *testing.T) {
	m := testManager()
	ctx := context.Background()

	tests := []struct {
		scope   string
		want    []string
		wantErr bool
	}{
		{scope: "loopback", want: []string{"127.0.0.1"}},
		{scope: "all", want: []string{""}},
		{scope: "192.168.0.5", want: []string{"192.168.0.5"}},
		{scope: "fd7a:115c:a1e0::1", want: []string{"fd7a:115c:a1e0::1"}},
		{scope: "not-an-address", wantErr: true},
		{scope: "192.168.0.0/24", wantErr: true}, // a prefix is not an address
	}

	for _, tc := range tests {
		got, err := m.resolveBind(ctx, tc.scope)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error, got %v", tc.scope, got)
			} else if hverr.CodeOf(err) != hverr.InvalidArgument {
				t.Errorf("%q: got code %s, want INVALID_ARGUMENT", tc.scope, hverr.CodeOf(err))
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.scope, err)
			continue
		}
		if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("%q: got %v, want %v", tc.scope, got, tc.want)
		}
	}
}

// A port the host itself holds cannot be tunnelled, and suggesting another port
// would be useless for a client that cannot be told one. The error has to say so.
func TestPortErrorNamesSystemPorts(t *testing.T) {
	err := portError(445, "127.0.0.1", net.ErrClosed)
	if hverr.CodeOf(err) != hverr.PortInUse {
		t.Fatalf("got code %s, want PORT_IN_USE", hverr.CodeOf(err))
	}
	msg := err.Error()
	for _, want := range []string{"SMB", "External switch"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
}

func TestPortErrorSuggestsAnotherPort(t *testing.T) {
	err := portError(8080, "127.0.0.1", net.ErrClosed)
	if !strings.Contains(err.Error(), "another host_port") {
		t.Errorf("an ordinary port should suggest picking a different one:\n%s", err)
	}
}

// Binding must be all-or-nothing: a tunnel half-listening on one of two
// addresses would serve some peers and silently fail others.
func TestListenRollsBackOnFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port

	// The second address collides with the listener above.
	listeners, _, _, err := listen([]string{"127.0.0.2", "127.0.0.1"}, port)
	if err == nil {
		for _, ln := range listeners {
			ln.Close()
		}
		t.Fatal("expected the second bind to fail")
	}
	if len(listeners) != 0 {
		t.Fatalf("got %d listeners back on failure, want none", len(listeners))
	}

	// The rolled-back address must be free again.
	again, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", "0"))
	if err != nil {
		t.Fatalf("first address was not released: %v", err)
	}
	again.Close()
}

// Port 0 means "any free port", and every address must end up on the same one.
func TestListenSharesTheChosenPort(t *testing.T) {
	listeners, addrs, port, err := listen([]string{"127.0.0.1", "127.0.0.2"}, 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		for _, ln := range listeners {
			ln.Close()
		}
	}()

	if port == 0 {
		t.Fatal("no port was chosen")
	}
	for _, a := range addrs {
		_, p, err := net.SplitHostPort(a)
		if err != nil {
			t.Fatalf("bad address %q", a)
		}
		if p != itoa(port) {
			t.Errorf("%s is not on the chosen port %d", a, port)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
