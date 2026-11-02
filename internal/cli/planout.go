package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/matfax/hvmc/internal/ledger"
)

// none marks a value that does not apply or is not known.
const none = "-"

// writeJSON writes the report as indented JSON.
func writeJSON(w io.Writer, rep *planReport) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// writeText writes the report as aligned tables: host, VMs, queue and, if
// any agent call failed, notes.
func writeText(w io.Writer, rep *planReport) error {
	var b strings.Builder
	writeHost(&b, rep.Snapshot)
	writeVMs(&b, rep.Snapshot.VMs)
	writeQueue(&b, rep)
	writeNotes(&b, rep.Notes)
	_, err := io.WriteString(w, b.String())
	return err
}

// table writes tab-separated rows as aligned columns, indented by two
// spaces.
func table(b *strings.Builder, rows [][]string) {
	tw := tabwriter.NewWriter(b, 0, 8, 2, ' ', 0)
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "  %s\n", strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

func writeHost(b *strings.Builder, s ledger.Snapshot) {
	frozen := "no"
	if s.Frozen {
		frozen = "yes"
	}
	b.WriteString("Host\n")
	table(b, [][]string{
		{"host agent", string(s.HostAgentState)},
		{"frozen", frozen},
		{"capacity", formatBytes(s.Capacity)},
		{"reserve", formatBytes(s.Reserve)},
		{"committed", formatBytes(s.Committed)},
		{"free", formatBytes(s.HostFree)},
		{"in flight", formatBytes(s.InFlight)},
		{"pool", formatBytes(s.Pool)},
	})
}

func writeVMs(b *strings.Builder, vms []ledger.VMView) {
	b.WriteString("\nVMs\n")
	if len(vms) == 0 {
		b.WriteString("  none\n")
		return
	}
	rows := [][]string{{"NAME", "MODE", "STATE", "ASSIGNED", "USED", "FLOOR", "HEADROOM", "COMMITTED", "LEASED"}}
	for _, v := range vms {
		rows = append(rows, vmRow(v))
	}
	table(b, rows)
}

func vmRow(v ledger.VMView) []string {
	if !v.Managed {
		return []string{
			string(v.Name), none, string(v.State), formatBytes(v.Assigned),
			none, none, none, formatBytes(v.Committed), none,
		}
	}
	used := none
	if v.UsedKnown {
		used = formatBytes(v.Used)
	}
	return []string{
		string(v.Name), string(v.Mode), string(v.State), formatBytes(v.Assigned), used,
		formatBytes(v.Floor), formatBytes(v.Headroom), formatBytes(v.Committed), formatBytes(v.Leased),
	}
}

// writeQueue writes one row per queued request and candidate VM, in queue
// order.
func writeQueue(b *strings.Builder, rep *planReport) {
	b.WriteString("\nQueue\n")
	if len(rep.Evaluation) == 0 {
		b.WriteString("  no pending requests\n")
		return
	}
	reqs := make(map[ledger.RequestID]ledger.Request, len(rep.Snapshot.Leases))
	for _, lv := range rep.Snapshot.Leases {
		reqs[lv.Request.ID] = lv.Request
	}
	rows := [][]string{{"POS", "REQUEST", "KIND", "PRIORITY", "BYTES", "GRANTABLE", "VM", "FIT", "DELTA", "HEADROOM", "POOL", "REASON"}}
	for _, ev := range rep.Evaluation {
		rows = append(rows, queueRows(ev, reqs[ev.ID])...)
	}
	table(b, rows)
}

func queueRows(ev requestEvaluation, r ledger.Request) [][]string {
	head := []string{
		strconv.Itoa(int(ev.Position)), string(ev.ID), string(r.Kind),
		strconv.Itoa(int(r.PriorityClass)), formatBytes(r.Bytes), yesNo(ev.Grantable),
	}
	if len(ev.Fits) == 0 {
		return [][]string{append(head, none, "no", none, none, none, "NoCandidates")}
	}
	rows := make([][]string, 0, len(ev.Fits))
	for _, f := range ev.Fits {
		rows = append(rows, append(append([]string(nil), head...), fitColumns(f)...))
	}
	return rows
}

// fitColumns are VM, FIT, DELTA, HEADROOM, POOL and REASON of one fit.
// Delta and headroom are not computed when the ledger is frozen or the VM
// is not a candidate.
func fitColumns(f ledger.Fit) []string {
	delta, headroom := formatBytes(f.Delta), formatBytes(f.Headroom)
	switch f.Reason {
	case ledger.ReasonHostFrozen, ledger.ReasonUnknownVM, ledger.ReasonNotCandidate:
		delta, headroom = none, none
	default:
	}
	reason := string(f.Reason)
	if f.Fits || reason == "" {
		reason = none
	}
	return []string{string(f.VM), yesNo(f.Fits), delta, headroom, formatBytes(f.Pool), reason}
}

func writeNotes(b *strings.Builder, notes []string) {
	if len(notes) == 0 {
		return
	}
	b.WriteString("\nNotes\n")
	for _, n := range notes {
		b.WriteString("  " + n + "\n")
	}
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// formatBytes writes a byte count as a binary quantity, e.g. 8Gi or 7680Mi.
func formatBytes(v int64) string {
	return resource.NewQuantity(v, resource.BinarySI).String()
}
