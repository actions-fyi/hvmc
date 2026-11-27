//go:build linux

package svc

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestUFWRuleArgs(t *testing.T) {
	got, err := ufwRuleArgs([]FirewallRule{
		{Name: "grpc", Port: 9443},
		{Name: "health", Port: 9090, RemoteAddresses: []string{"10.0.0.0/8", "192.0.2.7", "2001:db8::/32", "198.51.100.10-198.51.100.13"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rule := func(from, port string) []string {
		args := []string{"allow", "proto", "tcp"}
		if from != "" {
			args = append(args, "from", from)
		}
		return append(args, "to", "any", "port", port, "comment", "hvmc-agent")
	}
	want := [][]string{
		rule("", "9443"),
		rule("10.0.0.0/8", "9090"),
		rule("192.0.2.7/32", "9090"),
		rule("2001:db8::/32", "9090"),
		rule("198.51.100.10/31", "9090"),
		rule("198.51.100.12/31", "9090"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ufw commands (-want +got):\n%s", diff)
	}
	for _, bad := range []FirewallRule{
		{Name: "port", Port: 0},
		{Name: "addr", Port: 1, RemoteAddresses: []string{"10.0.0.0/8; rm -rf /"}},
		{Name: "range", Port: 1, RemoteAddresses: []string{"10.0.0.9-10.0.0.1"}},
		{Name: "mixed", Port: 1, RemoteAddresses: []string{"10.0.0.1-2001:db8::1"}},
		{Name: "cidr range", Port: 1, RemoteAddresses: []string{"10.0.0.0/8-10.0.0.9"}},
		{Name: "empty", Port: 1, RemoteAddresses: []string{""}},
		// Each range expands to 46 prefixes: 92 rules, over the cap.
		{Name: "too many", Port: 1, RemoteAddresses: []string{"10.0.0.1-10.255.255.254", "11.0.0.1-11.255.255.254"}},
	} {
		if _, err := ufwRuleArgs([]FirewallRule{bad}); err == nil || (bad.Name == "too many" && !strings.Contains(err.Error(), "more than 64")) {
			t.Errorf("%s: accepted %+v", bad.Name, bad)
		}
	}
}

func TestRangePrefixes(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     []string
	}{
		{"10.0.0.0", "10.0.0.255", []string{"10.0.0.0/24"}},
		{"10.0.0.1", "10.0.0.1", []string{"10.0.0.1/32"}},
		{"10.0.0.1", "10.0.0.6", []string{"10.0.0.1/32", "10.0.0.2/31", "10.0.0.4/31", "10.0.0.6/32"}},
		{"0.0.0.0", "255.255.255.255", []string{"0.0.0.0/0"}},
		{"10.0.0.255", "10.0.1.0", []string{"10.0.0.255/32", "10.0.1.0/32"}},
		{"2001:db8::", "2001:db8::ffff", []string{"2001:db8::/112"}},
		{"2001:db8::1", "2001:db8::2", []string{"2001:db8::1/128", "2001:db8::2/128"}},
		{"::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", []string{"::/0"}},
	} {
		var got []string
		for _, p := range rangePrefixes(netip.MustParseAddr(tc.from), netip.MustParseAddr(tc.to)) {
			got = append(got, p.String())
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s-%s: %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// showAdded is `ufw show added` (ufw 0.36) with two agent rules, an
// operator's rule and a rule with another comment.
const showAdded = `Added user rules (see 'ufw status' for running firewall):
ufw allow 22/tcp
ufw allow from 10.0.0.0/8 to any port 9443 proto tcp comment 'hvmc-agent'
ufw allow 9090/tcp comment 'hvmc-agent'
ufw allow 8080/tcp comment 'web'
`

func TestOwnUFWRules(t *testing.T) {
	got, err := ownUFWRules(showAdded)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"allow", "from", "10.0.0.0/8", "to", "any", "port", "9443", "proto", "tcp"},
		{"allow", "9090/tcp"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("rules (-want +got):\n%s", diff)
	}
	if got, err := ownUFWRules("Added user rules (see 'ufw status' for running firewall):\n(None)\n"); err != nil || len(got) != 0 {
		t.Errorf("none: %v, %v", got, err)
	}
	// A rule with the agent's comment but words ufw would not print for
	// it is refused, not passed to ufw.
	if _, err := ownUFWRules("ufw allow from 10.0.0.0/8 to any port 9443 app $(id) comment 'hvmc-agent'\n"); err == nil {
		t.Error("unexpected words accepted")
	}
}

// fakeUFW records ufw's argv and answers status and show added.
type fakeUFW struct {
	status, added string
	calls         [][]string
	fail          string
}

func (f *fakeUFW) ufw(t *testing.T) ufw {
	path := filepath.Join(t.TempDir(), "ufw")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return ufw{path: path, run: func(_ context.Context, p string, args ...string) ([]byte, error) {
		if p != path {
			t.Errorf("ran %s, want %s", p, path)
		}
		f.calls = append(f.calls, args)
		switch {
		case f.fail != "" && strings.Join(args, " ") == f.fail:
			return nil, errors.New("ufw failed")
		case slices.Equal(args, []string{"status"}):
			return []byte(f.status), nil
		case slices.Equal(args, []string{"show", "added"}):
			return []byte(f.added), nil
		}
		return []byte("Rules updated\n"), nil
	}}
}

func TestUFWSetAndRemove(t *testing.T) {
	rules := []FirewallRule{{Name: "grpc", Port: 9443}}
	active := &fakeUFW{status: "Status: active\n\nTo Action From\n", added: showAdded}
	if err := active.ufw(t).set(context.Background(), rules); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"status"},
		{"show", "added"},
		{"delete", "allow", "from", "10.0.0.0/8", "to", "any", "port", "9443", "proto", "tcp"},
		{"delete", "allow", "9090/tcp"},
		{"allow", "proto", "tcp", "to", "any", "port", "9443", "comment", "hvmc-agent"},
	}
	if diff := cmp.Diff(want, active.calls); diff != "" {
		t.Errorf("active (-want +got):\n%s", diff)
	}

	inactive := &fakeUFW{status: "Status: inactive\n", added: showAdded}
	if err := inactive.ufw(t).set(context.Background(), rules); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([][]string{{"status"}}, inactive.calls); diff != "" {
		t.Errorf("inactive touched the firewall (-want +got):\n%s", diff)
	}

	// Removal deletes the agent's rules whether ufw is active or not.
	if err := inactive.ufw(t).remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(inactive.calls); n != 4 || inactive.calls[1][0] != "show" || inactive.calls[3][0] != "delete" {
		t.Errorf("remove: %v", inactive.calls)
	}

	failing := &fakeUFW{status: "Status: active\n", added: "", fail: "allow proto tcp to any port 9443 comment hvmc-agent"}
	if err := failing.ufw(t).set(context.Background(), rules); err == nil {
		t.Error("a failing ufw allow was not reported")
	}

	// Without ufw nothing runs.
	none := ufw{path: filepath.Join(t.TempDir(), "missing"), run: func(context.Context, string, ...string) ([]byte, error) {
		t.Error("ran a missing ufw")
		return nil, nil
	}}
	if err := none.set(context.Background(), rules); err != nil {
		t.Error(err)
	}
	if err := none.remove(context.Background()); err != nil {
		t.Error(err)
	}
}
