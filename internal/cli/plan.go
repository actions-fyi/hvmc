package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/matfax/hvmc/internal/hostconfig"
	"github.com/matfax/hvmc/internal/ledger"
)

const planLong = `Plan builds the ledger for a HostConfig against in-process fake agents and
prints what it would decide: the host (capacity, reserve, committed, pool,
frozen), every VM (mode, assigned, floor, headroom, committed, leased, state)
and the queue in order with each request's fit or blocking cause. Nothing
outside the process is contacted or changed.

The fake host and guest agents are started from --scenario and read through
the agent wire API, as the coordinator reads real agents. The scenario is the
fake-agent scenario format of internal/fake (host, policy, vms, guests) with
an optional list of requests that are queued in file order:

  host: {name: hyperv-1, total: 64Gi, overhead: 4Gi}
  vms:
    - {name: vm-a, startup: 8Gi}
  guests:
    - {name: vm-a, vm: vm-a, os: windows, used: 3Gi}
  requests:
    - name: request-1      # default request-<n>
      kind: External       # Pod | External (default) | Reconciler | Probe
      vm: vm-a             # required unless kind is Pod
      bytes: 6Gi
      priorityClass: 0     # higher first
    - kind: Pod
      candidateVMs: [vm-a, linux-node-1]
      bytes: 2Gi

The scenario's host is the HostConfig's host agent; a guest serves the VM it
is linked to (vm), or the VM of its own name. Without --scenario, plan
simulates an idle host of spec.capacity with every VM at its floor.

--output json prints the same as a stable JSON object for scripts: at, host,
vms, queue (each request with its fits per candidate VM and, when it is not
grantable, its blockingCause), agents and notes. Byte values are integers in
fields ending in Bytes.`

// planOptions are the flags of `hvmc plan`.
type planOptions struct {
	hostConfig string
	scenario   string
	output     string
}

// Output formats of `hvmc plan`.
const (
	outputText = "text"
	outputJSON = "json"
)

// callTimeout bounds every agent call of `hvmc plan`, so a scenario with a
// hanging agent ends instead of blocking.
const callTimeout = 2 * time.Second

func newPlanCommand(e env) *cobra.Command {
	var o planOptions
	c := &cobra.Command{
		Use:   "plan -f hostconfig.yaml [--scenario scenario.yaml] [--output text|json]",
		Short: "Dry-run the ledger for a HostConfig against fake agents",
		Long:  planLong,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.hostConfig == "" {
				return usageErrorf("--filename (-f) is required")
			}
			if o.output != outputText && o.output != outputJSON {
				return usageErrorf("--output %q: want %s or %s", o.output, outputText, outputJSON)
			}
			return runPlan(cmd.Context(), o, e.now(), cmd.OutOrStdout())
		},
	}
	f := c.Flags()
	f.StringVarP(&o.hostConfig, "filename", "f", "", "HostConfig manifest (YAML)")
	f.StringVar(&o.scenario, "scenario", "", "fake-agent scenario with optional requests (YAML)")
	f.StringVarP(&o.output, "output", "o", outputText, "output format: text or json")
	return c
}

func runPlan(ctx context.Context, o planOptions, now time.Time, w io.Writer) error {
	hc, err := hostconfig.Load(o.hostConfig)
	if err != nil {
		return err
	}
	cfg, err := hostconfig.ToLedgerConfig(hc)
	if err != nil {
		return err
	}
	pf, err := planInput(o.scenario, cfg)
	if err != nil {
		return err
	}
	rep, err := buildPlan(ctx, cfg, pf, now)
	if err != nil {
		return err
	}
	if o.output == outputJSON {
		return writeJSON(w, rep)
	}
	return writeText(w, rep)
}

// planInput loads the scenario file, or derives one from cfg without it.
func planInput(path string, cfg ledger.Config) (*planFile, error) {
	if path == "" {
		return derivedPlanFile(cfg)
	}
	pf, err := loadPlanFile(path)
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", path, err)
	}
	return pf, nil
}
