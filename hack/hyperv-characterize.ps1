<#
.SYNOPSIS
    Spike S1: record how Set-VMMemory really behaves on a running static-memory VM.

.DESCRIPTION
    Runs a fixed sequence of Set-VMMemory -StartupBytes calls against one disposable,
    explicitly opted-in VM and records, after every step, MemoryAssigned, MemoryStartup,
    the cmdlet duration and, for failures, the complete error records. The result is one
    JSON file under -OutDir with VM name, VM ID and host name replaced by placeholders,
    so it can be committed as a fixture (internal/agent/host/testdata/), plus a summary
    on the console.

    Sequence (B = MemoryStartup at start, S = -StepMiB, L = -LargeStepMiB):
      get-vm-missing       Get-VM -Id <fixed GUID that does not exist>     expect refusal
      set-unaligned        B + 1 MiB (not 2 MiB aligned)                   expect refusal
      grow-step            B + S                                           expect success
      grow-large           B + S + L                                       expect success
      shrink-to-baseline   B                                               expect success
      shrink-refused       -RefuseTargetMiB (below what the guest frees)   expect refusal
      shrink-partial-1..3  midpoint between the last refused target and
                           the current assignment (coordinator retry rule) any outcome
      [service account probe, only with -ServiceAccountProbe]
      restore-1[,2]        B (always, in a finally block)                  expect success

    The script refuses to change anything unless every safety check passes: the VM
    exists exactly once, uses static memory, is Running, MemoryStartup equals
    MemoryAssigned, its Hyper-V Notes contain the opt-in marker 'hvmc-probe' as a
    separate word, and the host has at least (S + L) + -HostMarginMiB of free memory.
    Name uniqueness, Running state and host free memory are re-checked before every write.

    See docs/spikes/s1-hyperv.md for prerequisites, the output schema and how results
    become fixtures. The JSON Schema is hack/hyperv-characterize.schema.json.

.PARAMETER VMName
    Exact Hyper-V name of the disposable VM (no wildcards).

.PARAMETER OutDir
    Directory for the JSON result. Created if missing. Default: ./hvmc-s1

.PARAMETER StepMiB
    Resize step (hvmc default 256). Must be a multiple of 2 (Hyper-V alignment).

.PARAMETER LargeStepMiB
    Additional growth of the second grow step. Must be a multiple of 2.

.PARAMETER RefuseTargetMiB
    Target of the shrink that the guest is expected to refuse. Must be a multiple of 2,
    at least 32 and below the VM's MemoryStartup.

.PARAMETER HostMarginMiB
    Free host memory that must remain after the largest planned growth (hvmc's default
    host reserve is 4 GiB).

.PARAMETER SettleSeconds
    After each Set-VMMemory call, keep sampling MemoryAssigned/MemoryStartup this long
    to see whether values change after the cmdlet returned. 0 disables sampling.

.PARAMETER GuestCheck
    Measure, through PowerShell Direct, how long until the guest's total physical memory
    (GlobalMemoryStatusEx ullTotalPhys) reflects each resize. Windows guests only.
    Requires -GuestCredential.

.PARAMETER GuestCredential
    Credential of an account inside the guest, used for PowerShell Direct.

.PARAMETER GuestTimeoutSeconds
    How long to wait for the guest to reflect a resize (hvmc default 60).

.PARAMETER ServiceAccountProbe
    Also check whether a virtual service account in Hyper-V Administrators can run
    Get-VM, Get-VMMemory, Get-CimInstance and Set-VMMemory. Creates a temporary service
    'hvmc-s1-probe' running as 'NT SERVICE\hvmc-s1-probe', adds that account to
    Hyper-V Administrators, runs a probe script once, and removes the service, the group
    membership and its work directory again. Requires an elevated session.

.PARAMETER DryRun
    Run the safety checks and print the planned targets; change nothing, write no file.

.EXAMPLE
    pwsh -NoProfile -File hack/hyperv-characterize.ps1 -VMName hvmc-s1-disposable -DryRun

.EXAMPLE
    pwsh -NoProfile -File hack/hyperv-characterize.ps1 -VMName hvmc-s1-disposable -OutDir .\s1 -GuestCheck -GuestCredential (Get-Credential)

.NOTES
    Exit codes: 0 completed (or dry run passed); 1 aborted or internal error;
    2 refused by a safety check; 3 the original MemoryStartup could not be restored.
#>
#Requires -Version 7
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidateNotNullOrEmpty()]
    [ValidateScript({ $_ -notmatch '[\*\?\[\]]' }, ErrorMessage = 'VMName must be an exact VM name; wildcard characters are not allowed.')]
    [string] $VMName,

    [ValidateNotNullOrEmpty()]
    [string] $OutDir = (Join-Path -Path (Get-Location).Path -ChildPath 'hvmc-s1'),

    [ValidateRange(2, 65536)]
    [ValidateScript({ $_ % 2 -eq 0 }, ErrorMessage = 'StepMiB must be a multiple of 2 (Hyper-V requires 2 MiB alignment).')]
    [int] $StepMiB = 256,

    [ValidateRange(2, 1048576)]
    [ValidateScript({ $_ % 2 -eq 0 }, ErrorMessage = 'LargeStepMiB must be a multiple of 2 (Hyper-V requires 2 MiB alignment).')]
    [int] $LargeStepMiB = 1024,

    [ValidateRange(32, 1048576)]
    [ValidateScript({ $_ % 2 -eq 0 }, ErrorMessage = 'RefuseTargetMiB must be a multiple of 2 (Hyper-V requires 2 MiB alignment).')]
    [int] $RefuseTargetMiB = 256,

    [ValidateRange(0, 1048576)]
    [int] $HostMarginMiB = 4096,

    [ValidateRange(0, 300)]
    [int] $SettleSeconds = 5,

    [switch] $GuestCheck,

    [pscredential] $GuestCredential,

    [ValidateRange(1, 600)]
    [int] $GuestTimeoutSeconds = 60,

    [switch] $ServiceAccountProbe,

    [switch] $DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
# Native tools (sc.exe, net.exe, icacls.exe) report through exit codes; check them explicitly.
$PSNativeCommandUseErrorActionPreference = $false

# ---------------------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------------------

$script:SchemaVersion = 1
$script:DocumentKind = 'hvmc.s1.hyperv-characterization'
$script:ScriptVersion = '1'
$script:ProbeMarker = 'hvmc-probe'
$script:VmNamePlaceholder = 'vm-under-test'
$script:VmIdPlaceholder = '00000000-0000-4000-8000-000000000001'
$script:HostPlaceholder = 'hyperv-host'
# A fixed GUID that no VM has; used to record the "VM not found" error shape.
$script:MissingVmId = [guid]'00000000-0000-4000-8000-0000000000ff'
$script:HyperVAdministratorsSid = 'S-1-5-32-578'
$script:HyperVMinimumStartupBytes = 32MB
$script:AlignmentBytes = 2MB
$script:PartialShrinkMaxAttempts = 3
$script:ProbeServiceName = 'hvmc-s1-probe'
$script:ProbeAccount = 'NT SERVICE\hvmc-s1-probe'
$script:VMShapeProperties = @(
    'Name', 'Id', 'State', 'Status', 'MemoryAssigned', 'MemoryStartup', 'MemoryDemand',
    'MemoryMinimum', 'MemoryMaximum', 'DynamicMemoryEnabled', 'Generation', 'Version',
    'Uptime', 'IsClustered'
)
$script:VMMemoryShapeProperties = @(
    'VMName', 'VMId', 'Startup', 'Minimum', 'Maximum', 'Buffer', 'Priority',
    'DynamicMemoryEnabled', 'MaximumPerNumaNode'
)
# Deliberately excludes CSName, RegisteredUser, Organization and SerialNumber.
$script:OSShapeProperties = @(
    'Caption', 'Version', 'BuildNumber', 'TotalVisibleMemorySize', 'FreePhysicalMemory'
)

# ---------------------------------------------------------------------------------------
# Pure helpers (covered by hack/tests without Hyper-V)
# ---------------------------------------------------------------------------------------

function Get-UtcTimestamp {
    [DateTime]::UtcNow.ToString('o')
}

function Get-ElapsedMs {
    param([Parameter(Mandatory)] [System.Diagnostics.Stopwatch] $Stopwatch)
    [long][math]::Round($Stopwatch.Elapsed.TotalMilliseconds)
}

function ConvertTo-PlainValue {
    # Converts a property value into something ConvertTo-Json renders predictably:
    # enums and GUIDs as strings, dates as ISO 8601 UTC, time spans as milliseconds.
    param([AllowNull()] $Value)
    if ($null -eq $Value) { return $null }
    if ($Value -is [string] -or $Value -is [bool]) { return $Value }
    if ($Value -is [enum] -or $Value -is [guid] -or $Value -is [version]) { return $Value.ToString() }
    if ($Value -is [datetime]) { return $Value.ToUniversalTime().ToString('o') }
    if ($Value -is [timespan]) { return [long][math]::Round($Value.TotalMilliseconds) }
    if ($Value.GetType().IsPrimitive -or $Value -is [decimal]) { return $Value }
    return $Value.ToString()
}

function Get-ObjectShape {
    # Records type names and the listed properties (type + plain value) of a cmdlet result,
    # so the CI mock module can return objects with the same shape.
    param(
        [AllowNull()] $InputObject,
        [Parameter(Mandatory)] [string[]] $Property
    )
    if ($null -eq $InputObject) { return $null }
    $properties = [ordered]@{}
    foreach ($name in $Property) {
        $p = $InputObject.PSObject.Properties[$name]
        if ($null -eq $p) {
            $properties[$name] = [ordered]@{ present = $false; type = $null; value = $null }
            continue
        }
        try {
            $value = $p.Value
        } catch {
            $properties[$name] = [ordered]@{ present = $true; type = $null; value = $null; readError = $_.Exception.Message }
            continue
        }
        $typeName = $null
        if ($null -ne $value) { $typeName = $value.GetType().FullName }
        $properties[$name] = [ordered]@{ present = $true; type = $typeName; value = (ConvertTo-PlainValue $value) }
    }
    [ordered]@{
        type        = $InputObject.GetType().FullName
        psTypeNames = @($InputObject.PSObject.TypeNames)
        properties  = $properties
    }
}

function ConvertTo-ExceptionInfo {
    param([Parameter(Mandatory)] [Exception] $Exception)
    $skip = @(
        'Message', 'StackTrace', 'Source', 'HelpLink', 'TargetSite', 'Data', 'InnerException',
        'InnerExceptions', 'HResult', 'ErrorRecord', 'WasThrownFromThrowStatement'
    )
    # Exception-specific scalar properties (e.g. Hyper-V error codes) help classification.
    $extra = [ordered]@{}
    foreach ($prop in $Exception.GetType().GetProperties()) {
        if ($prop.Name -in $skip -or $prop.GetIndexParameters().Count -gt 0) { continue }
        try { $v = $prop.GetValue($Exception) } catch { continue }
        if ($null -eq $v) { $extra[$prop.Name] = $null; continue }
        if ($v -is [string] -or $v -is [enum] -or $v -is [guid] -or $v.GetType().IsPrimitive) {
            $extra[$prop.Name] = ConvertTo-PlainValue $v
        }
    }
    [ordered]@{
        type       = $Exception.GetType().FullName
        message    = $Exception.Message
        hResult    = $Exception.HResult
        properties = $extra
    }
}

function ConvertTo-ErrorInfo {
    # The full, serializable error record: what the host agent's classifier will see.
    param([Parameter(Mandatory)] [System.Management.Automation.ErrorRecord] $ErrorRecord)
    $inner = [System.Collections.Generic.List[object]]::new()
    $e = $ErrorRecord.Exception.InnerException
    while ($null -ne $e -and $inner.Count -lt 8) {
        $inner.Add((ConvertTo-ExceptionInfo -Exception $e))
        $e = $e.InnerException
    }
    $ci = $ErrorRecord.CategoryInfo
    $details = $null
    if ($null -ne $ErrorRecord.ErrorDetails) { $details = $ErrorRecord.ErrorDetails.Message }
    $targetType = $null
    if ($null -ne $ErrorRecord.TargetObject) { $targetType = $ErrorRecord.TargetObject.GetType().FullName }
    [ordered]@{
        fullyQualifiedErrorId = $ErrorRecord.FullyQualifiedErrorId
        exception             = ConvertTo-ExceptionInfo -Exception $ErrorRecord.Exception
        innerExceptions       = $inner
        category              = [ordered]@{
            category   = $ci.Category.ToString()
            activity   = $ci.Activity
            reason     = $ci.Reason
            targetName = $ci.TargetName
            targetType = $ci.TargetType
        }
        errorDetailsMessage   = $details
        targetObjectType      = $targetType
    }
}

function Get-DistinctErrorInfo {
    # Merges -ErrorVariable contents and the caught record; drops the
    # ActionPreferenceStopException entries that -ErrorAction Stop adds.
    param([AllowNull()] [object[]] $Records)
    $out = [System.Collections.Generic.List[object]]::new()
    $seen = [System.Collections.Generic.HashSet[string]]::new()
    foreach ($r in @($Records)) {
        if ($r -isnot [System.Management.Automation.ErrorRecord]) { continue }
        $key = '{0}|{1}' -f $r.FullyQualifiedErrorId, $r.Exception.Message
        if ($seen.Add($key)) { $out.Add((ConvertTo-ErrorInfo -ErrorRecord $r)) }
    }
    , $out
}

function Test-ProbeMarker {
    # The opt-in marker must appear as its own word (case-sensitive) in the VM's Notes.
    param([AllowNull()] [AllowEmptyString()] [string] $Notes)
    if ([string]::IsNullOrEmpty($Notes)) { return $false }
    return $Notes -cmatch ('(?<![\w-])' + [regex]::Escape($script:ProbeMarker) + '(?![\w-])')
}

function Get-MidpointTarget {
    # Coordinator retry rule (DESIGN 4.2): midpoint between the refused target and the
    # current assignment, quantized up to the step, falling back to 2 MiB alignment.
    # Returns $null when no target strictly between the two exists.
    param(
        [Parameter(Mandatory)] [long] $RefusedTargetBytes,
        [Parameter(Mandatory)] [long] $CurrentAssignedBytes,
        [Parameter(Mandatory)] [long] $StepBytes
    )
    if ($CurrentAssignedBytes -le $RefusedTargetBytes) { return $null }
    $mid = [math]::Floor(($RefusedTargetBytes + $CurrentAssignedBytes) / 2)
    foreach ($q in @($StepBytes, $script:AlignmentBytes)) {
        $candidate = [long]([math]::Ceiling($mid / $q) * $q)
        if ($candidate -gt $RefusedTargetBytes -and $candidate -lt $CurrentAssignedBytes) { return $candidate }
    }
    return $null
}

function New-Plan {
    param(
        [Parameter(Mandatory)] [long] $BaselineBytes,
        [Parameter(Mandatory)] [long] $StepBytes,
        [Parameter(Mandatory)] [long] $LargeStepBytes,
        [Parameter(Mandatory)] [long] $RefuseTargetBytes
    )
    [ordered]@{
        baselineBytes            = $BaselineBytes
        stepBytes                = $StepBytes
        largeStepBytes           = $LargeStepBytes
        unalignedTargetBytes     = $BaselineBytes + 1MB
        growStepTargetBytes      = $BaselineBytes + $StepBytes
        growLargeTargetBytes     = $BaselineBytes + $StepBytes + $LargeStepBytes
        refuseTargetBytes        = $RefuseTargetBytes
        partialShrinkMaxAttempts = $script:PartialShrinkMaxAttempts
        peakBytes                = $BaselineBytes + $StepBytes + $LargeStepBytes
        plannedGrowBytes         = $StepBytes + $LargeStepBytes
    }
}

function New-Check {
    param([string] $Name, [bool] $Passed, [string] $Detail)
    [ordered]@{ name = $Name; passed = $Passed; detail = $Detail }
}

function Format-MiB {
    param([AllowNull()] $Bytes)
    if ($null -eq $Bytes) { return 'n/a' }
    '{0:0.#} MiB' -f ([double]$Bytes / 1MB)
}

function Test-Safety {
    # Pure: evaluates the facts gathered by Get-SafetyFactSet. Every check is reported;
    # checks that need a unique VM are skipped when there is none.
    param(
        [Parameter(Mandatory)] [System.Collections.IDictionary] $Facts,
        [AllowNull()] [System.Collections.IDictionary] $Plan,
        [Parameter(Mandatory)] [long] $MarginBytes,
        [switch] $GuestCheck,
        [switch] $HasGuestCredential,
        [switch] $ServiceAccountProbe
    )
    $checks = [System.Collections.Generic.List[object]]::new()
    $checks.Add((New-Check 'hyperv-available' ([bool]$Facts.hyperV.available) $Facts.hyperV.detail))
    if (-not $Facts.hyperV.available) { return , $checks }

    $count = [int]$Facts.vmCount
    $checks.Add((New-Check 'vm-unique' ($count -eq 1) ("{0} VM(s) named exactly as -VMName (0 also means no permission to read it)" -f $count)))
    if ($count -eq 1) {
        $vm = $Facts.vm
        $state = "$($vm.State)"
        $checks.Add((New-Check 'vm-running' ($state -eq 'Running') "state is $state"))

        $dynamic = $true
        if ($null -ne $Facts.memory) { $dynamic = [bool]$Facts.memory.DynamicMemoryEnabled }
        $checks.Add((New-Check 'static-memory' (-not $dynamic) "DynamicMemoryEnabled is $dynamic"))

        $marker = Test-ProbeMarker -Notes $vm.Notes
        $checks.Add((New-Check 'opt-in-marker' $marker "Notes contain '$($script:ProbeMarker)' as a separate word: $marker"))

        $startup = [long]$vm.MemoryStartup
        $assigned = [long]$vm.MemoryAssigned
        $consistent = ($startup -eq $assigned) -and ($startup % $script:AlignmentBytes -eq 0)
        $checks.Add((New-Check 'startup-matches-assigned' $consistent ("MemoryStartup {0}, MemoryAssigned {1}; they must be equal and 2 MiB aligned (a difference means an earlier resize left the VM inconsistent)" -f (Format-MiB $startup), (Format-MiB $assigned))))

        if ($null -ne $Plan) {
            $r = [long]$Plan.refuseTargetBytes
            $okTarget = ($r -ge $script:HyperVMinimumStartupBytes) -and ($r -lt $startup) -and ($r % $script:AlignmentBytes -eq 0)
            $checks.Add((New-Check 'refuse-target' $okTarget ("refused-shrink target {0} must be >= 32 MiB, 2 MiB aligned and below MemoryStartup {1}" -f (Format-MiB $r), (Format-MiB $startup))))
        }
    }

    if ($null -ne $Plan) {
        $need = [long]$Plan.plannedGrowBytes + $MarginBytes
        $free = $Facts.hostFreeBytes
        $okFree = ($null -ne $free) -and ([long]$free -ge $need)
        $checks.Add((New-Check 'host-free-memory' $okFree ("host free {0}, required {1} (planned growth {2} + margin {3})" -f (Format-MiB $free), (Format-MiB $need), (Format-MiB $Plan.plannedGrowBytes), (Format-MiB $MarginBytes))))
    }

    if ($GuestCheck) {
        $checks.Add((New-Check 'guest-credential' ([bool]$HasGuestCredential) '-GuestCheck requires -GuestCredential'))
    }
    if ($ServiceAccountProbe) {
        $checks.Add((New-Check 'elevated' ([bool]$Facts.isElevated) '-ServiceAccountProbe requires an elevated (Administrator) session'))
        $checks.Add((New-Check 'probe-service-absent' (-not [bool]$Facts.probeServiceExists) ("service '{0}' must not exist (leftover from an interrupted run? see docs/spikes/s1-hyperv.md)" -f $script:ProbeServiceName)))
        $checks.Add((New-Check 'probe-dir-absent' (-not [bool]$Facts.probeDirExists) ("probe work directory '{0}' must not exist" -f (Get-ProbeWorkDir))))
    }
    , $checks
}

function Get-ObservedOutcome {
    # Classifies one resize as the mock module needs it.
    param(
        [Parameter(Mandatory)] [bool] $Succeeded,
        [Parameter(Mandatory)] [long] $TargetBytes,
        [Parameter(Mandatory)] [long] $AssignedBeforeBytes,
        [Parameter(Mandatory)] [long] $AssignedAfterBytes
    )
    if ($Succeeded) {
        if ($AssignedAfterBytes -eq $TargetBytes) { return 'applied' }
        return 'succeeded-not-applied'
    }
    if ($AssignedAfterBytes -eq $AssignedBeforeBytes) { return 'refused' }
    return 'partially-applied'
}

function Test-Expectation {
    param([string] $Expected, [bool] $Succeeded)
    switch ($Expected) {
        'success' { return $Succeeded }
        'refusal' { return -not $Succeeded }
        default { return $true }
    }
}

# ---------------------------------------------------------------------------------------
# Anonymisation
# ---------------------------------------------------------------------------------------

function New-RedactionRule {
    param([Parameter(Mandatory)] [string] $Literal, [Parameter(Mandatory)] [AllowEmptyString()] [string] $Replacement)
    # Word-ish boundaries so a short name does not rewrite parts of unrelated words.
    $pattern = '(?<![\p{L}\p{N}_-])' + [regex]::Escape($Literal) + '(?![\p{L}\p{N}_-])'
    [pscustomobject]@{
        Literal     = $Literal
        Replacement = $Replacement
        Regex       = [regex]::new($pattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase -bor [System.Text.RegularExpressions.RegexOptions]::CultureInvariant)
    }
}

function New-RedactionRuleSet {
    param(
        [AllowNull()] [string] $VMName,
        [AllowNull()] [Nullable[guid]] $VMId,
        [AllowNull()] [string[]] $HostNames
    )
    $pairs = [System.Collections.Generic.List[object]]::new()
    if ($null -ne $VMId) {
        $id = [guid]$VMId
        $pairs.Add([pscustomobject]@{ Literal = $id.ToString('D'); Replacement = $script:VmIdPlaceholder })
        $pairs.Add([pscustomobject]@{ Literal = $id.ToString('N'); Replacement = $script:VmIdPlaceholder.Replace('-', '') })
    }
    if (-not [string]::IsNullOrEmpty($VMName)) { $pairs.Add([pscustomobject]@{ Literal = $VMName; Replacement = $script:VmNamePlaceholder }) }
    foreach ($h in @($HostNames)) {
        if (-not [string]::IsNullOrWhiteSpace($h)) { $pairs.Add([pscustomobject]@{ Literal = $h; Replacement = $script:HostPlaceholder }) }
    }
    # Longest first, so an FQDN is replaced before its host label.
    $sorted = @($pairs | Sort-Object -Property { $_.Literal.Length } -Descending)
    $rules = foreach ($p in $sorted) { New-RedactionRule -Literal $p.Literal -Replacement $p.Replacement }
    , @($rules)
}

function Protect-Document {
    # Returns a copy of the document tree with every string passed through the rules.
    param([AllowNull()] $InputObject, [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]] $Rules)
    if ($null -eq $InputObject) { return $null }
    if ($InputObject -is [string]) {
        $s = $InputObject
        foreach ($r in $Rules) { $s = $r.Regex.Replace($s, $r.Replacement) }
        return $s
    }
    if ($InputObject -is [System.Collections.IDictionary]) {
        $copy = [ordered]@{}
        foreach ($k in @($InputObject.Keys)) { $copy[$k] = Protect-Document -InputObject $InputObject[$k] -Rules $Rules }
        return $copy
    }
    if ($InputObject -is [System.Collections.IEnumerable]) {
        $items = [System.Collections.Generic.List[object]]::new()
        foreach ($i in $InputObject) { $items.Add((Protect-Document -InputObject $i -Rules $Rules)) }
        return , $items
    }
    return $InputObject
}

function Find-RedactionLeak {
    # Checks the serialized JSON for any original value (also in its JSON-escaped form).
    param([Parameter(Mandatory)] [string] $Json, [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]] $Rules)
    $leaks = [System.Collections.Generic.List[string]]::new()
    foreach ($r in $Rules) {
        $escaped = (ConvertTo-Json -InputObject $r.Literal -Compress).Trim('"')
        $variants = @(@($r.Literal, $escaped) | Select-Object -Unique)
        foreach ($variant in $variants) {
            if ((New-RedactionRule -Literal $variant -Replacement '').Regex.IsMatch($Json)) {
                $leaks.Add($r.Replacement)
                break
            }
        }
    }
    , $leaks
}

# ---------------------------------------------------------------------------------------
# Environment probes (mocked in tests)
# ---------------------------------------------------------------------------------------

function Test-HyperVAvailable {
    if (-not $IsWindows) {
        return [ordered]@{ available = $false; detail = 'not running on Windows'; moduleVersion = $null }
    }
    $missing = foreach ($name in 'Get-VM', 'Get-VMMemory', 'Set-VMMemory') {
        $cmd = Get-Command -Name $name -ErrorAction SilentlyContinue
        if ($null -eq $cmd -or $cmd.Source -ne 'Hyper-V') { $name }
    }
    if (@($missing).Count -gt 0) {
        return [ordered]@{ available = $false; detail = ('not resolved from the Hyper-V module: {0}' -f (@($missing) -join ', ')); moduleVersion = $null }
    }
    $module = Get-Module -Name Hyper-V | Sort-Object -Property Version -Descending | Select-Object -First 1
    $version = $null
    if ($null -ne $module) { $version = $module.Version.ToString() }
    [ordered]@{ available = $true; detail = 'Hyper-V module loaded'; moduleVersion = $version }
}

function Test-IsElevated {
    if (-not $IsWindows) { return $false }
    $principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
    $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Test-TokenHasHyperVAdministrators {
    if (-not $IsWindows) { return $false }
    $groups = [Security.Principal.WindowsIdentity]::GetCurrent().Groups
    [bool](@($groups | Where-Object { $_.Value -eq $script:HyperVAdministratorsSid }).Count)
}

function Get-HostNameList {
    $names = [System.Collections.Generic.List[string]]::new()
    $names.Add([Environment]::MachineName)
    try { $names.Add([System.Net.Dns]::GetHostName()) } catch { Write-Verbose "no DNS host name: $_" }
    try { $names.Add([System.Net.Dns]::GetHostEntry([Environment]::MachineName).HostName) } catch { Write-Verbose "no FQDN: $_" }
    , @($names | Where-Object { -not [string]::IsNullOrWhiteSpace($_) -and $_ -ne 'localhost' } | Select-Object -Unique)
}

function Get-ProbeWorkDir {
    $base = $env:ProgramData
    if ([string]::IsNullOrEmpty($base)) { $base = [System.IO.Path]::GetTempPath() }
    Join-Path -Path $base -ChildPath $script:ProbeServiceName
}

function Test-ProbeServiceExists {
    if (-not $IsWindows) { return $false }
    $null -ne (Get-Service -Name $script:ProbeServiceName -ErrorAction SilentlyContinue)
}

function Get-HostMemory {
    $os = Get-CimInstance -ClassName Win32_OperatingSystem
    [ordered]@{
        os         = $os
        totalBytes = [long]$os.TotalVisibleMemorySize * 1KB
        freeBytes  = [long]$os.FreePhysicalMemory * 1KB
    }
}

function Get-SafetyFactSet {
    param(
        [Parameter(Mandatory)] [string] $VMName,
        [switch] $ServiceAccountProbe
    )
    $facts = [ordered]@{
        hyperV             = Test-HyperVAvailable
        vmCount            = 0
        vm                 = $null
        memory             = $null
        os                 = $null
        hostTotalBytes     = $null
        hostFreeBytes      = $null
        isElevated         = $null
        probeServiceExists = $null
        probeDirExists     = $null
    }
    if ($ServiceAccountProbe) {
        $facts.isElevated = Test-IsElevated
        $facts.probeServiceExists = Test-ProbeServiceExists
        $facts.probeDirExists = Test-Path -LiteralPath (Get-ProbeWorkDir)
    }
    if (-not $facts.hyperV.available) { return $facts }

    $vms = @(Get-VM -Name $VMName -ErrorAction SilentlyContinue | Where-Object { $_.Name -eq $VMName })
    $facts.vmCount = $vms.Count
    if ($vms.Count -eq 1) {
        $facts.vm = $vms[0]
        $facts.memory = Get-VMMemory -VM $vms[0]
    }
    $hostMemory = Get-HostMemory
    $facts.os = $hostMemory.os
    $facts.hostTotalBytes = $hostMemory.totalBytes
    $facts.hostFreeBytes = $hostMemory.freeBytes
    $facts
}

# ---------------------------------------------------------------------------------------
# Hyper-V reads and writes
# ---------------------------------------------------------------------------------------

function Get-VMMemoryState {
    param([Parameter(Mandatory)] [guid] $VMId)
    $vm = Get-VM -Id $VMId
    $memory = Get-VMMemory -VM $vm
    [ordered]@{
        observedAt           = Get-UtcTimestamp
        state                = "$($vm.State)"
        status               = "$($vm.Status)"
        assignedBytes        = [long]$vm.MemoryAssigned
        startupBytes         = [long]$vm.MemoryStartup
        vmMemoryStartupBytes = [long]$memory.Startup
    }
}

function Assert-SafeToWrite {
    # Re-checked before every write: the name still resolves to exactly our VM, it is
    # still running, and growth leaves the host margin intact.
    param(
        [Parameter(Mandatory)] [string] $VMName,
        [Parameter(Mandatory)] [guid] $VMId,
        [Parameter(Mandatory)] [long] $TargetBytes,
        [Parameter(Mandatory)] [long] $MarginBytes,
        [switch] $Restore
    )
    $vms = @(Get-VM -Name $VMName -ErrorAction SilentlyContinue | Where-Object { $_.Name -eq $VMName })
    if ($vms.Count -ne 1 -or $vms[0].Id -ne $VMId) {
        throw "-VMName no longer resolves to exactly the VM under test ($($vms.Count) match(es)); refusing to write."
    }
    if ($Restore) { return }
    $state = "$($vms[0].State)"
    if ($state -ne 'Running') { throw "VM under test is no longer Running (state $state); stopping the sequence." }
    $growth = $TargetBytes - [long]$vms[0].MemoryAssigned
    if ($growth -gt 0) {
        $free = (Get-HostMemory).freeBytes
        if ($free -lt $growth + $MarginBytes) {
            throw ("host free memory {0} is below growth {1} + margin {2}; stopping the sequence." -f (Format-MiB $free), (Format-MiB $growth), (Format-MiB $MarginBytes))
        }
    }
}

function Measure-Settle {
    param(
        [Parameter(Mandatory)] [guid] $VMId,
        [Parameter(Mandatory)] [System.Collections.IDictionary] $Initial,
        [Parameter(Mandatory)] [int] $Seconds,
        [Parameter(Mandatory)] [System.Diagnostics.Stopwatch] $SinceReturn
    )
    $changes = [System.Collections.Generic.List[object]]::new()
    $last = $Initial
    $lastChangeMs = $null
    if ($Seconds -gt 0) {
        $deadline = $Seconds * 1000
        do {
            Start-Sleep -Milliseconds 500
            $s = Get-VMMemoryState -VMId $VMId
            if ($s.assignedBytes -ne $last.assignedBytes -or $s.startupBytes -ne $last.startupBytes -or $s.vmMemoryStartupBytes -ne $last.vmMemoryStartupBytes) {
                $lastChangeMs = Get-ElapsedMs $SinceReturn
                $changes.Add([ordered]@{ atMs = $lastChangeMs; assignedBytes = $s.assignedBytes; startupBytes = $s.startupBytes; vmMemoryStartupBytes = $s.vmMemoryStartupBytes })
            }
            $last = $s
        } while ((Get-ElapsedMs $SinceReturn) -lt $deadline)
    }
    [ordered]@{
        windowMs             = $Seconds * 1000
        lastChangeAtMs       = $lastChangeMs
        changes              = $changes
        state                = $last.state
        assignedBytes        = $last.assignedBytes
        startupBytes         = $last.startupBytes
        vmMemoryStartupBytes = $last.vmMemoryStartupBytes
    }
}

function Invoke-ReadErrorStep {
    # Records the error shape of reading a VM that does not exist (expected: not found).
    param([Parameter(Mandatory)] [int] $Index)
    $record = [ordered]@{
        index = $Index; name = 'get-vm-missing'; operation = 'Get-VM'; direction = 'none'; expected = 'refusal'
        targetBytes = $null; startedAt = (Get-UtcTimestamp); durationMs = $null; result = $null; observed = $null
        matchesExpectation = $null; startupDiverged = $null; before = $null; after = $null; settled = $null
        errors = $null; warnings = @(); guest = $null
    }
    $stepErrors = $null
    $caught = $null
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $null = Get-VM -Id $script:MissingVmId -ErrorAction Stop -ErrorVariable stepErrors
        $ok = $true
    } catch {
        $ok = $false
        $caught = $_
    }
    $sw.Stop()
    $record.durationMs = Get-ElapsedMs $sw
    $record.result = if ($ok) { 'succeeded' } else { 'failed' }
    $record.observed = if ($ok) { 'read-ok' } else { 'read-failed' }
    $record.matchesExpectation = Test-Expectation -Expected 'refusal' -Succeeded $ok
    $record.errors = Get-DistinctErrorInfo -Records (@($stepErrors) + @($caught))
    $record
}

function Invoke-ResizeStep {
    param(
        [Parameter(Mandatory)] [int] $Index,
        [Parameter(Mandatory)] [string] $Name,
        [Parameter(Mandatory)] [string] $VMName,
        [Parameter(Mandatory)] [guid] $VMId,
        [Parameter(Mandatory)] [long] $TargetBytes,
        [Parameter(Mandatory)] [ValidateSet('success', 'refusal', 'any')] [string] $Expected,
        [Parameter(Mandatory)] [int] $SettleSeconds,
        [Parameter(Mandatory)] [long] $StepBytes,
        [AllowNull()] [System.Collections.IDictionary] $Guest
    )
    $before = Get-VMMemoryState -VMId $VMId
    $direction = 'none'
    if ($TargetBytes -gt $before.assignedBytes) { $direction = 'grow' } elseif ($TargetBytes -lt $before.assignedBytes) { $direction = 'shrink' }
    $record = [ordered]@{
        index = $Index; name = $Name; operation = 'Set-VMMemory'; direction = $direction; expected = $Expected
        targetBytes = $TargetBytes; startedAt = (Get-UtcTimestamp); durationMs = $null; result = $null; observed = $null
        matchesExpectation = $null; startupDiverged = $null; before = $before; after = $null; settled = $null
        errors = $null; warnings = $null; guest = $null
    }

    $stepErrors = $null
    $stepWarnings = $null
    $caught = $null
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        # Same call shape as the host agent's pwsh worker (DESIGN 4.2).
        Set-VMMemory -VMName $VMName -StartupBytes $TargetBytes -ErrorAction Stop -ErrorVariable stepErrors -WarningAction SilentlyContinue -WarningVariable stepWarnings
        $ok = $true
    } catch {
        $ok = $false
        $caught = $_
    }
    $sw.Stop()
    $sinceReturn = [System.Diagnostics.Stopwatch]::StartNew()
    $record.durationMs = Get-ElapsedMs $sw
    $record.result = if ($ok) { 'succeeded' } else { 'failed' }
    $record.errors = Get-DistinctErrorInfo -Records (@($stepErrors) + @($caught))
    $record.warnings = @(@($stepWarnings) | Where-Object { $null -ne $_ } | ForEach-Object { "$($_.Message)" })

    $after = Get-VMMemoryState -VMId $VMId
    $record.after = $after

    if ($null -ne $Guest -and $null -ne $Guest.session -and $after.assignedBytes -ne $before.assignedBytes) {
        $record.guest = Wait-GuestReflect -Guest $Guest -AssignedBytes $after.assignedBytes -Grow ($after.assignedBytes -gt $before.assignedBytes) -StepBytes $StepBytes -CmdletDurationMs $record.durationMs -SinceReturn $sinceReturn
    }

    $settled = Measure-Settle -VMId $VMId -Initial $after -Seconds $SettleSeconds -SinceReturn $sinceReturn
    $record.settled = $settled
    $record.observed = Get-ObservedOutcome -Succeeded $ok -TargetBytes $TargetBytes -AssignedBeforeBytes $before.assignedBytes -AssignedAfterBytes $settled.assignedBytes
    $record.matchesExpectation = Test-Expectation -Expected $Expected -Succeeded $ok
    # DESIGN 3.5: a failed shrink may change MemoryStartup although the assignment did not follow.
    $record.startupDiverged = ($settled.startupBytes -ne $settled.assignedBytes)
    $record
}

# ---------------------------------------------------------------------------------------
# Guest check (PowerShell Direct; Windows guests)
# ---------------------------------------------------------------------------------------

$script:GuestInitScript = {
    if (-not ('HvmcS1.GuestMemory' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace HvmcS1 {
    public static class GuestMemory {
        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Auto)]
        private class MEMORYSTATUSEX {
            public uint dwLength;
            public uint dwMemoryLoad;
            public ulong ullTotalPhys;
            public ulong ullAvailPhys;
            public ulong ullTotalPageFile;
            public ulong ullAvailPageFile;
            public ulong ullTotalVirtual;
            public ulong ullAvailVirtual;
            public ulong ullAvailExtendedVirtual;
            public MEMORYSTATUSEX() { dwLength = (uint)Marshal.SizeOf(typeof(MEMORYSTATUSEX)); }
        }
        [DllImport("kernel32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool GlobalMemoryStatusEx([In, Out] MEMORYSTATUSEX buffer);
        public static ulong TotalPhys() {
            MEMORYSTATUSEX m = new MEMORYSTATUSEX();
            if (!GlobalMemoryStatusEx(m)) { throw new System.ComponentModel.Win32Exception(); }
            return m.ullTotalPhys;
        }
    }
}
'@
    }
}

function New-GuestSession {
    param([Parameter(Mandatory)] [guid] $VMId, [Parameter(Mandatory)] [pscredential] $Credential)
    $session = New-PSSession -VMId $VMId -Credential $Credential
    Invoke-Command -Session $session -ScriptBlock $script:GuestInitScript
    $session
}

function Get-GuestTotalBytes {
    param([Parameter(Mandatory)] $Session)
    [long](Invoke-Command -Session $Session -ScriptBlock { [HvmcS1.GuestMemory]::TotalPhys() })
}

function Remove-GuestSession {
    param([AllowNull()] $Session)
    if ($null -ne $Session) { Remove-PSSession -Session $Session }
}

function Wait-GuestReflect {
    # Verification rule from DESIGN 4.4: grown when guest total >= assigned - offset - step/2
    # (shrink: <= assigned - offset + step/2). Time is measured from the cmdlet's return.
    param(
        [Parameter(Mandatory)] [System.Collections.IDictionary] $Guest,
        [Parameter(Mandatory)] [long] $AssignedBytes,
        [Parameter(Mandatory)] [bool] $Grow,
        [Parameter(Mandatory)] [long] $StepBytes,
        [Parameter(Mandatory)] [long] $CmdletDurationMs,
        [Parameter(Mandatory)] [System.Diagnostics.Stopwatch] $SinceReturn
    )
    $expected = $AssignedBytes - [long]$Guest.offsetBytes
    $tolerance = [long]($StepBytes / 2)
    $rec = [ordered]@{
        expectedTotalBytes = $expected; toleranceBytes = $tolerance; direction = $(if ($Grow) { 'grow' } else { 'shrink' })
        reached = $false; reflectedAfterMs = $null; reflectedAfterCmdletStartMs = $null; finalTotalBytes = $null
        polls = 0; timeoutMs = [long]$Guest.timeoutSeconds * 1000; error = $null
    }
    try {
        do {
            $total = Get-GuestTotalBytes -Session $Guest.session
            $rec.polls++
            $rec.finalTotalBytes = $total
            $hit = if ($Grow) { $total -ge $expected - $tolerance } else { $total -le $expected + $tolerance }
            if ($hit) {
                $rec.reached = $true
                $rec.reflectedAfterMs = Get-ElapsedMs $SinceReturn
                $rec.reflectedAfterCmdletStartMs = $rec.reflectedAfterMs + $CmdletDurationMs
                break
            }
            Start-Sleep -Milliseconds 250
        } while ((Get-ElapsedMs $SinceReturn) -lt $rec.timeoutMs)
    } catch {
        $rec.error = ConvertTo-ErrorInfo -ErrorRecord $_
    }
    $rec
}

# ---------------------------------------------------------------------------------------
# Service account probe (-ServiceAccountProbe)
# ---------------------------------------------------------------------------------------

$script:ProbeBody = @'
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$dir = Split-Path -Parent $PSCommandPath
$outputPath = Join-Path $dir 'result.json'
$in = Get-Content -LiteralPath (Join-Path $dir 'input.json') -Raw | ConvertFrom-Json
$result = [ordered]@{
    complete   = $false
    startedAt  = [DateTime]::UtcNow.ToString('o')
    psVersion  = $PSVersionTable.PSVersion.ToString()
    identity   = $null
    operations = [System.Collections.Generic.List[object]]::new()
}
function Save-Result { ConvertTo-Json -InputObject $result -Depth 12 | Set-Content -LiteralPath $outputPath -Encoding utf8 }
try {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $result.identity = [ordered]@{
        name                         = $id.Name
        isSystem                     = $id.IsSystem
        isAdministrator              = ([Security.Principal.WindowsPrincipal]::new($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
        tokenHasHyperVAdministrators = [bool](@($id.Groups | Where-Object { $_.Value -eq 'S-1-5-32-578' }).Count)
    }
} catch {
    $result.identity = [ordered]@{ error = $_.Exception.Message }
}
Save-Result
function Invoke-Op {
    param([string] $Name, [scriptblock] $Body)
    $rec = [ordered]@{ name = $Name; result = $null; durationMs = $null; summary = $null; errors = @() }
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $rec.summary = & $Body
        $rec.result = 'succeeded'
    } catch {
        $rec.result = 'failed'
        $rec.errors = @(ConvertTo-ErrorInfo -ErrorRecord $_)
    }
    $rec.durationMs = [long][math]::Round($sw.Elapsed.TotalMilliseconds)
    $result.operations.Add($rec)
    Save-Result
    $rec
}
$null = Invoke-Op 'Import-Module Hyper-V' { Import-Module -Name Hyper-V; [ordered]@{ version = (Get-Module -Name Hyper-V).Version.ToString() } }
$getVm = Invoke-Op 'Get-VM' {
    $v = @(Get-VM -Name $in.vmName)
    [ordered]@{ vmCount = $v.Count; state = "$($v[0].State)"; assignedBytes = [long]$v[0].MemoryAssigned; startupBytes = [long]$v[0].MemoryStartup }
}
$getMem = Invoke-Op 'Get-VMMemory' {
    $m = Get-VMMemory -VMName $in.vmName
    [ordered]@{ startupBytes = [long]$m.Startup; dynamicMemoryEnabled = [bool]$m.DynamicMemoryEnabled }
}
$null = Invoke-Op 'Get-CimInstance Win32_OperatingSystem' {
    $os = Get-CimInstance -ClassName Win32_OperatingSystem
    [ordered]@{ freePhysicalMemoryBytes = [long]$os.FreePhysicalMemory * 1KB }
}
if ($getVm.result -eq 'succeeded' -and $getMem.result -eq 'succeeded' -and $getVm.summary.vmCount -eq 1) {
    $original = [long]$getMem.summary.startupBytes
    $grow = Invoke-Op 'Set-VMMemory grow' {
        Set-VMMemory -VMName $in.vmName -StartupBytes ($original + [long]$in.stepBytes)
        [ordered]@{ targetBytes = $original + [long]$in.stepBytes }
    }
    if ($grow.result -eq 'succeeded') {
        $null = Invoke-Op 'Set-VMMemory shrink back' {
            Set-VMMemory -VMName $in.vmName -StartupBytes $original
            [ordered]@{ targetBytes = $original }
        }
    }
}
$result.complete = $true
$result.finishedAt = [DateTime]::UtcNow.ToString('o')
Save-Result
'@

function New-ProbeScript {
    # The probe runs in a separate process as the virtual account; it carries its own copy
    # of the error-record helpers so its records have the same shape as the main run's.
    $parts = foreach ($name in 'ConvertTo-PlainValue', 'ConvertTo-ExceptionInfo', 'ConvertTo-ErrorInfo') {
        "function $name {`n$((Get-Item -LiteralPath "function:$name").ScriptBlock.ToString())`n}`n"
    }
    ($parts -join "`n") + "`n" + $script:ProbeBody
}

function Invoke-NativeTool {
    param([Parameter(Mandatory)] [string] $FilePath, [Parameter(Mandatory)] [string[]] $ArgumentList)
    $ErrorActionPreference = 'Continue'
    $output = & $FilePath @ArgumentList 2>&1 | Out-String
    [ordered]@{ exitCode = $LASTEXITCODE; output = $output.Trim() }
}

function Get-HyperVAdministratorsGroupName {
    # Localized group name, resolved from the well-known SID.
    $account = ([Security.Principal.SecurityIdentifier]::new($script:HyperVAdministratorsSid)).Translate([Security.Principal.NTAccount]).Value
    $account.Split('\')[-1]
}

function Invoke-ServiceAccountProbe {
    param(
        [Parameter(Mandatory)] [string] $VMName,
        [Parameter(Mandatory)] [long] $StepBytes,
        [int] $ResultTimeoutSeconds = 60
    )
    $svc = $script:ProbeServiceName
    $dir = Get-ProbeWorkDir
    $rec = [ordered]@{
        serviceName = $svc; account = $script:ProbeAccount; workDir = $dir
        setup = [System.Collections.Generic.List[object]]::new(); serviceStart = $null; result = $null; error = $null
        cleanup = [System.Collections.Generic.List[object]]::new(); cleanupVerified = $false
    }
    $did = @{ dir = $false; service = $false; member = $false }
    $group = $null
    try {
        $group = Get-HyperVAdministratorsGroupName
        $null = New-Item -ItemType Directory -Path $dir
        $did.dir = $true
        $probePath = Join-Path $dir 'probe.ps1'
        Set-Content -LiteralPath $probePath -Value (New-ProbeScript) -Encoding utf8
        ConvertTo-Json -InputObject ([ordered]@{ vmName = $VMName; stepBytes = $StepBytes }) | Set-Content -LiteralPath (Join-Path $dir 'input.json') -Encoding utf8
        $rec.setup.Add([ordered]@{ action = 'create work directory and probe script'; ok = $true; detail = $null })

        # Same process shape as the host agent's worker: pwsh -NoLogo -NoProfile -NonInteractive.
        $pwsh = (Get-Process -Id $PID).Path
        $binPath = '"{0}" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{1}"' -f $pwsh, $probePath
        $null = New-Service -Name $svc -BinaryPathName $binPath -DisplayName 'hvmc S1 probe (temporary)' -Description 'Temporary service created by hack/hyperv-characterize.ps1; safe to delete.' -StartupType Manual
        $did.service = $true
        $rec.setup.Add([ordered]@{ action = 'New-Service'; ok = $true; detail = $null })

        $r = Invoke-NativeTool -FilePath 'sc.exe' -ArgumentList @('config', $svc, 'obj=', $script:ProbeAccount)
        $rec.setup.Add([ordered]@{ action = 'sc.exe config obj= (virtual account)'; ok = ($r.exitCode -eq 0); detail = $r.output })
        if ($r.exitCode -ne 0) { throw "sc.exe config failed with exit code $($r.exitCode)" }

        $r = Invoke-NativeTool -FilePath 'icacls.exe' -ArgumentList @($dir, '/grant', "$($script:ProbeAccount):(OI)(CI)M")
        $rec.setup.Add([ordered]@{ action = 'icacls grant Modify on work directory'; ok = ($r.exitCode -eq 0); detail = $r.output })
        if ($r.exitCode -ne 0) { throw "icacls failed with exit code $($r.exitCode)" }

        $r = Invoke-NativeTool -FilePath 'net.exe' -ArgumentList @('localgroup', $group, $script:ProbeAccount, '/add')
        $rec.setup.Add([ordered]@{ action = 'add account to Hyper-V Administrators'; ok = ($r.exitCode -eq 0); detail = $r.output })
        if ($r.exitCode -ne 0) { throw "net localgroup /add failed with exit code $($r.exitCode)" }
        $did.member = $true

        # pwsh is not a service binary: SCM reports a start timeout (1053) or an early exit
        # (1067) after the probe finished. The probe writes its result incrementally.
        $rec.serviceStart = Invoke-NativeTool -FilePath 'sc.exe' -ArgumentList @('start', $svc)

        $resultPath = Join-Path $dir 'result.json'
        $deadline = [DateTime]::UtcNow.AddSeconds($ResultTimeoutSeconds)
        do {
            if (Test-Path -LiteralPath $resultPath) {
                try {
                    $parsed = Get-Content -LiteralPath $resultPath -Raw | ConvertFrom-Json -AsHashtable
                    $rec.result = $parsed
                    if ($parsed['complete']) { break }
                } catch {
                    Write-Verbose "probe result not readable yet: $_"
                }
            }
            Start-Sleep -Milliseconds 500
        } while ([DateTime]::UtcNow -lt $deadline)
    } catch {
        $rec.error = ConvertTo-ErrorInfo -ErrorRecord $_
    } finally {
        if ($did.service) {
            try {
                $cim = Get-CimInstance -ClassName Win32_Service -Filter "Name='$svc'"
                if ($null -ne $cim -and [int]$cim.ProcessId -gt 0) { Stop-Process -Id ([int]$cim.ProcessId) -Force }
                $rec.cleanup.Add([ordered]@{ action = 'stop probe process'; ok = $true; detail = $null })
            } catch {
                $rec.cleanup.Add([ordered]@{ action = 'stop probe process'; ok = $false; detail = $_.Exception.Message })
            }
        }
        if ($did.member -and $null -ne $group) {
            $r = Invoke-NativeTool -FilePath 'net.exe' -ArgumentList @('localgroup', $group, $script:ProbeAccount, '/delete')
            $rec.cleanup.Add([ordered]@{ action = 'remove account from Hyper-V Administrators'; ok = ($r.exitCode -eq 0); detail = $r.output })
        }
        if ($did.service) {
            $r = Invoke-NativeTool -FilePath 'sc.exe' -ArgumentList @('delete', $svc)
            $rec.cleanup.Add([ordered]@{ action = 'sc.exe delete'; ok = ($r.exitCode -eq 0); detail = $r.output })
        }
        if ($did.dir) {
            try {
                Remove-Item -LiteralPath $dir -Recurse -Force
                $rec.cleanup.Add([ordered]@{ action = 'remove work directory'; ok = $true; detail = $null })
            } catch {
                $rec.cleanup.Add([ordered]@{ action = 'remove work directory'; ok = $false; detail = $_.Exception.Message })
            }
        }
        $memberLeft = $false
        if ($null -ne $group) {
            $members = Invoke-NativeTool -FilePath 'net.exe' -ArgumentList @('localgroup', $group)
            $memberLeft = $members.output -match [regex]::Escape($svc)
        }
        $rec.cleanupVerified = (-not (Test-ProbeServiceExists)) -and (-not (Test-Path -LiteralPath $dir)) -and (-not $memberLeft)
    }
    $rec
}

# ---------------------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------------------

function Write-ResultFile {
    param(
        [Parameter(Mandatory)] [System.Collections.IDictionary] $Document,
        [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]] $Rules,
        [Parameter(Mandatory)] [string] $OutDir
    )
    $redacted = Protect-Document -InputObject $Document -Rules $Rules
    $redacted.anonymization.leakCheck = 'passed'
    $json = ConvertTo-Json -InputObject $redacted -Depth 16
    $leaks = Find-RedactionLeak -Json $json -Rules $Rules
    if ($leaks.Count -gt 0) {
        $redacted.anonymization.leakCheck = 'failed'
        $json = ConvertTo-Json -InputObject $redacted -Depth 16
    }
    $null = New-Item -ItemType Directory -Path $OutDir -Force
    $stamp = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')
    $path = Join-Path -Path $OutDir -ChildPath "hyperv-characterize-$stamp.json"
    Set-Content -LiteralPath $path -Value $json -Encoding utf8NoBOM
    [ordered]@{ path = $path; leaks = $leaks }
}

function Write-Summary {
    param(
        [Parameter(Mandatory)] [System.Collections.IDictionary] $Document,
        [AllowNull()] [string] $Path
    )
    Write-Host ''
    Write-Host ("S1 Hyper-V characterization: {0} (exit code {1})" -f $Document.run.outcome, $Document.run.exitCode)
    if ($null -ne $Document.run.abortReason) { Write-Host "  abort reason: $($Document.run.abortReason)" }
    $rows = foreach ($s in $Document.steps) {
        $assigned = 'n/a'
        if ($null -ne $s.before -and $null -ne $s.settled) {
            $assigned = '{0} -> {1}' -f ([long]$s.before.assignedBytes / 1MB), ([long]$s.settled.assignedBytes / 1MB)
        }
        $startup = 'n/a'
        if ($null -ne $s.settled) { $startup = [long]$s.settled.startupBytes / 1MB }
        $errorId = ''
        if (@($s.errors).Count -gt 0) { $errorId = $s.errors[0].fullyQualifiedErrorId }
        $target = ''
        if ($null -ne $s.targetBytes) { $target = [long]$s.targetBytes / 1MB }
        [pscustomobject]@{
            Step          = $s.name
            'Target MiB'  = $target
            Expected      = $s.expected
            Observed      = $s.observed
            OK            = $(if ($s.matchesExpectation) { 'yes' } else { 'NO' })
            'Assigned MiB' = $assigned
            'Startup MiB' = $startup
            Diverged      = $s.startupDiverged
            ms            = $s.durationMs
            'Error id'    = $errorId
        }
    }
    if (@($rows).Count -gt 0) { Write-Host (@($rows) | Format-Table -AutoSize | Out-String -Width 220).TrimEnd() }
    $restore = $Document.restore
    if ($null -ne $restore) {
        Write-Host ("  restore: original MemoryStartup {0}, final MemoryStartup {1}, final MemoryAssigned {2}, restored: {3}" -f (Format-MiB $restore.originalStartupBytes), (Format-MiB $restore.finalStartupBytes), (Format-MiB $restore.finalAssignedBytes), $restore.restored)
    }
    $probe = $Document.serviceAccountProbe
    if ($null -ne $probe) {
        Write-Host ("  service account probe ({0}): cleanup verified: {1}" -f $probe.account, $probe.cleanupVerified)
        if ($null -ne $probe.result -and $probe.result.Contains('operations')) {
            foreach ($op in $probe.result['operations']) { Write-Host ("    {0}: {1}" -f $op['name'], $op['result']) }
        }
    }
    if ($Path) { Write-Host "  result: $Path" }
    Write-Host '  Review the JSON before committing it as a fixture (docs/spikes/s1-hyperv.md).'
}

# ---------------------------------------------------------------------------------------
# Orchestration
# ---------------------------------------------------------------------------------------

function Invoke-Characterization {
    param(
        [Parameter(Mandatory)] [string] $VMName,
        [Parameter(Mandatory)] [string] $OutDir,
        [int] $StepMiB = 256,
        [int] $LargeStepMiB = 1024,
        [int] $RefuseTargetMiB = 256,
        [int] $HostMarginMiB = 4096,
        [int] $SettleSeconds = 5,
        [switch] $GuestCheck,
        [AllowNull()] [pscredential] $GuestCredential,
        [int] $GuestTimeoutSeconds = 60,
        [switch] $ServiceAccountProbe,
        [switch] $DryRun
    )
    $stepBytes = [long]$StepMiB * 1MB
    $marginBytes = [long]$HostMarginMiB * 1MB

    $facts = Get-SafetyFactSet -VMName $VMName -ServiceAccountProbe:$ServiceAccountProbe
    $plan = $null
    if ($null -ne $facts.vm) {
        $plan = New-Plan -BaselineBytes ([long]$facts.vm.MemoryStartup) -StepBytes $stepBytes -LargeStepBytes ([long]$LargeStepMiB * 1MB) -RefuseTargetBytes ([long]$RefuseTargetMiB * 1MB)
    }
    $checks = Test-Safety -Facts $facts -Plan $plan -MarginBytes $marginBytes -GuestCheck:$GuestCheck -HasGuestCredential:($null -ne $GuestCredential) -ServiceAccountProbe:$ServiceAccountProbe

    Write-Host 'Safety checks:'
    foreach ($c in $checks) { Write-Host ('  [{0}] {1}: {2}' -f $(if ($c.passed) { 'ok' } else { 'FAIL' }), $c.name, $c.detail) }
    if (@($checks | Where-Object { -not $_.passed }).Count -gt 0) {
        Write-Host 'Refused: nothing was changed.'
        return 2
    }
    Write-Host ('Plan: baseline {0}; grow to {1} and {2}; shrink to {0}; refused shrink to {3}; up to {4} midpoint retries; restore to {0}.' -f (Format-MiB $plan.baselineBytes), (Format-MiB $plan.growStepTargetBytes), (Format-MiB $plan.growLargeTargetBytes), (Format-MiB $plan.refuseTargetBytes), $plan.partialShrinkMaxAttempts)
    if ($DryRun) {
        Write-Host 'Dry run: nothing was changed.'
        return 0
    }

    $vmId = [guid]$facts.vm.Id
    $original = [long]$facts.vm.MemoryStartup
    $rules = New-RedactionRuleSet -VMName $VMName -VMId $vmId -HostNames (Get-HostNameList)
    $steps = [System.Collections.Generic.List[object]]::new()
    $doc = [ordered]@{
        schemaVersion       = $script:SchemaVersion
        kind                = $script:DocumentKind
        script              = [ordered]@{ path = 'hack/hyperv-characterize.ps1'; version = $script:ScriptVersion }
        run                 = [ordered]@{ startedAt = (Get-UtcTimestamp); finishedAt = $null; outcome = 'running'; abortReason = $null; exitCode = $null }
        anonymization       = [ordered]@{ vmName = $script:VmNamePlaceholder; vmId = $script:VmIdPlaceholder; hostName = $script:HostPlaceholder; leakCheck = $null }
        parameters          = [ordered]@{
            stepBytes = $stepBytes; largeStepBytes = [long]$LargeStepMiB * 1MB; refuseTargetBytes = [long]$RefuseTargetMiB * 1MB
            hostMarginBytes = $marginBytes; settleSeconds = $SettleSeconds; guestCheck = [bool]$GuestCheck
            guestTimeoutSeconds = $GuestTimeoutSeconds; serviceAccountProbe = [bool]$ServiceAccountProbe
        }
        invoker             = [ordered]@{ isAdministrator = [bool](Test-IsElevated); tokenHasHyperVAdministrators = [bool](Test-TokenHasHyperVAdministrators) }
        host                = [ordered]@{
            osCaption = "$($facts.os.Caption)"; osVersion = "$($facts.os.Version)"; osBuildNumber = "$($facts.os.BuildNumber)"
            psVersion = $PSVersionTable.PSVersion.ToString(); hyperVModuleVersion = $facts.hyperV.moduleVersion
            totalVisibleMemoryBytes = $facts.hostTotalBytes; freePhysicalMemoryBytes = $facts.hostFreeBytes
        }
        safety              = $checks
        plan                = $plan
        baseline            = $null
        steps               = $steps
        serviceAccountProbe = $null
        restore             = $null
    }

    $guest = $null
    $index = 0
    try {
        $doc.baseline = [ordered]@{
            vm              = Get-ObjectShape -InputObject $facts.vm -Property $script:VMShapeProperties
            vmMemory        = Get-ObjectShape -InputObject $facts.memory -Property $script:VMMemoryShapeProperties
            operatingSystem = Get-ObjectShape -InputObject $facts.os -Property $script:OSShapeProperties
            state           = Get-VMMemoryState -VMId $vmId
            guest           = $null
        }
        if ($GuestCheck) {
            $guestRecord = [ordered]@{ totalBytes = $null; offsetBytes = $null; error = $null }
            try {
                $session = New-GuestSession -VMId $vmId -Credential $GuestCredential
                $total = Get-GuestTotalBytes -Session $session
                $guestRecord.totalBytes = $total
                $guestRecord.offsetBytes = $doc.baseline.state.assignedBytes - $total
                $guest = [ordered]@{ session = $session; offsetBytes = $guestRecord.offsetBytes; timeoutSeconds = $GuestTimeoutSeconds }
            } catch {
                $guestRecord.error = ConvertTo-ErrorInfo -ErrorRecord $_
                Write-Warning "guest check disabled: $($_.Exception.Message)"
            }
            $doc.baseline.guest = $guestRecord
        }

        $steps.Add((Invoke-ReadErrorStep -Index ($index++)))

        $sequence = @(
            @{ name = 'set-unaligned'; target = $plan.unalignedTargetBytes; expected = 'refusal' }
            @{ name = 'grow-step'; target = $plan.growStepTargetBytes; expected = 'success' }
            @{ name = 'grow-large'; target = $plan.growLargeTargetBytes; expected = 'success' }
            @{ name = 'shrink-to-baseline'; target = $plan.baselineBytes; expected = 'success' }
            @{ name = 'shrink-refused'; target = $plan.refuseTargetBytes; expected = 'refusal' }
        )
        foreach ($s in $sequence) {
            Assert-SafeToWrite -VMName $VMName -VMId $vmId -TargetBytes $s.target -MarginBytes $marginBytes
            $steps.Add((Invoke-ResizeStep -Index ($index++) -Name $s.name -VMName $VMName -VMId $vmId -TargetBytes $s.target -Expected $s.expected -SettleSeconds $SettleSeconds -StepBytes $stepBytes -Guest $guest))
        }

        $refused = $plan.refuseTargetBytes
        for ($attempt = 1; $attempt -le $script:PartialShrinkMaxAttempts; $attempt++) {
            if ($steps[$steps.Count - 1].result -eq 'succeeded') { break }
            $current = (Get-VMMemoryState -VMId $vmId).assignedBytes
            $mid = Get-MidpointTarget -RefusedTargetBytes $refused -CurrentAssignedBytes $current -StepBytes $stepBytes
            if ($null -eq $mid) { break }
            Assert-SafeToWrite -VMName $VMName -VMId $vmId -TargetBytes $mid -MarginBytes $marginBytes
            $steps.Add((Invoke-ResizeStep -Index ($index++) -Name "shrink-partial-$attempt" -VMName $VMName -VMId $vmId -TargetBytes $mid -Expected 'any' -SettleSeconds $SettleSeconds -StepBytes $stepBytes -Guest $guest))
            $refused = $mid
        }

        if ($ServiceAccountProbe) {
            $current = Get-VMMemoryState -VMId $vmId
            Assert-SafeToWrite -VMName $VMName -VMId $vmId -TargetBytes ($current.startupBytes + $stepBytes) -MarginBytes $marginBytes
            $doc.serviceAccountProbe = Invoke-ServiceAccountProbe -VMName $VMName -StepBytes $stepBytes
        }
        $doc.run.outcome = 'completed'
    } catch {
        $doc.run.outcome = 'aborted'
        $doc.run.abortReason = $_.Exception.Message
        Write-Warning "sequence aborted: $($_.Exception.Message)"
    } finally {
        # Always put MemoryStartup back, whatever happened above (also on Ctrl+C).
        $restore = [ordered]@{ originalStartupBytes = $original; needed = $null; restored = $false; finalStartupBytes = $null; finalAssignedBytes = $null; error = $null }
        try {
            $now = Get-VMMemoryState -VMId $vmId
            $restore.needed = ($now.startupBytes -ne $original) -or ($now.assignedBytes -ne $original)
            if ($restore.needed) {
                for ($attempt = 1; $attempt -le 2; $attempt++) {
                    Assert-SafeToWrite -VMName $VMName -VMId $vmId -TargetBytes $original -MarginBytes $marginBytes -Restore
                    $r = Invoke-ResizeStep -Index ($index++) -Name "restore-$attempt" -VMName $VMName -VMId $vmId -TargetBytes $original -Expected 'success' -SettleSeconds $SettleSeconds -StepBytes $stepBytes -Guest $guest
                    $steps.Add($r)
                    if ($r.settled.startupBytes -eq $original) { break }
                    if ($attempt -lt 2) { Start-Sleep -Seconds 5 }
                }
            }
            $final = Get-VMMemoryState -VMId $vmId
            $restore.finalStartupBytes = $final.startupBytes
            $restore.finalAssignedBytes = $final.assignedBytes
            $restore.restored = ($final.startupBytes -eq $original)
        } catch {
            $restore.error = ConvertTo-ErrorInfo -ErrorRecord $_
        }
        $doc.restore = $restore
        if ($null -ne $guest) {
            try { Remove-GuestSession -Session $guest.session } catch { Write-Verbose "guest session cleanup: $_" }
        }
        if ($doc.run.outcome -eq 'running') { $doc.run.outcome = 'interrupted' }
        $doc.run.finishedAt = Get-UtcTimestamp
        $exitCode = 0
        if ($doc.run.outcome -ne 'completed') { $exitCode = 1 }
        if (-not $restore.restored) { $exitCode = 3 }
        $doc.run.exitCode = $exitCode

        $written = $null
        try {
            $written = Write-ResultFile -Document $doc -Rules $rules -OutDir $OutDir
            if ($written.leaks.Count -gt 0) {
                Write-Warning ("anonymisation check failed for: {0}. Do not commit this file without editing it." -f ($written.leaks -join ', '))
            }
        } catch {
            Write-Warning "could not write the result file: $($_.Exception.Message)"
        }
        $path = $null
        if ($null -ne $written) { $path = $written.path }
        Write-Summary -Document $doc -Path $path
        if (-not $restore.restored) {
            Write-Warning ("MemoryStartup was NOT restored to {0}. Restore it manually: Set-VMMemory -VMName <name> -StartupBytes {1}" -f (Format-MiB $original), $original)
        }
    }
    return $exitCode
}

# Dot-sourcing (tests) loads the functions only.
if ($MyInvocation.InvocationName -eq '.') { return }

$invokeArgs = @{
    VMName              = $VMName
    OutDir              = $OutDir
    StepMiB             = $StepMiB
    LargeStepMiB        = $LargeStepMiB
    RefuseTargetMiB     = $RefuseTargetMiB
    HostMarginMiB       = $HostMarginMiB
    SettleSeconds       = $SettleSeconds
    GuestCheck          = $GuestCheck
    GuestCredential     = $GuestCredential
    GuestTimeoutSeconds = $GuestTimeoutSeconds
    ServiceAccountProbe = $ServiceAccountProbe
    DryRun              = $DryRun
}
$exitCode = @(Invoke-Characterization @invokeArgs)[-1]
exit ([int]$exitCode)
