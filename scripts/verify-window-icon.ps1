[CmdletBinding()]
param(
    [string]$Path = (Join-Path $PSScriptRoot '..\build\bin\GAMEBOOST.exe')
)

$ErrorActionPreference = 'Stop'
$exe = (Resolve-Path -LiteralPath $Path).Path

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class GameboostIconResource
{
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern IntPtr LoadLibraryEx(string file, IntPtr reserved, uint flags);

    [DllImport("kernel32.dll")]
    public static extern bool FreeLibrary(IntPtr module);

    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern IntPtr LoadImage(IntPtr module, IntPtr id, uint type, int width, int height, uint flags);

    [DllImport("user32.dll")]
    public static extern bool DestroyIcon(IntPtr icon);
}
'@

$module = [GameboostIconResource]::LoadLibraryEx($exe, [IntPtr]::Zero, 2) # LOAD_LIBRARY_AS_DATAFILE
if ($module -eq [IntPtr]::Zero) {
    throw [ComponentModel.Win32Exception]::new([Runtime.InteropServices.Marshal]::GetLastWin32Error())
}
try {
    # Wails uses resource ID 3 for the native window icon.
    $icon = [GameboostIconResource]::LoadImage($module, [IntPtr]3, 1, 16, 16, 0)
    if ($icon -eq [IntPtr]::Zero) {
        throw 'GAMEBOOST executable is missing its native window icon (resource 3).'
    }
    [void][GameboostIconResource]::DestroyIcon($icon)
} finally {
    [void][GameboostIconResource]::FreeLibrary($module)
}
Write-Host 'Verified the GAMEBOOST window icon.'
