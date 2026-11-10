param([string]$TerraformPath, [switch]$SkipInit)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repositoryDirectory = Split-Path $PSScriptRoot -Parent
$moduleDirectory = Join-Path $repositoryDirectory 'infra\terraform'
$artifactDirectory = Join-Path $repositoryDirectory '.cache'
New-Item -ItemType Directory -Path $artifactDirectory -Force | Out-Null
$logFile = Join-Path $artifactDirectory 'milestone2-terraform-tests.log'
'Local mocked Terraform verification; no cloud plan/apply/import.' | Set-Content -LiteralPath $logFile -Encoding UTF8

if ([string]::IsNullOrWhiteSpace($TerraformPath)) {
    $installedTerraform = Get-Command terraform -ErrorAction SilentlyContinue
    if ($null -ne $installedTerraform) {
        $TerraformPath = $installedTerraform.Source
    } else {
        $TerraformPath = Join-Path $repositoryDirectory '.cache\milestone2-small-db-tools\terraform.exe'
    }
}
if (-not (Test-Path -LiteralPath $TerraformPath)) { throw 'Install Terraform 1.13.5 or pass -TerraformPath.' }

function Invoke-LocalTerraform {
    param([string[]]$Arguments, [int]$ExpectedTests = 0)
    $savedPreference = $ErrorActionPreference
    $lines = New-Object 'System.Collections.Generic.List[string]'
    try {
        $ErrorActionPreference = 'Continue'
        & $TerraformPath @Arguments 2>&1 | ForEach-Object {
            $line = $_.ToString()
            $lines.Add($line)
            $line | Out-File -FilePath $logFile -Append -Encoding UTF8
            $line
        }
        $nativeExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $savedPreference
    }
    if ($nativeExitCode -ne 0) { throw "Terraform failed: $($Arguments -join ' ')" }
    if ($ExpectedTests -gt 0 -and ($lines -join "`n") -notmatch "Success! $ExpectedTests passed, 0 failed\.") {
        throw "Expected $ExpectedTests tests; an empty/partial filter match is not verification."
    }
}

$originalDataDirectory = $env:TF_DATA_DIR
$env:TF_DATA_DIR = Join-Path $moduleDirectory '.terraform'
try {
    Invoke-LocalTerraform -Arguments @("-chdir=$moduleDirectory", 'fmt', '-check', '-recursive')
    if (-not $SkipInit) {
        Invoke-LocalTerraform -Arguments @("-chdir=$moduleDirectory", 'init', '-backend=false', '-input=false', '-lockfile=readonly', '-no-color')
    }
    Invoke-LocalTerraform -Arguments @("-chdir=$moduleDirectory", 'validate', '-no-color')
    foreach ($suite in @('safety', 'small_database', 'rollout', 'notifications')) {
        $relativeFile = Join-Path 'tests' "$suite.tftest.hcl"
        $fullFile = Join-Path $moduleDirectory $relativeFile
        $expectedCount = ([regex]::Matches((Get-Content -LiteralPath $fullFile -Raw), '(?m)^run\s+"')).Count
        if ($expectedCount -eq 0) { throw "No test runs found in $relativeFile" }
        $arguments = @("-chdir=$moduleDirectory", 'test', '-no-color', "-filter=$relativeFile")
        if ($suite -eq 'small_database') {
            $fixture = Join-Path $moduleDirectory 'tests\fixtures\small-db.tfvars.example'
            $arguments += "-var-file=$fixture"
        }
        Invoke-LocalTerraform -Arguments $arguments -ExpectedTests $expectedCount
    }
    Write-Output 'All Terraform safety, budget and staged-rollout mock tests passed.'
} finally {
    $env:TF_DATA_DIR = $originalDataDirectory
}
