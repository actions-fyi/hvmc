package svc

// Firewall rules are written by fixed PowerShell scripts with the built-in
// NetSecurity cmdlets (New-NetFirewallRule, Remove-NetFirewallRule; DESIGN
// §2.1). The scripts are constant text: every value reaches them as JSON on
// stdin and is bound to a cmdlet parameter, never interpolated into code,
// and no shell is involved. Windows PowerShell 5.1 runs them, because it is
// on every Windows machine, including guests without PowerShell 7.
//
// A lookup that finds no rule leaves $? false, which would make the
// process exit 1, so the scripts end with an explicit exit 0; a failing
// cmdlet still ends them earlier, with exit 1, through
// $ErrorActionPreference = 'Stop'.

// firewallParams is the JSON the scripts read.
type firewallParams struct {
	Group   string         `json:"group"`
	Program string         `json:"program,omitempty"`
	Service string         `json:"service,omitempty"`
	Rules   []FirewallRule `json:"rules,omitempty"`
}

// firewallSetScript replaces the group's rules with params.rules, each an
// inbound TCP allow rule for the agent's program and service on one port.
const firewallSetScript = `$ErrorActionPreference = 'Stop'
$p = (@($input) -join [Environment]::NewLine) | ConvertFrom-Json
$old = @(Get-NetFirewallRule -Group $p.group -ErrorAction SilentlyContinue)
if ($old.Count -gt 0) { $old | Remove-NetFirewallRule }
foreach ($r in $p.rules) {
  $remote = 'Any'
  if ($r.remoteAddresses) { $remote = [string[]]$r.remoteAddresses }
  $null = New-NetFirewallRule -Name $r.name -DisplayName $r.displayName -Group $p.group -Direction Inbound -Action Allow -Protocol TCP -LocalPort ([string]$r.port) -RemoteAddress $remote -Program $p.program -Service $p.service -Profile Any
}
exit 0`

// firewallRemoveScript removes the group's rules; none is not an error.
const firewallRemoveScript = `$ErrorActionPreference = 'Stop'
$p = (@($input) -join [Environment]::NewLine) | ConvertFrom-Json
$old = @(Get-NetFirewallRule -Group $p.group -ErrorAction SilentlyContinue)
if ($old.Count -gt 0) { $old | Remove-NetFirewallRule }
exit 0`
