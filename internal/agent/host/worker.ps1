# hvmc host agent: PowerShell worker (DESIGN §4.2).
#
# The agent starts `pwsh -NoLogo -NoProfile -NonInteractive` with a short
# bootstrap (worker.go) that defines $hvmcIn, $hvmcOut and Read-HvmcExact,
# reads this script from the first frame on stdin and dot-sources it. Every
# frame is a 4-byte big-endian length followed by UTF-8 JSON. Nothing else
# may reach stdout: every value is assigned or discarded, and the bootstrap
# redirects Console.Out to stderr.
#
# Output never depends on PowerShell's formatting: every result is an
# explicit Select-Object projection with enums stringified, serialized with
# ConvertTo-Json -Compress -Depth 4 (-AsArray for lists).
#
# Errors are classified from the failed cmdlet and the host only (see
# errors.go for the table): vmms_down when the vmms service is not running
# after a failure, out_of_memory for out-of-memory HResults, failed for
# everything else. Refused and partial shrinks are recognized by the agent
# from the VM re-read after the write, not from (localized) messages.

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$WarningPreference = 'SilentlyContinue'
$InformationPreference = 'SilentlyContinue'
$VerbosePreference = 'SilentlyContinue'
$DebugPreference = 'SilentlyContinue'
$ConfirmPreference = 'None'

# HResults that mean the host ran out of memory for the operation.
$HvmcOutOfMemoryHResults = @(
    -2147024882 # 0x8007000E E_OUTOFMEMORY
    -2147024888 # 0x80070008 ERROR_NOT_ENOUGH_MEMORY
    -2147023446 # 0x800705AA ERROR_NO_SYSTEM_RESOURCES
)

function Write-HvmcFrame([string]$Json) {
    $body = [Text.Encoding]::UTF8.GetBytes($Json)
    $n = $body.Length
    $hdr = [byte[]]@((($n -shr 24) -band 255), (($n -shr 16) -band 255), (($n -shr 8) -band 255), ($n -band 255))
    $hvmcOut.Write($hdr, 0, 4)
    $hvmcOut.Write($body, 0, $n)
    $hvmcOut.Flush()
}

function Read-HvmcFrame {
    $hdr = Read-HvmcExact 4
    $n = ([int64]$hdr[0] -shl 24) -bor ([int64]$hdr[1] -shl 16) -bor ([int64]$hdr[2] -shl 8) -bor [int64]$hdr[3]
    return [Text.Encoding]::UTF8.GetString((Read-HvmcExact $n))
}

function Format-HvmcOk([int64]$Id, [string]$ResultJson) {
    return '{"id":' + $Id + ',"ok":true,"result":' + $ResultJson + '}'
}

function Format-HvmcError([int64]$Id, $ErrorObject) {
    return '{"id":' + $Id + ',"ok":false,"error":' + (ConvertTo-Json -InputObject $ErrorObject -Compress -Depth 4) + '}'
}

function Test-HvmcVmmsRunning {
    try {
        $svc = Get-Service -Name vmms -ErrorAction Stop
        return ([string]$svc.Status -eq 'Running')
    } catch {
        return $false
    }
}

function Test-HvmcOutOfMemory([Exception]$Exception) {
    for ($e = $Exception; $null -ne $e; $e = $e.InnerException) {
        if ($e -is [OutOfMemoryException] -or $HvmcOutOfMemoryHResults -contains $e.HResult) {
            return $true
        }
    }
    return $false
}

function ConvertTo-HvmcError {
    param(
        [Parameter(Mandatory)][Management.Automation.ErrorRecord]$Record,
        [string]$Kind = ''
    )
    $ex = $Record.Exception
    if ($Kind -eq '') {
        if (-not (Test-HvmcVmmsRunning)) {
            $Kind = 'vmms_down'
        } elseif (Test-HvmcOutOfMemory $ex) {
            $Kind = 'out_of_memory'
        } else {
            $Kind = 'failed'
        }
    }
    return [pscustomobject]@{
        kind      = $Kind
        message   = [string]$ex.Message
        category  = [string]$Record.CategoryInfo.Category
        errorId   = [string]$Record.FullyQualifiedErrorId
        exception = [string]$ex.GetType().FullName
        hresult   = [int64]$ex.HResult
    }
}

function Assert-HvmcVmms {
    if (-not (Test-HvmcVmmsRunning)) {
        throw [InvalidOperationException]::new('The Hyper-V Virtual Machine Management service (vmms) is not running.')
    }
}

# vms: every VM with its memory configuration (Get-VM, Get-VMMemory).
function Get-HvmcVMs {
    $list = [Collections.Generic.List[object]]::new()
    foreach ($vm in @(Get-VM)) {
        $mem = Get-VMMemory -VM $vm
        $list.Add(($vm | Select-Object -Property @(
                    @{ Name = 'name'; Expression = { [string]$_.Name } }
                    @{ Name = 'id'; Expression = { ([guid][string]$_.Id).ToString('D') } }
                    @{ Name = 'state'; Expression = { [string]$_.State } }
                    @{ Name = 'assignedBytes'; Expression = { [int64]$_.MemoryAssigned } }
                    @{ Name = 'startupBytes'; Expression = { [int64]$mem.Startup } }
                    @{ Name = 'dynamicMemoryEnabled'; Expression = { [bool]$mem.DynamicMemoryEnabled } }
                    @{ Name = 'minimumBytes'; Expression = { [int64]$mem.Minimum } }
                    @{ Name = 'maximumBytes'; Expression = { [int64]$mem.Maximum } }
                    @{ Name = 'buffer'; Expression = { [int32]$mem.Buffer } }
                    @{ Name = 'priority'; Expression = { [int32]$mem.Priority } }
                )))
    }
    if ($list.Count -eq 0) {
        return '[]'
    }
    return ($list | ConvertTo-Json -AsArray -Compress -Depth 4)
}

# host: computer name and physical memory (Win32_OperatingSystem, KiB).
function Get-HvmcHost {
    Assert-HvmcVmms
    $os = Get-CimInstance -ClassName Win32_OperatingSystem | Select-Object -First 1 -Property @(
        @{ Name = 'computerName'; Expression = { [string]$_.CSName } }
        @{ Name = 'totalVisibleMemoryKiB'; Expression = { [int64]$_.TotalVisibleMemorySize } }
        @{ Name = 'freePhysicalMemoryKiB'; Expression = { [int64]$_.FreePhysicalMemory } }
    )
    return (ConvertTo-Json -InputObject $os -Compress -Depth 4)
}

# setStartup: Set-VMMemory -StartupBytes on the VM with the given ID.
function Set-HvmcStartup($Arguments) {
    $vm = Get-VM -Id ([guid][string]$Arguments.id)
    $null = Set-VMMemory -VM $vm -StartupBytes ([int64]$Arguments.startupBytes)
    return '{}'
}

# setDynamic: Set-VMMemory -MinimumBytes / -MaximumBytes (each only when
# given) on the VM with the given ID.
function Set-HvmcDynamic($Arguments) {
    $vm = Get-VM -Id ([guid][string]$Arguments.id)
    $params = @{ VM = $vm }
    if ($null -ne $Arguments.minimumBytes) {
        $params.MinimumBytes = [int64]$Arguments.minimumBytes
    }
    if ($null -ne $Arguments.maximumBytes) {
        $params.MaximumBytes = [int64]$Arguments.maximumBytes
    }
    $null = Set-VMMemory @params
    return '{}'
}

function Invoke-HvmcOp([string]$Op, $Arguments) {
    switch ($Op) {
        'vms' { return Get-HvmcVMs }
        'host' { return Get-HvmcHost }
        'setStartup' { return Set-HvmcStartup $Arguments }
        'setDynamic' { return Set-HvmcDynamic $Arguments }
    }
}

$HvmcOps = @('vms', 'host', 'setStartup', 'setDynamic')

try {
    Import-Module -Name Hyper-V -ErrorAction Stop -WarningAction SilentlyContinue
    $module = Get-Module -Name Hyper-V | Select-Object -First 1
    $hello = [pscustomobject]@{
        pid          = [int64]$PID
        psVersion    = [string]$PSVersionTable.PSVersion
        hypervModule = [string]$module.Path
    }
    Write-HvmcFrame (Format-HvmcOk 0 (ConvertTo-Json -InputObject $hello -Compress -Depth 4))
} catch {
    Write-HvmcFrame (Format-HvmcError 0 (ConvertTo-HvmcError -Record $_ -Kind 'startup'))
    exit 2
}

function New-HvmcProtocolError([string]$Message) {
    return [pscustomobject]@{ kind = 'protocol'; message = $Message }
}

while ($true) {
    $text = Read-HvmcFrame
    $req = $null
    try {
        $req = ConvertFrom-Json -InputObject $text
        $id = [int64]$req.id
        $op = [string]$req.op
    } catch {
        Write-HvmcFrame (Format-HvmcError 0 (New-HvmcProtocolError "undecodable request: $($_.Exception.Message)"))
        continue
    }
    if ($HvmcOps -notcontains $op) {
        Write-HvmcFrame (Format-HvmcError $id (New-HvmcProtocolError "unknown op '$op'"))
        continue
    }
    $hvmcArgs = $null
    if ($req.PSObject.Properties['args']) {
        $hvmcArgs = $req.args
    }
    try {
        $resp = Format-HvmcOk $id (Invoke-HvmcOp $op $hvmcArgs)
    } catch {
        $resp = Format-HvmcError $id (ConvertTo-HvmcError -Record $_)
    }
    Write-HvmcFrame $resp
}
