$ErrorActionPreference = 'Stop'
$target = $env:TRANSCOOP_TARGET
$stage = $env:TRANSCOOP_STAGE
$work = Split-Path -LiteralPath $stage
$backup = Join-Path $work 'previous.exe'
$lock = $null
$keep = $false
$failed = $false
try {
    $parent = Get-Process -Id $env:TRANSCOOP_PARENT_PID -ErrorAction SilentlyContinue
    if ($parent -and !$parent.WaitForExit(15000)) { throw 'Timed out waiting for the update command to exit.' }
    $lock = [IO.File]::Open(($target + '.update.lock'), 'Open', 'ReadWrite', 'ReadWrite')
    $lock.Lock(0, 1)
    # Another updater may have finished between the launcher exiting and this lock.
    if ((Get-FileHash -LiteralPath $target).Hash -ne $env:TRANSCOOP_ORIGINAL_HASH) {
        throw 'The installed executable changed while waiting. Run update again.'
    }
    if ((Get-FileHash -LiteralPath $stage).Hash -ne $env:TRANSCOOP_STAGE_HASH) {
        throw 'The verified replacement changed while waiting.'
    }
    try {
        [IO.File]::Replace($stage, $target, $backup)
    } catch {
        throw ('Could not replace {0}. Stop running Transcoop workers or coordinators and retry. {1}' -f $target, $_)
    }
    $keep = $true
    $info = New-Object System.Diagnostics.ProcessStartInfo
    $info.FileName = $target
    $info.Arguments = 'version'
    $info.UseShellExecute = $false
    $info.RedirectStandardOutput = $true
    $probe = [Diagnostics.Process]::Start($info)
    try {
        $output = $probe.StandardOutput.ReadToEndAsync()
        if (!$probe.WaitForExit(10000)) {
            $probe.Kill()
            $probe.WaitForExit()
            throw 'The installed executable timed out during its version check.'
        }
        if ($probe.ExitCode -ne 0 -or $output.Result.Trim() -cne ('transcoop ' + $env:TRANSCOOP_VERSION)) {
            throw 'The installed executable failed its version check.'
        }
    } finally {
        $probe.Dispose()
    }
    $keep = $false
    [Console]::WriteLine(('Installed {0}. Restart any Transcoop services to use it.' -f $env:TRANSCOOP_VERSION))
} catch {
    $failed = $true
    [Console]::Error.WriteLine($_)
    if ([IO.File]::Exists($backup)) {
        try {
            if ([IO.File]::Exists($target)) {
                [IO.File]::Replace($backup, $target, (Join-Path $work 'rejected.exe'))
            } else {
                [IO.File]::Move($backup, $target)
            }
            $keep = $false
            [Console]::Error.WriteLine('Restored the previous executable.')
        } catch {
            $keep = $true
            [Console]::Error.WriteLine(('Restore {0} manually. {1}' -f $backup, $_))
        }
    }
} finally {
    if ($lock) { $lock.Dispose() }
    if (!$keep) { Remove-Item -LiteralPath $work -Recurse -Force }
}
if ($failed) { exit 1 }
