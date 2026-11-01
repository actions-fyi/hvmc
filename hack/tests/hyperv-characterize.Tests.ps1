#Requires -Version 7.4
#Requires -Modules @{ ModuleName = 'Pester'; ModuleVersion = '5.5.0' }
<#
    Tests for the parts of hack/hyperv-characterize.ps1 that do not need Hyper-V:
    argument validation, safety checks against mocked cmdlets, anonymisation, and the
    JSON document (validated against hack/hyperv-characterize.schema.json) produced by a
    full run against a fake VM. Run on any OS with PowerShell 7.4+ and Pester 5:

        pwsh -NoProfile -Command "Invoke-Pester -Path hack/tests -Output Detailed"
#>

BeforeAll {
    $script:ScriptPath = Join-Path $PSScriptRoot '..' 'hyperv-characterize.ps1'
    $script:SchemaPath = Join-Path $PSScriptRoot '..' 'hyperv-characterize.schema.json'

    # Stand-ins so Pester can mock commands that do not exist off Windows/Hyper-V.
    $stubs = [ordered]@{
        'Get-VM'          = '[CmdletBinding()] param([string[]] $Name, [guid[]] $Id)'
        'Get-VMMemory'    = '[CmdletBinding()] param($VM, [string[]] $VMName)'
        'Set-VMMemory'    = '[CmdletBinding()] param($VM, [string[]] $VMName, [long] $StartupBytes)'
        'Get-CimInstance' = '[CmdletBinding()] param([string] $ClassName, [string] $Filter)'
    }
    foreach ($name in $stubs.Keys) {
        if (-not (Get-Command -Name $name -ErrorAction SilentlyContinue)) {
            Set-Item -Path "function:global:$name" -Value ([scriptblock]::Create($stubs[$name]))
        }
    }

    . $script:ScriptPath -VMName 'loaded-for-tests'

    $script:VmName = 'disposable-vm-7'
    $script:VmId = [guid]'3f1c2a9e-8b7d-4e6f-9a5b-1c2d3e4f5a6b'
    $script:HostName = 'HV-LAB-HOST-01'

    function script:New-FakeState {
        param(
            [long] $Startup = 4096MB,
            [long] $GuestFloor = 1000MB,
            [string] $Notes = "disposable test VM`nhvmc-probe",
            [string] $State = 'Running',
            [bool] $Dynamic = $false,
            [long] $HostFree = 32GB,
            [int] $Copies = 1,
            [bool] $PartialRelease = $true
        )
        $script:Fake = @{
            Startup = $Startup; Assigned = $Startup; GuestFloor = $GuestFloor; Notes = $Notes
            State = $State; Dynamic = $Dynamic; HostFree = $HostFree; Copies = $Copies; PartialRelease = $PartialRelease
            StopAfterTarget = $null; FailRestore = $false; Writes = [System.Collections.Generic.List[long]]::new()
        }
    }

    function script:New-FakeVM {
        [pscustomobject]@{
            Name = $script:VmName; Id = $script:VmId; State = $script:Fake.State; Status = 'Operating normally'
            MemoryAssigned = $script:Fake.Assigned; MemoryStartup = $script:Fake.Startup; MemoryDemand = 0L
            MemoryMinimum = 512MB; MemoryMaximum = 1TB; DynamicMemoryEnabled = $script:Fake.Dynamic
            Generation = 2; Version = '10.0'; Uptime = [timespan]::FromMinutes(42); IsClustered = $false
            Notes = $script:Fake.Notes
        }
    }

    function script:Register-FakeHyperV {
        Mock Get-VM {
            if ($null -ne $Id -and @($Id).Count -gt 0) {
                if ($Id[0] -eq $script:VmId) { return (New-FakeVM) }
                Write-Error -Message "Hyper-V was unable to find a virtual machine with ID '$($Id[0])'." -ErrorId 'InvalidParameter,Microsoft.HyperV.PowerShell.Commands.GetVM' -Category InvalidArgument
                return
            }
            if ($Name[0] -eq $script:VmName) { for ($i = 0; $i -lt $script:Fake.Copies; $i++) { New-FakeVM } ; return }
            Write-Error -Message "Hyper-V was unable to find a virtual machine with name '$($Name[0])'." -ErrorId 'InvalidParameter,Microsoft.HyperV.PowerShell.Commands.GetVM' -Category InvalidArgument
        }
        Mock Get-VMMemory {
            [pscustomobject]@{
                VMName = $script:VmName; VMId = $script:VmId; Startup = $script:Fake.Startup; Minimum = 512MB
                Maximum = 1TB; Buffer = 20; Priority = 50; DynamicMemoryEnabled = $script:Fake.Dynamic
                MaximumPerNumaNode = 65536
            }
        }
        Mock Get-CimInstance {
            [pscustomobject]@{
                Caption = 'Microsoft Windows Server Datacenter'; Version = '10.0.20348'; BuildNumber = '20348'
                TotalVisibleMemorySize = [uint64](64GB / 1KB); FreePhysicalMemory = [uint64]($script:Fake.HostFree / 1KB)
                CSName = $script:HostName
            }
        }
        Mock Set-VMMemory {
            $target = $StartupBytes
            $script:Fake.Writes.Add($target)
            $prefix = "'$($VMName[0])' failed to modify device 'Memory'. (Virtual machine ID $($script:VmId.ToString().ToUpperInvariant())) on host $($script:HostName)."
            if ($target % 2MB -ne 0) {
                Write-Error -Exception ([System.ArgumentException]::new("$prefix Invalid startup memory amount; it must be a multiple of 2 MB.")) -ErrorId 'InvalidParameter,Microsoft.HyperV.PowerShell.Commands.SetVMMemory' -Category InvalidArgument
                return
            }
            if ($script:Fake.FailRestore -and $target -eq $script:Fake.OriginalStartup) {
                Write-Error -Exception ([System.InvalidOperationException]::new("$prefix Restore refused.")) -ErrorId 'OperationFailed,Microsoft.HyperV.PowerShell.Commands.SetVMMemory' -Category NotSpecified
                return
            }
            if ($target -ge $script:Fake.GuestFloor) {
                $script:Fake.Assigned = $target
                $script:Fake.Startup = $target
                if ($null -ne $script:Fake.StopAfterTarget -and $target -eq $script:Fake.StopAfterTarget) { $script:Fake.State = 'Off' }
                return
            }
            # Guest releases what it can (or nothing); MemoryStartup still takes the requested value (DESIGN 3.5).
            if ($script:Fake.PartialRelease) { $script:Fake.Assigned = [math]::Min($script:Fake.Assigned, $script:Fake.GuestFloor) }
            $script:Fake.Startup = $target
            $inner = [System.ComponentModel.Win32Exception]::new(0x80070057, 'The guest could not release the requested memory.')
            Write-Error -Exception ([System.InvalidOperationException]::new("$prefix The virtual machine could not release enough memory.", $inner)) -ErrorId 'OperationFailed,Microsoft.HyperV.PowerShell.Commands.SetVMMemory' -Category NotSpecified
        }
        Mock Test-HyperVAvailable { [ordered]@{ available = $true; detail = 'fake'; moduleVersion = '2.0.0.0' } }
        Mock Test-IsElevated { $false }
        Mock Test-TokenHasHyperVAdministrators { $true }
        Mock Get-HostNameList { , @($script:HostName) }
        Mock Start-Sleep { }
        Mock Write-Host { }
    }

    function script:Invoke-FakeRun {
        param([hashtable] $Extra = @{})
        $script:Fake.OriginalStartup = $script:Fake.Startup
        $outDir = Join-Path $TestDrive ([guid]::NewGuid().ToString('N'))
        $params = @{ VMName = $script:VmName; OutDir = $outDir; SettleSeconds = 0 } + $Extra
        $code = @(Invoke-Characterization @params)[-1]
        $file = @(Get-ChildItem -LiteralPath $outDir -Filter '*.json' -ErrorAction SilentlyContinue)
        $json = $null
        if ($file.Count -eq 1) { $json = Get-Content -LiteralPath $file[0].FullName -Raw }
        [pscustomobject]@{ Code = $code; Files = $file.Count; Json = $json; Doc = $(if ($json) { $json | ConvertFrom-Json -AsHashtable }) }
    }
}

Describe 'argument validation' {
    It 'rejects <Case>' -ForEach @(
        @{ Case = 'a wildcard VM name'; Arguments = @('-VMName', 'probe*'); Pattern = 'wildcard' }
        @{ Case = 'an unaligned step'; Arguments = @('-VMName', 'x', '-StepMiB', '255'); Pattern = 'StepMiB' }
        @{ Case = 'a refuse target below 32 MiB'; Arguments = @('-VMName', 'x', '-RefuseTargetMiB', '16'); Pattern = 'RefuseTargetMiB' }
        @{ Case = 'a missing VM name'; Arguments = @(); Pattern = 'VMName' }
    ) {
        $pwsh = (Get-Process -Id $PID).Path
        $output = & $pwsh -NoLogo -NoProfile -NonInteractive -File $script:ScriptPath @Arguments 2>&1 | Out-String
        $LASTEXITCODE | Should -Not -Be 0
        $output | Should -Match $Pattern
    }

    It 'refuses with exit code 2 and changes nothing when Hyper-V is not available' -Skip:$IsWindows {
        $pwsh = (Get-Process -Id $PID).Path
        $output = & $pwsh -NoLogo -NoProfile -NonInteractive -File $script:ScriptPath -VMName 'any-vm' -OutDir (Join-Path $TestDrive 'none') 2>&1 | Out-String
        $LASTEXITCODE | Should -Be 2
        $output | Should -Match 'hyperv-available'
        Test-Path (Join-Path $TestDrive 'none') | Should -BeFalse
    }
}

Describe 'Test-ProbeMarker' {
    It 'returns <Expected> for <Notes>' -ForEach @(
        @{ Notes = 'hvmc-probe'; Expected = $true }
        @{ Notes = "disposable`nhvmc-probe: ok to resize"; Expected = $true }
        @{ Notes = 'scratch VM (hvmc-probe)'; Expected = $true }
        @{ Notes = 'HVMC-PROBE'; Expected = $false }
        @{ Notes = 'no-hvmc-probe'; Expected = $false }
        @{ Notes = 'hvmc-probes'; Expected = $false }
        @{ Notes = ''; Expected = $false }
        @{ Notes = $null; Expected = $false }
    ) {
        Test-ProbeMarker -Notes $Notes | Should -Be $Expected
    }
}

Describe 'Get-MidpointTarget' {
    It 'quantizes the midpoint up to the step' {
        Get-MidpointTarget -RefusedTargetBytes 256MB -CurrentAssignedBytes 1000MB -StepBytes 256MB | Should -Be 768MB
    }
    It 'falls back to 2 MiB alignment when the step would reach the current assignment' {
        Get-MidpointTarget -RefusedTargetBytes 768MB -CurrentAssignedBytes 1000MB -StepBytes 256MB | Should -Be 884MB
    }
    It 'returns null when nothing lies strictly between' {
        Get-MidpointTarget -RefusedTargetBytes 998MB -CurrentAssignedBytes 1000MB -StepBytes 256MB | Should -BeNullOrEmpty
        Get-MidpointTarget -RefusedTargetBytes 1000MB -CurrentAssignedBytes 1000MB -StepBytes 256MB | Should -BeNullOrEmpty
    }
}

Describe 'New-Plan' {
    It 'derives every target from the baseline' {
        $p = New-Plan -BaselineBytes 4096MB -StepBytes 256MB -LargeStepBytes 1024MB -RefuseTargetBytes 256MB
        $p.growStepTargetBytes | Should -Be 4352MB
        $p.growLargeTargetBytes | Should -Be 5376MB
        $p.unalignedTargetBytes | Should -Be 4097MB
        $p.plannedGrowBytes | Should -Be 1280MB
        $p.peakBytes | Should -Be 5376MB
    }
}

Describe 'safety checks' {
    BeforeEach {
        New-FakeState
        Register-FakeHyperV
    }

    It 'passes for an opted-in, running, static-memory VM with enough host memory' {
        $facts = Get-SafetyFactSet -VMName $script:VmName
        $plan = New-Plan -BaselineBytes 4096MB -StepBytes 256MB -LargeStepBytes 1024MB -RefuseTargetBytes 256MB
        $checks = Test-Safety -Facts $facts -Plan $plan -MarginBytes 4GB
        @($checks | Where-Object { -not $_.passed }) | Should -HaveCount 0
        @($checks).name | Should -Be @('hyperv-available', 'vm-unique', 'vm-running', 'static-memory', 'opt-in-marker', 'startup-matches-assigned', 'refuse-target', 'host-free-memory')
    }

    It 'fails <Check> when <Case>' -ForEach @(
        @{ Case = 'the marker is missing'; Setup = @{ Notes = 'production database' }; Check = 'opt-in-marker' }
        @{ Case = 'the VM is off'; Setup = @{ State = 'Off' }; Check = 'vm-running' }
        @{ Case = 'dynamic memory is enabled'; Setup = @{ Dynamic = $true }; Check = 'static-memory' }
        @{ Case = 'the host lacks free memory'; Setup = @{ HostFree = 4GB }; Check = 'host-free-memory' }
        @{ Case = 'two VMs share the name'; Setup = @{ Copies = 2 }; Check = 'vm-unique' }
        @{ Case = 'the VM does not exist'; Setup = @{ Copies = 0 }; Check = 'vm-unique' }
        @{ Case = 'the refuse target is not below startup'; Setup = @{ Startup = 256MB; GuestFloor = 128MB }; Check = 'refuse-target' }
    ) {
        New-FakeState @Setup
        $facts = Get-SafetyFactSet -VMName $script:VmName
        $plan = $null
        if ($null -ne $facts.vm) { $plan = New-Plan -BaselineBytes ([long]$facts.vm.MemoryStartup) -StepBytes 256MB -LargeStepBytes 1024MB -RefuseTargetBytes 256MB }
        $checks = Test-Safety -Facts $facts -Plan $plan -MarginBytes 4GB
        ($checks | Where-Object name -EQ $Check).passed | Should -BeFalse
    }

    It 'does not read memory settings when the name is ambiguous' {
        New-FakeState -Copies 2
        $null = Get-SafetyFactSet -VMName $script:VmName
        Should -Invoke Get-VMMemory -Times 0 -Exactly
    }

    It 'fails startup-matches-assigned when an earlier shrink left the VM inconsistent' {
        $script:Fake.Startup = 2048MB
        $facts = Get-SafetyFactSet -VMName $script:VmName
        $checks = Test-Safety -Facts $facts -Plan $null -MarginBytes 4GB
        ($checks | Where-Object name -EQ 'startup-matches-assigned').passed | Should -BeFalse
    }

    It 'requires a credential for -GuestCheck and elevation for -ServiceAccountProbe' {
        Mock Test-ProbeServiceExists { $true }
        $facts = Get-SafetyFactSet -VMName $script:VmName -ServiceAccountProbe
        $checks = Test-Safety -Facts $facts -Plan $null -MarginBytes 4GB -GuestCheck -ServiceAccountProbe
        ($checks | Where-Object name -EQ 'guest-credential').passed | Should -BeFalse
        ($checks | Where-Object name -EQ 'elevated').passed | Should -BeFalse
        ($checks | Where-Object name -EQ 'probe-service-absent').passed | Should -BeFalse
    }

    It 'stops at hyperv-available when the module is missing' {
        Mock Test-HyperVAvailable { [ordered]@{ available = $false; detail = 'not running on Windows'; moduleVersion = $null } }
        $facts = Get-SafetyFactSet -VMName $script:VmName
        $checks = Test-Safety -Facts $facts -Plan $null -MarginBytes 4GB
        @($checks).name | Should -Be @('hyperv-available')
        Should -Invoke Get-VM -Times 0 -Exactly
    }
}

Describe 'anonymisation' {
    BeforeAll {
        $script:Rules = New-RedactionRuleSet -VMName 'web' -VMId $script:VmId -HostNames @('hv01', 'hv01.corp.example')
    }

    It 'replaces VM name, VM ID (both formats) and host names in nested strings' {
        $doc = [ordered]@{
            a = "'web' failed on hv01.corp.example (Virtual machine ID $($script:VmId.ToString().ToUpperInvariant()))"
            b = @("id $($script:VmId.ToString('N'))", [ordered]@{ c = 'HV01 and Web.' })
            n = 42
        }
        $out = Protect-Document -InputObject $doc -Rules $script:Rules
        $out.a | Should -Be "'vm-under-test' failed on hyperv-host (Virtual machine ID 00000000-0000-4000-8000-000000000001)"
        $out.b[0] | Should -Be 'id 00000000000040008000000000000001'
        $out.b[1].c | Should -Be 'hyperv-host and vm-under-test.'
        $out.n | Should -Be 42
    }

    It 'leaves words that merely contain the name alone' {
        (Protect-Document -InputObject 'webserver web-ui cobweb' -Rules $script:Rules) | Should -Be 'webserver web-ui cobweb'
    }

    It 'detects leaks, including JSON-escaped values' {
        $rules = New-RedactionRuleSet -VMName 'lab\vm "a"' -VMId $null -HostNames @()
        $json = ConvertTo-Json -InputObject @{ m = 'lab\vm "a" failed' }
        (Find-RedactionLeak -Json $json -Rules $rules) | Should -Be @('vm-under-test')
        $clean = ConvertTo-Json -InputObject (Protect-Document -InputObject @{ m = 'lab\vm "a" failed' } -Rules $rules)
        (Find-RedactionLeak -Json $clean -Rules $rules).Count | Should -Be 0
    }
}

Describe 'ConvertTo-ErrorInfo' {
    It 'captures id, exception chain and category' {
        try {
            Write-Error -Exception ([System.InvalidOperationException]::new('outer', [System.IO.IOException]::new('inner'))) -ErrorId 'OperationFailed,Test' -Category ResourceUnavailable -ErrorAction Stop
        } catch {
            $info = ConvertTo-ErrorInfo -ErrorRecord $_
        }
        $info.fullyQualifiedErrorId | Should -Match '^OperationFailed,Test'
        $info.exception.type | Should -Be 'System.InvalidOperationException'
        $info.exception.message | Should -Be 'outer'
        $info.innerExceptions[0].type | Should -Be 'System.IO.IOException'
        $info.category.category | Should -Be 'ResourceUnavailable'
    }
}

Describe 'full run against a fake VM' {
    BeforeEach {
        New-FakeState
        Register-FakeHyperV
    }

    It 'writes a schema-valid, anonymised document and restores MemoryStartup' {
        $r = Invoke-FakeRun
        $r.Code | Should -Be 0
        $r.Files | Should -Be 1
        Test-Json -Json $r.Json -SchemaFile $script:SchemaPath | Should -BeTrue

        $r.Json | Should -Not -Match ([regex]::Escape($script:VmName))
        $r.Json | Should -Not -Match ([regex]::Escape($script:VmId.ToString()))
        $r.Json | Should -Not -Match ([regex]::Escape($script:HostName))
        $r.Doc.anonymization.leakCheck | Should -Be 'passed'

        $r.Doc.run.outcome | Should -Be 'completed'
        @($r.Doc.steps.name) | Should -Be @('get-vm-missing', 'set-unaligned', 'grow-step', 'grow-large', 'shrink-to-baseline', 'shrink-refused', 'shrink-partial-1', 'shrink-partial-2', 'shrink-partial-3', 'restore-1')
        $r.Doc.restore.restored | Should -BeTrue
        $script:Fake.Startup | Should -Be 4096MB
        $script:Fake.Writes[-1] | Should -Be 4096MB
    }

    It 'records refusals, partial shrinks and diverged MemoryStartup' {
        $r = Invoke-FakeRun
        $byName = @{}
        foreach ($s in $r.Doc.steps) { $byName[$s.name] = $s }

        $byName['get-vm-missing'].observed | Should -Be 'read-failed'
        $byName['get-vm-missing'].errors[0].fullyQualifiedErrorId | Should -Match '^InvalidParameter,Microsoft\.HyperV'

        $byName['set-unaligned'].observed | Should -Be 'refused'
        $byName['set-unaligned'].errors[0].exception.type | Should -Be 'System.ArgumentException'

        $byName['grow-step'].observed | Should -Be 'applied'
        $byName['grow-large'].settled.assignedBytes | Should -Be 5376MB

        $refused = $byName['shrink-refused']
        $refused.result | Should -Be 'failed'
        $refused.observed | Should -Be 'partially-applied'
        $refused.startupDiverged | Should -BeTrue
        $refused.settled.startupBytes | Should -Be 256MB
        $refused.settled.assignedBytes | Should -Be 1000MB
        $refused.errors[0].innerExceptions[0].type | Should -Be 'System.ComponentModel.Win32Exception'
        $refused.errors[0].exception.message | Should -Match "^'vm-under-test' failed to modify device 'Memory'\. \(Virtual machine ID 00000000-0000-4000-8000-000000000001\) on host hyperv-host\."

        $byName['shrink-partial-1'].targetBytes | Should -Be 768MB
        $byName['shrink-partial-1'].observed | Should -Be 'refused'
        $byName['restore-1'].direction | Should -Be 'grow'
    }

    It 'stops the midpoint retries once a midpoint shrink succeeds' {
        # Guest refuses without releasing anything; midpoints 2304 MiB (refused), 3328 MiB (applied).
        New-FakeState -GuestFloor 3000MB -PartialRelease $false
        $r = Invoke-FakeRun
        ($r.Doc.steps | Where-Object name -EQ 'shrink-refused').observed | Should -Be 'refused'
        $partials = @($r.Doc.steps | Where-Object { $_.name -like 'shrink-partial-*' })
        @($partials.targetBytes) | Should -Be @(2304MB, 3328MB)
        @($partials.observed) | Should -Be @('refused', 'applied')
        $r.Doc.restore.restored | Should -BeTrue
    }

    It 'refuses with exit code 2, writes nothing and never calls Set-VMMemory without the marker' {
        New-FakeState -Notes 'not opted in'
        $r = Invoke-FakeRun
        $r.Code | Should -Be 2
        $r.Files | Should -Be 0
        Should -Invoke Set-VMMemory -Times 0 -Exactly
    }

    It 'changes nothing on -DryRun' {
        $r = Invoke-FakeRun -Extra @{ DryRun = $true }
        $r.Code | Should -Be 0
        $r.Files | Should -Be 0
        Should -Invoke Set-VMMemory -Times 0 -Exactly
    }

    It 'aborts but still restores when the VM stops mid-sequence' {
        $script:Fake.StopAfterTarget = 5376MB
        $r = Invoke-FakeRun
        $r.Code | Should -Be 1
        $r.Doc.run.outcome | Should -Be 'aborted'
        $r.Doc.run.abortReason | Should -Match 'no longer Running'
        @($r.Doc.steps.name)[-1] | Should -Be 'restore-1'
        $r.Doc.restore.restored | Should -BeTrue
        Test-Json -Json $r.Json -SchemaFile $script:SchemaPath | Should -BeTrue
    }

    It 'aborts a grow when host free memory dropped below growth plus margin' {
        Mock Get-HostMemory {
            $script:hostReads++
            $free = if ($script:hostReads -le 1) { 32GB } else { 1GB }
            [ordered]@{ os = [pscustomobject]@{ Caption = 'c'; Version = 'v'; BuildNumber = 'b' }; totalBytes = 64GB; freeBytes = $free }
        }
        $script:hostReads = 0
        $r = Invoke-FakeRun
        $r.Doc.run.outcome | Should -Be 'aborted'
        $r.Doc.run.abortReason | Should -Match 'host free memory'
        @($r.Doc.steps | Where-Object name -EQ 'grow-step') | Should -HaveCount 0
    }

    It 'exits 3 and says so when MemoryStartup cannot be restored' {
        $script:Fake.FailRestore = $true
        $r = Invoke-FakeRun
        $r.Code | Should -Be 3
        $r.Doc.restore.restored | Should -BeFalse
        @($r.Doc.steps | Where-Object { $_.name -like 'restore-*' }) | Should -HaveCount 2
        Test-Json -Json $r.Json -SchemaFile $script:SchemaPath | Should -BeTrue
    }

    It 'measures guest reflection through the guest session' {
        Mock New-GuestSession { 'fake-session' }
        Mock Remove-GuestSession { }
        # Guest sees assigned minus a fixed 64 MiB firmware offset.
        Mock Get-GuestTotalBytes { $script:Fake.Assigned - 64MB }
        $cred = [pscredential]::new('guest', [securestring]::new())
        $r = Invoke-FakeRun -Extra @{ GuestCheck = $true; GuestCredential = $cred }
        $r.Code | Should -Be 0
        $r.Doc.baseline.guest.offsetBytes | Should -Be 64MB
        $grow = $r.Doc.steps | Where-Object name -EQ 'grow-step'
        $grow.guest.reached | Should -BeTrue
        $grow.guest.expectedTotalBytes | Should -Be (4352MB - 64MB)
        Test-Json -Json $r.Json -SchemaFile $script:SchemaPath | Should -BeTrue
        Should -Invoke Remove-GuestSession -Times 1 -Exactly
    }
}

Describe 'Measure-Settle' {
    It 'records values that change after the cmdlet returned' {
        Mock Start-Sleep { }
        $script:reads = 0
        Mock Get-VMMemoryState {
            $script:reads++
            $assigned = if ($script:reads -ge 2) { 4352MB } else { 4096MB }
            [ordered]@{ observedAt = 't'; state = 'Running'; status = 'ok'; assignedBytes = $assigned; startupBytes = 4352MB; vmMemoryStartupBytes = 4352MB }
        }
        $initial = [ordered]@{ observedAt = 't'; state = 'Running'; status = 'ok'; assignedBytes = 4096MB; startupBytes = 4352MB; vmMemoryStartupBytes = 4352MB }
        $clock = [System.Diagnostics.Stopwatch]::StartNew()
        $s = Measure-Settle -VMId ([guid]::NewGuid()) -Initial $initial -Seconds 1 -SinceReturn $clock
        $s.assignedBytes | Should -Be 4352MB
        $s.changes | Should -HaveCount 1
        $s.lastChangeAtMs | Should -Not -BeNullOrEmpty
        $s.windowMs | Should -Be 1000
    }
}

Describe 'service account probe script' {
    It 'is valid PowerShell and carries the error helpers' {
        $text = New-ProbeScript
        $tokens = $null
        $errors = $null
        $null = [System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$tokens, [ref]$errors)
        $errors | Should -BeNullOrEmpty
        $text | Should -Match 'function ConvertTo-ErrorInfo'
        $text | Should -Match 'Set-VMMemory -VMName \$in\.vmName'
    }

    It 'runs in its own pwsh process and records every operation' {
        $dir = Join-Path $TestDrive 'probe'
        $null = New-Item -ItemType Directory -Path $dir
        Set-Content -LiteralPath (Join-Path $dir 'probe.ps1') -Value (New-ProbeScript)
        ConvertTo-Json @{ vmName = 'disposable-vm-7'; stepBytes = 256MB } | Set-Content -LiteralPath (Join-Path $dir 'input.json')
        # Stub Hyper-V in the child process; Set-VMMemory refuses the shrink back.
        $prelude = @'
function global:Import-Module { param($Name) }
function global:Get-Module { param($Name) [pscustomobject]@{ Version = [version]'2.0.0.0' } }
function global:Get-VM { param($Name) [pscustomobject]@{ State = 'Running'; MemoryAssigned = 4GB; MemoryStartup = 4GB } }
function global:Get-VMMemory { param($VMName) [pscustomobject]@{ Startup = 4GB; DynamicMemoryEnabled = $false } }
function global:Get-CimInstance { param($ClassName) [pscustomobject]@{ FreePhysicalMemory = 1048576 } }
function global:Set-VMMemory { [CmdletBinding()] param($VMName, [long] $StartupBytes) if ($StartupBytes -lt 4GB + 256MB) { Write-Error -Message 'shrink refused' -ErrorId 'OperationFailed,Stub' -Category InvalidOperation } }
'@
        Set-Content -LiteralPath (Join-Path $dir 'prelude.ps1') -Value $prelude
        $pwsh = (Get-Process -Id $PID).Path
        & $pwsh -NoLogo -NoProfile -NonInteractive -Command ". '$dir/prelude.ps1'; & '$dir/probe.ps1'" | Out-Null
        $result = Get-Content -LiteralPath (Join-Path $dir 'result.json') -Raw | ConvertFrom-Json -AsHashtable
        $result.complete | Should -BeTrue
        @($result.operations.name) | Should -Be @('Import-Module Hyper-V', 'Get-VM', 'Get-VMMemory', 'Get-CimInstance Win32_OperatingSystem', 'Set-VMMemory grow', 'Set-VMMemory shrink back')
        @($result.operations.result) | Should -Be @('succeeded', 'succeeded', 'succeeded', 'succeeded', 'succeeded', 'failed')
        $result.operations[5].errors[0].fullyQualifiedErrorId | Should -Match '^OperationFailed,Stub'
    }
}
