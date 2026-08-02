package hyperv

import (
	"net/netip"
	"testing"
)

func TestPickGuestIP(t *testing.T) {
	tests := []struct {
		name      string
		adapters  []NetworkAdapter
		subnet    string
		linkLocal bool
		want      string
	}{
		{
			name:     "no addresses reported yet",
			adapters: []NetworkAdapter{{Name: "Network Adapter"}},
			want:     "",
		},
		{
			name: "link-local only means DHCP has not finished",
			adapters: []NetworkAdapter{{
				IPAddresses: []string{"169.254.13.7", "fe80::1"},
			}},
			want: "",
		},
		{
			name: "IPv4 wins over IPv6",
			adapters: []NetworkAdapter{{
				IPAddresses: []string{"fd00::5", "172.30.1.4"},
			}},
			want: "172.30.1.4",
		},
		{
			name: "IPv6 is used when there is no IPv4",
			adapters: []NetworkAdapter{{
				IPAddresses: []string{"fe80::1", "fd00::5"},
			}},
			want: "fd00::5",
		},
		{
			name: "subnet filter picks the LAN address over the NAT one",
			adapters: []NetworkAdapter{
				{Name: "Default", IPAddresses: []string{"172.30.1.4"}},
				{Name: "External", IPAddresses: []string{"192.168.0.42"}},
			},
			subnet: "192.168.0.0/24",
			want:   "192.168.0.42",
		},
		{
			name: "subnet filter rejects everything outside it",
			adapters: []NetworkAdapter{
				{IPAddresses: []string{"172.30.1.4"}},
			},
			subnet: "192.168.0.0/24",
			want:   "",
		},
		{
			name: "loopback is never usable",
			adapters: []NetworkAdapter{{
				IPAddresses: []string{"127.0.0.1", "::1"},
			}},
			want: "",
		},
		{
			// On an Internal or Private switch nothing hands out addresses, so a
			// link-local one is not a transient state — it is the only one there
			// will ever be.
			name: "link-local is accepted when asked for",
			adapters: []NetworkAdapter{{
				IPAddresses: []string{"169.254.13.7"},
			}},
			linkLocal: true,
			want:      "169.254.13.7",
		},
		{
			// A guest can hold a link-local address alongside a real one while
			// DHCP settles; returning the link-local one then would be the worse
			// answer even though it was allowed.
			name: "a routable address wins over link-local even when link-local is allowed",
			adapters: []NetworkAdapter{{
				IPAddresses: []string{"169.254.13.7", "10.10.0.5"},
			}},
			linkLocal: true,
			want:      "10.10.0.5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var prefix netip.Prefix
			if tc.subnet != "" {
				prefix = netip.MustParsePrefix(tc.subnet)
			}
			got, _ := pickGuestIP(tc.adapters, prefix, tc.linkLocal)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Every reported address is returned even when none is selectable, so callers
// can tell "nothing reported" from "only link-local so far".
func TestPickGuestIPReturnsAllAddresses(t *testing.T) {
	adapters := []NetworkAdapter{
		{IPAddresses: []string{"169.254.1.1"}},
		{IPAddresses: []string{"fe80::2"}},
	}
	_, all := pickGuestIP(adapters, netip.Prefix{}, false)
	if len(all) != 2 {
		t.Fatalf("got %v, want 2 addresses", all)
	}
}
