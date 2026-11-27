// Package svc integrates the hvmc agent with the platform's service manager
// (DESIGN §4.5, §4.8): `hvmc agent install`, `uninstall` and `status`, and
// running `hvmc agent run` under the Windows Service Control Manager or
// systemd.
//
// The orchestration (argument checks, the order of the steps, the policy
// confirmation, plan and status output) is portable and drives a
// [Platform], which does the machine-specific work and names its steps
// ([Labels]). [NewPlatform] returns the Windows service integration or the
// systemd one (Linux); elsewhere there is none.
package svc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/fileacl"
)

// Service names and the system locations of DESIGN §4.5.
const (
	// ServiceName is the service and the name of its virtual account,
	// NT SERVICE\hvmc-agent.
	ServiceName = fileacl.ServiceName
	// DisplayName is the service's display name.
	DisplayName = "Hyper-V Memory Coordinator Agent"
	// Description is the service's description.
	Description = "Serves the hvmc coordinator's gRPC API for this machine (host or guest agent)."
	// ServiceAccount is the virtual account the service runs as.
	ServiceAccount = `NT SERVICE\` + ServiceName
	// HyperVAdminsSID is the well-known SID of BUILTIN\Hyper-V
	// Administrators; its name is localized, so it is never used.
	HyperVAdminsSID = "S-1-5-32-578"
	// VMMS is the Hyper-V Virtual Machine Management service, a dependency
	// of the host role.
	VMMS = "vmms"
	// FirewallGroup groups the agent's firewall rules, so they are
	// replaced and removed together.
	FirewallGroup = ServiceName
)

// Recovery actions (DESIGN §4.5): restart after 5 s, 30 s and 60 s, reset
// the failure count after a day, also for stops with an error.
var (
	RecoveryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 60 * time.Second}
	RecoveryReset  = 24 * time.Hour
)

// State is a service's state as the service manager reports it.
type State string

// Service states.
const (
	NotInstalled State = "not installed"
	Stopped      State = "stopped"
	StartPending State = "starting"
	StopPending  State = "stopping"
	Running      State = "running"
	Other        State = "other"
)

// ServiceSpec is the service's configuration.
type ServiceSpec struct {
	Name, DisplayName, Description string
	// Binary is the installed executable; Args follow it on the command
	// line.
	Binary string
	Args   []string
	// Account is the account the service runs as.
	Account string
	// Dependencies are services that must run first.
	Dependencies []string
	// RecoveryDelays restart the service after its 1st, 2nd and 3rd
	// failure; RecoveryReset resets the failure count.
	RecoveryDelays []time.Duration
	RecoveryReset  time.Duration
	// PreshutdownTimeout is how long the service may take after a
	// preshutdown notification: the drain plus a margin.
	PreshutdownTimeout time.Duration
}

// FirewallRule is one inbound TCP allow rule for the agent's program.
type FirewallRule struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Port        int    `json:"port"`
	// RemoteAddresses are IP addresses, CIDR prefixes or ranges; empty
	// means any source.
	RemoteAddresses []string `json:"remoteAddresses,omitempty"`
}

// Labels name a platform's steps in the output of install and uninstall.
// An empty label skips its step: the platform method is not called.
type Labels struct {
	// RegisterEventSource and RemoveEventSource are the Event Log source's
	// steps.
	RegisterEventSource, RemoveEventSource string
	// ConfigureService and RemoveService are the service's steps.
	ConfigureService, RemoveService string
	// AddHyperVAdmin and RemoveHyperVAdmin are the host's group membership.
	AddHyperVAdmin, RemoveHyperVAdmin string
	// Firewall ends the summary of the rules install writes ("in the
	// firewall"); RemoveFirewallRules is uninstall's step.
	Firewall, RemoveFirewallRules string
	// Logs is where the service's log is, for a start that fails.
	Logs string
}

// windowsLabels are the Windows service's steps.
func windowsLabels() Labels {
	return Labels{
		RegisterEventSource: "Registered the Event Log source " + agent.EventSource,
		RemoveEventSource:   "Removed the Event Log source " + agent.EventSource,
		ConfigureService:    "Configured service " + ServiceName + " (" + ServiceAccount + ", delayed automatic start)",
		RemoveService:       "Removed service " + ServiceName,
		AddHyperVAdmin:      "Added " + ServiceAccount + " to Hyper-V Administrators (" + HyperVAdminsSID + ")",
		RemoveHyperVAdmin:   "Removed " + ServiceAccount + " from Hyper-V Administrators",
		Firewall:            "in the firewall",
		RemoveFirewallRules: "Removed the firewall rules (group " + FirewallGroup + ")",
		Logs:                "the Event Log, source " + agent.EventSource,
	}
}

// Platform does the machine-specific work of install and uninstall. Every
// method is idempotent: it succeeds when the machine is already in the
// requested state.
type Platform interface {
	// Describe names the platform's steps.
	Describe() Labels
	// CheckPrivileges fails unless the process may install services.
	CheckPrivileges() error
	// CheckSpec fails for a service the platform cannot install, before
	// install changes anything.
	CheckSpec(spec ServiceSpec) error
	// FindPwsh returns PowerShell 7's absolute path (host prerequisite).
	FindPwsh() (string, error)
	// BinaryPath is where the binary is installed.
	BinaryPath() string
	// InstallBinary copies src to BinaryPath (nothing when src is it).
	InstallBinary(src string) error
	// RemoveBinary removes the installed binary and its directory. When
	// the running program is that binary, it schedules the removal for
	// the next restart and reports true.
	RemoveBinary() (deferred bool, err error)
	// RegisterEventSource registers the Event Log source.
	RegisterEventSource() error
	RemoveEventSource() error
	// ServiceState returns the service's state and process ID.
	ServiceState() (State, uint32, error)
	// ConfigureService creates the service or updates its configuration.
	ConfigureService(spec ServiceSpec) error
	// StartService starts the service and waits until it runs.
	StartService(ctx context.Context) error
	// StopService stops the service and waits until it stopped.
	StopService(ctx context.Context) error
	// RemoveService deletes the service and waits until it is gone.
	RemoveService(ctx context.Context) error
	// AddHyperVAdmin adds the service account to Hyper-V Administrators;
	// RemoveHyperVAdmin removes it.
	AddHyperVAdmin() error
	RemoveHyperVAdmin() error
	// SetFirewallRules replaces the agent's rules with rules, scoped to
	// the installed binary and the service; RemoveFirewallRules removes
	// them.
	SetFirewallRules(rules []FirewallRule) error
	RemoveFirewallRules() error
}

// ErrUnsupported is returned by NewPlatform where no service integration
// exists yet.
var ErrUnsupported = errors.New("service integration is not available on this platform")

// serviceSpec returns the service of a role: `<binary> agent run`, plus
// --config and --state-dir when paths are not the platform defaults; the
// host role depends on vmms.
func serviceSpec(role agent.Role, binary string, paths, defaults agent.Paths) ServiceSpec {
	args := []string{"agent", "run"}
	if !samePath(paths.Config, defaults.Config) {
		args = append(args, "--config", paths.Config)
	}
	if !samePath(paths.State, defaults.State) {
		args = append(args, "--state-dir", paths.State)
	}
	var deps []string
	if role == agent.RoleHost {
		deps = []string{VMMS}
	}
	return ServiceSpec{
		Name: ServiceName, DisplayName: DisplayName, Description: Description,
		Binary: binary, Args: args, Account: ServiceAccount, Dependencies: deps,
		RecoveryDelays: RecoveryDelays, RecoveryReset: RecoveryReset,
		PreshutdownTimeout: agent.DrainTimeout + 10*time.Second,
	}
}

// samePath compares cleaned paths as the platform does: case-insensitive
// on Windows ([caseInsensitivePaths]), exactly elsewhere.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if caseInsensitivePaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// firewallRules returns the rules for the gRPC and health ports of c,
// narrowed to firewall.remoteAddresses when set. The addresses must be IP
// addresses, CIDR prefixes or ranges (a-b); anything else is refused.
func firewallRules(c agent.Config) ([]FirewallRule, error) {
	var remote []string
	if c.Firewall != nil {
		for _, a := range c.Firewall.RemoteAddresses {
			if !validRemoteAddress(a) {
				return nil, fmt.Errorf("firewall.remoteAddresses: %q is not an IP address, CIDR prefix or range", a)
			}
			remote = append(remote, a)
		}
	}
	var rules []FirewallRule
	for _, p := range []struct{ key, addr, name, display string }{
		{"listen", c.Listen, ServiceName + "-grpc", "hvmc agent (gRPC, mTLS)"},
		{"health", c.Health, ServiceName + "-health", "hvmc agent (health and metrics)"},
	} {
		port, err := addrPort(p.addr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.key, err)
		}
		rules = append(rules, FirewallRule{Name: p.name, DisplayName: p.display, Port: port, RemoteAddresses: remote})
	}
	return rules, nil
}

func addrPort(addr string) (int, error) {
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %q: want 1 to 65535", ps)
	}
	return port, nil
}

func validRemoteAddress(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return false
	}
	a, err1 := netip.ParseAddr(from)
	b, err2 := netip.ParseAddr(to)
	return err1 == nil && err2 == nil && a.Is4() == b.Is4() && !b.Less(a)
}

// healthURL is the /readyz URL of the agent's health server on this
// machine: the configured host, or loopback for a wildcard address.
func healthURL(c agent.Config) (string, error) {
	host, port, err := net.SplitHostPort(c.Health)
	if err != nil {
		return "", err
	}
	if ip, err := netip.ParseAddr(host); host == "" || (err == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
		if err == nil && ip.Is6() {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz", nil
}
