package guest

import "testing"

// Interface names are validated differently per guest family because they are
// embedded differently: the Linux path builds shell text, the Windows path a
// single-quoted PowerShell literal. Applying the Linux rule to both rejected
// every ordinary Windows adapter name, so this pins the split.
func TestInterfaceNamePatterns(t *testing.T) {
	cases := []struct {
		name           string
		linux, windows bool
		why            string
	}{
		{"eth0", true, true, "a plain Linux name suits both"},
		{"ens160", true, true, ""},
		{"br-lan.100", true, true, "dots and dashes are ordinary"},
		{"Ethernet 2", false, true, "the usual Windows name has a space"},
		{"vEthernet (Default Switch)", false, true, "Hyper-V host adapters carry parentheses"},
		{"Wi-Fi", true, true, ""},
		{"eth0; rm -rf /", false, true, "shell metacharacters must not reach the Linux path"},
		{"eth0\nup", false, false, "a line break defeats quoting on either side"},
		{"eth0\x00", false, false, "so does a NUL"},
		{"", false, false, "empty is not a name"},
	}

	for _, c := range cases {
		if got := ifaceNamePattern.MatchString(c.name); got != c.linux {
			t.Errorf("linux %q: got %v, want %v (%s)", c.name, got, c.linux, c.why)
		}
		if got := winIfaceNamePattern.MatchString(c.name); got != c.windows {
			t.Errorf("windows %q: got %v, want %v (%s)", c.name, got, c.windows, c.why)
		}
	}
}

// A name that passes the Windows check still reaches PowerShell inside a quoted
// literal, so the quoting has to survive a name containing a quote.
func TestPSQuote(t *testing.T) {
	cases := map[string]string{
		"Ethernet 2":                 "'Ethernet 2'",
		"vEthernet (Default Switch)": "'vEthernet (Default Switch)'",
		"it's":                       "'it''s'",
		"'; Stop-Computer; '":        "'''; Stop-Computer; '''",
		"":                           "''",
	}
	for in, want := range cases {
		if got := psQuote(in); got != want {
			t.Errorf("psQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
