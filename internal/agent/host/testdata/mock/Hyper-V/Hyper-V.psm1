# Mock of the Hyper-V PowerShell module for the host agent's Windows tests.
#
# It replays a fixture (../../hyperv/<scenario>.json, path in
# $env:HVMC_HYPERV_FIXTURE) with the documented behaviour of Get-VM,
# Get-VMMemory and Set-VMMemory, and the error records of errors.json in the
# same directory. It also shadows Get-Service (for vmms) and Get-CimInstance
# (for Win32_OperatingSystem), because the GitHub Windows runner has no
# Hyper-V. The state lives in this module and changes with every
# Set-VMMemory, like a host would. The Go fake worker (fakeworker_test.go)
# implements the same semantics.

$script:Fixture = Get-Content -Raw -LiteralPath $env:HVMC_HYPERV_FIXTURE | ConvertFrom-Json
$script:Errors = Get-Content -Raw -LiteralPath (Join-Path (Split-Path -Parent $env:HVMC_HYPERV_FIXTURE) 'errors.json') | ConvertFrom-Json
$script:Alignment = [int64]2MB

function Get-MockProperty($Object, [string]$Name, $Default) {
    $p = $Object.PSObject.Properties[$Name]
    if ($null -eq $p -or $null -eq $p.Value) {
        return $Default
    }
    return $p.Value
}

function New-MockErrorRecord([string]$Key, [string]$Target) {
    $e = $script:Errors.$Key
    $ex = [System.Runtime.InteropServices.COMException]::new([string]$e.message, [int]$e.hresult)
    return [System.Management.Automation.ErrorRecord]::new(
        $ex, [string]$e.errorId, [System.Management.Automation.ErrorCategory]([string]$e.category), $Target)
}

function Assert-MockVmms($Cmdlet) {
    if ([string]$script:Fixture.vmms -ne 'Running') {
        $Cmdlet.ThrowTerminatingError((New-MockErrorRecord 'vmmsDown' ''))
    }
}

function Invoke-MockHang([string]$Name) {
    if (@($script:Fixture.hang) -contains $Name) {
        [Console]::Error.WriteLine("hung $Name")
        Start-Sleep -Seconds 86400
    }
}

function Get-MockFree {
    $free = [int64]$script:Fixture.totalBytes - [int64]$script:Fixture.overheadBytes
    foreach ($v in @($script:Fixture.vms)) {
        $free -= [int64]$v.assignedBytes
    }
    return $free
}

function ConvertTo-MockVM($v) {
    return [pscustomobject]@{
        PSTypeName           = 'Microsoft.HyperV.PowerShell.VirtualMachine'
        Name                 = [string]$v.name
        Id                   = [guid][string]$v.id
        State                = [string]$v.state
        MemoryAssigned       = [int64]$v.assignedBytes
        MemoryStartup        = [int64]$v.startupBytes
        MemoryMinimum        = [int64]$v.minimumBytes
        MemoryMaximum        = [int64]$v.maximumBytes
        DynamicMemoryEnabled = [bool]$v.dynamicMemoryEnabled
    }
}

function Find-MockVM([guid]$Id) {
    foreach ($v in @($script:Fixture.vms)) {
        if ([guid][string]$v.id -eq $Id) {
            return $v
        }
    }
    return $null
}

function Get-VM {
    [CmdletBinding()]
    param(
        [Parameter(Position = 0)][string[]]$Name,
        [guid]$Id
    )
    Assert-MockVmms $PSCmdlet
    Invoke-MockHang 'Get-VM'
    if ($PSBoundParameters.ContainsKey('Id')) {
        $v = Find-MockVM $Id
        if ($null -eq $v) {
            $PSCmdlet.ThrowTerminatingError((New-MockErrorRecord 'vmNotFound' ([string]$Id)))
        }
        return (ConvertTo-MockVM $v)
    }
    foreach ($v in @($script:Fixture.vms)) {
        if ($null -eq $Name -or $Name -contains [string]$v.name) {
            ConvertTo-MockVM $v
        }
    }
}

function Get-VMMemory {
    [CmdletBinding()]
    param([Parameter(Mandatory, Position = 0)]$VM)
    Assert-MockVmms $PSCmdlet
    Invoke-MockHang 'Get-VMMemory'
    $v = Find-MockVM $VM.Id
    if ($null -eq $v) {
        $PSCmdlet.ThrowTerminatingError((New-MockErrorRecord 'vmNotFound' ([string]$VM.Id)))
    }
    return [pscustomobject]@{
        PSTypeName           = 'Microsoft.HyperV.PowerShell.VMMemory'
        VMName               = [string]$v.name
        Startup              = [int64]$v.startupBytes
        Minimum              = [int64]$v.minimumBytes
        Maximum              = [int64]$v.maximumBytes
        Buffer               = [int32]$v.buffer
        Priority             = [int32]$v.priority
        DynamicMemoryEnabled = [bool]$v.dynamicMemoryEnabled
    }
}

# Static memory: an off VM takes any startup value. A running VM grows when
# the host has the memory (else out of memory, nothing changes) and shrinks
# down to its shrink floor: below it the guest releases memory only down to
# the floor (partial, or nothing when the floor is at or above the current
# assignment) and the cmdlet fails; the startup value follows the result
# unless startupDrift is set.
function Set-MockStartup($Cmdlet, $v, [int64]$n) {
    if ([string]$v.state -ne 'Running') {
        $v.startupBytes = $n
        return
    }
    $cur = [int64]$v.assignedBytes
    if ($n -gt $cur) {
        if ($n - $cur -gt (Get-MockFree)) {
            $Cmdlet.ThrowTerminatingError((New-MockErrorRecord 'outOfMemory' ([string]$v.name)))
        }
        $v.assignedBytes = $n
        $v.startupBytes = $n
        return
    }
    $floor = Get-MockProperty $v 'shrinkFloorBytes' $null
    if ($null -eq $floor -or $n -ge [int64]$floor) {
        $v.assignedBytes = $n
        $v.startupBytes = $n
        return
    }
    $up = [int64]([Math]::Ceiling([double]$floor / $script:Alignment)) * $script:Alignment
    $got = [Math]::Min($cur, $up)
    $v.assignedBytes = $got
    if ([bool](Get-MockProperty $v 'startupDrift' $false)) {
        $v.startupBytes = $n
    } else {
        $v.startupBytes = $got
    }
    $Cmdlet.ThrowTerminatingError((New-MockErrorRecord 'shrinkRefused' ([string]$v.name)))
}

function Set-VMMemory {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory, Position = 0)]$VM,
        [int64]$StartupBytes,
        [int64]$MinimumBytes,
        [int64]$MaximumBytes
    )
    Assert-MockVmms $PSCmdlet
    Invoke-MockHang 'Set-VMMemory'
    $v = Find-MockVM $VM.Id
    if ($null -eq $v) {
        $PSCmdlet.ThrowTerminatingError((New-MockErrorRecord 'vmNotFound' ([string]$VM.Id)))
    }
    if ($PSBoundParameters.ContainsKey('MinimumBytes')) {
        $v.minimumBytes = $MinimumBytes
    }
    if ($PSBoundParameters.ContainsKey('MaximumBytes')) {
        $v.maximumBytes = $MaximumBytes
    }
    if ($PSBoundParameters.ContainsKey('StartupBytes')) {
        Set-MockStartup $PSCmdlet $v $StartupBytes
    }
}

function Get-Service {
    [CmdletBinding()]
    param([Parameter(Position = 0)][string[]]$Name)
    if ($Name -contains 'vmms') {
        return [pscustomobject]@{ Name = 'vmms'; Status = [string]$script:Fixture.vmms }
    }
    Microsoft.PowerShell.Management\Get-Service @PSBoundParameters
}

function Get-CimInstance {
    [CmdletBinding()]
    param([string]$ClassName)
    if ($ClassName -ne 'Win32_OperatingSystem') {
        return (CimCmdlets\Get-CimInstance @PSBoundParameters)
    }
    $free = Get-MockProperty $script:Fixture 'reportedFreeBytes' (Get-MockFree)
    return [pscustomobject]@{
        CSName                 = [string]$script:Fixture.computerName
        TotalVisibleMemorySize = [int64]([int64]$script:Fixture.totalBytes / 1KB)
        FreePhysicalMemory     = [int64]([int64]$free / 1KB)
    }
}

Export-ModuleMember -Function Get-VM, Get-VMMemory, Set-VMMemory, Get-Service, Get-CimInstance
