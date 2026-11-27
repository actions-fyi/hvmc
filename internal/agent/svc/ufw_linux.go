//go:build linux

package svc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// The agent's ufw rules (DESIGN §2.1, §4.5). ufw runs by its absolute path
// with a fixed argv and the C locale, never through a shell; every value
// in the argv is a port number or an address prefix this package
// formatted itself. The rules carry the comment hvmc-agent, which is how
// they are found again: `ufw show added` prints every user rule as the ufw
// command that adds it, and the agent's are deleted with the same words.

// ufwPath is ufw's location on Ubuntu and Debian.
const ufwPath = "/usr/sbin/ufw"

// ufwComment marks the agent's rules.
const ufwComment = ServiceName

// maxUFWRules caps the rules install adds: firewall.remoteAddresses with
// ranges that expand to more prefixes is refused rather than flooding ufw.
const maxUFWRules = 64

// ufw runs ufw commands.
type ufw struct {
	path string
	run  func(ctx context.Context, path string, args ...string) ([]byte, error)
}

func runUFW(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // G204: fixed program, argv built by ufwRuleArgs or parsed from ufw itself.
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("ufw %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// installed reports whether ufw is on the machine.
func (u ufw) installed() (bool, error) {
	_, err := os.Stat(u.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// active reports whether ufw is installed and enabled.
func (u ufw) active(ctx context.Context) (bool, error) {
	if ok, err := u.installed(); !ok || err != nil {
		return false, err
	}
	out, err := u.run(ctx, u.path, "status")
	if err != nil {
		return false, err
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(first) == "Status: active", nil
}

// set replaces the agent's rules with rules while ufw is active.
func (u ufw) set(ctx context.Context, rules []FirewallRule) error {
	active, err := u.active(ctx)
	if err != nil || !active {
		return err
	}
	add, err := ufwRuleArgs(rules)
	if err != nil {
		return err
	}
	if err := u.remove(ctx); err != nil {
		return err
	}
	out, err := u.run(ctx, u.path, "show", "added")
	if err != nil {
		return err
	}
	theirs := otherUFWRules(string(out))
	for _, args := range add {
		// ufw matches rules without their comment: adding a rule the
		// operator already has would put the agent's comment on it, and
		// uninstall would then delete the operator's rule. The rule
		// allows the same traffic, so it is left to the operator.
		if theirs[ufwShown(args)] {
			continue
		}
		if _, err := u.run(ctx, u.path, args...); err != nil {
			return err
		}
	}
	return nil
}

// ufwShown is how `ufw show added` prints the rule that args (from
// ufwRuleArgs, without `comment …`) add.
func ufwShown(args []string) string {
	var from, port string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "from":
			from = args[i+1]
		case "port":
			port = args[i+1]
		}
	}
	if from == "" {
		return "allow " + port + "/tcp"
	}
	// ufw prints a single host without its /32 or /128.
	if p, err := netip.ParsePrefix(from); err == nil && p.IsSingleIP() {
		from = p.Addr().String()
	}
	return "allow from " + from + " to any port " + port + " proto tcp"
}

// otherUFWRules returns the rules of `ufw show added` that are not the
// agent's, as ufw prints them without a comment.
func otherUFWRules(out string) map[string]bool {
	rules := map[string]bool{}
	for line := range strings.Lines(out) {
		words, ok := strings.CutPrefix(strings.TrimSpace(line), "ufw ")
		if !ok {
			continue
		}
		spec, comment, hasComment := strings.Cut(words, " comment ")
		if hasComment && strings.Trim(comment, "'") == ufwComment {
			continue
		}
		rules[strings.Join(strings.Fields(spec), " ")] = true
	}
	return rules
}

// remove deletes every rule with the agent's comment.
func (u ufw) remove(ctx context.Context) error {
	if ok, err := u.installed(); !ok || err != nil {
		return err
	}
	out, err := u.run(ctx, u.path, "show", "added")
	if err != nil {
		return err
	}
	del, err := ownUFWRules(string(out))
	if err != nil {
		return err
	}
	for _, args := range del {
		if _, err := u.run(ctx, u.path, append([]string{"delete"}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

// ufwRuleArgs returns the ufw commands that add rules: one per port and
// source, each an inbound TCP allow rule with the agent's comment. A range
// a-b, which ufw does not know, becomes the prefixes that cover it.
func ufwRuleArgs(rules []FirewallRule) ([][]string, error) {
	var cmds [][]string
	for _, r := range rules {
		if r.Port < 1 || r.Port > 65535 {
			return nil, fmt.Errorf("rule %s: port %d out of range", r.Name, r.Port)
		}
		sources, err := ufwSources(r.RemoteAddresses)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			args := []string{"allow", "proto", "tcp"}
			if src.IsValid() {
				args = append(args, "from", src.String())
			}
			args = append(args, "to", "any", "port", strconv.Itoa(r.Port), "comment", ufwComment)
			cmds = append(cmds, args)
		}
	}
	if len(cmds) > maxUFWRules {
		return nil, fmt.Errorf("firewall.remoteAddresses needs %d ufw rules for the %d ports, more than %d; "+
			"use fewer or wider CIDR prefixes instead of ranges", len(cmds), len(rules), maxUFWRules)
	}
	return cmds, nil
}

// ufwSources turns firewall.remoteAddresses into prefixes; none means any
// source, returned as one invalid prefix.
func ufwSources(addrs []string) ([]netip.Prefix, error) {
	if len(addrs) == 0 {
		return []netip.Prefix{{}}, nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ps, err := parseSource(a)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

func parseSource(s string) ([]netip.Prefix, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		return []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}, nil
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return []netip.Prefix{p.Masked()}, nil
	}
	from, to, ok := strings.Cut(s, "-")
	if ok {
		a, err1 := netip.ParseAddr(from)
		b, err2 := netip.ParseAddr(to)
		if err1 == nil && err2 == nil && a.Is4() == b.Is4() && !b.Less(a) {
			return rangePrefixes(a, b), nil
		}
	}
	return nil, fmt.Errorf("firewall.remoteAddresses: %q is not an IP address, CIDR prefix or range", s)
}

// rangePrefixes returns the fewest prefixes that cover exactly a to b
// (a <= b, same family): from a, the largest aligned block that does not
// pass b, repeated.
func rangePrefixes(a, b netip.Addr) []netip.Prefix {
	bits := a.BitLen()
	lo, hi := addrInt(a), addrInt(b)
	one := big.NewInt(1)
	var out []netip.Prefix
	for lo.Cmp(hi) <= 0 {
		// The block size is limited by lo's trailing zero bits and by
		// the addresses left up to hi.
		size := int(lo.TrailingZeroBits())
		if lo.Sign() == 0 {
			size = bits
		}
		left := new(big.Int).Sub(hi, lo)
		left.Add(left, one)
		for size > 0 && new(big.Int).Lsh(one, uint(size)).Cmp(left) > 0 {
			size--
		}
		out = append(out, netip.PrefixFrom(intAddr(lo, bits), bits-size))
		lo.Add(lo, new(big.Int).Lsh(one, uint(size)))
	}
	return out
}

func addrInt(a netip.Addr) *big.Int {
	b := a.AsSlice()
	return new(big.Int).SetBytes(b)
}

func intAddr(v *big.Int, bits int) netip.Addr {
	b := v.FillBytes(make([]byte, bits/8))
	a, _ := netip.AddrFromSlice(b)
	return a
}

// ownUFWRules parses `ufw show added` and returns, for every rule with the
// agent's comment, the words that delete it (after `ufw delete`). Every
// word is checked to be one ufw itself prints for such a rule, so nothing
// but a rule specification reaches the argv.
func ownUFWRules(out string) ([][]string, error) {
	var rules [][]string
	for line := range strings.Lines(out) {
		words, ok := strings.CutPrefix(strings.TrimSpace(line), "ufw ")
		if !ok {
			continue
		}
		spec, comment, ok := strings.Cut(words, " comment ")
		if !ok || strings.Trim(comment, "'") != ufwComment {
			continue
		}
		args := strings.Fields(spec)
		for _, w := range args {
			if !ufwWord(w) {
				return nil, fmt.Errorf("ufw show added: unexpected word %q in rule %q", w, strings.TrimSpace(line))
			}
		}
		rules = append(rules, args)
	}
	return rules, nil
}

// ufwWord reports whether w may appear in a rule the agent added: ufw's
// keywords, a port (9443 or 9443/tcp) or an address prefix.
func ufwWord(w string) bool {
	switch w {
	case "allow", "in", "proto", "tcp", "from", "to", "any", "port":
		return true
	}
	if port, ok := strings.CutSuffix(w, "/tcp"); ok {
		w = port
	}
	if n, err := strconv.Atoi(w); err == nil {
		return n >= 1 && n <= 65535
	}
	if _, err := netip.ParsePrefix(w); err == nil {
		return true
	}
	_, err := netip.ParseAddr(w)
	return err == nil
}
