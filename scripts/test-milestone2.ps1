param([switch]$WithEnvoy, [switch]$KeepFixture)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$projectDirectory = Split-Path $PSScriptRoot -Parent
$originalFixture = $env:TEST_DATABASE_URL
$startedHere = $false
Push-Location $projectDirectory
try {
    $running = & docker compose -p nordik-milestone2 -f docker-compose.test.yml ps --status running -q
    if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect the local fixture.' }
    $startedHere = [string]::IsNullOrWhiteSpace(($running -join ''))
    & docker compose -p nordik-milestone2 -f docker-compose.test.yml up -d --wait
    if ($LASTEXITCODE -ne 0) { throw 'Cannot start the isolated synthetic PostgreSQL fixture.' }
    $env:TEST_DATABASE_URL = 'postgres://nordik_test:synthetic-test-only@127.0.0.1:55432/nordik_baseline_test?sslmode=disable'
    $packages = & go list ./... | Where-Object { $_ -notmatch '/(cmd|internal)/deceasedimport$' }
    if ($LASTEXITCODE -ne 0) { throw 'Cannot enumerate repository packages.' }
    & go test -work @packages -count=1 '-coverprofile=coverage.unit.out'
    if ($LASTEXITCODE -ne 0) { throw 'Default regression suite failed.' }
    & go test -work -tags=integration @packages -count=1 '-coverpkg=./...' '-coverprofile=coverage.integration.out'
    if ($LASTEXITCODE -ne 0) { throw 'Integration regression suite failed.' }
    & go tool cover '-func=coverage.integration.out'
    if ($LASTEXITCODE -ne 0) { throw 'Cannot report combined coverage.' }
    & python scripts/check-platform-coverage.py coverage.integration.out
    if ($LASTEXITCODE -ne 0) { throw 'New platform core coverage is below 90%.' }
    & go vet @packages
    if ($LASTEXITCODE -ne 0) { throw 'Static checks failed.' }
    if ($WithEnvoy) {
        & docker build --target verification -t nordik-milestone2-verification .
        if ($LASTEXITCODE -ne 0) { throw 'Cannot build the local verification image.' }
        $fixtureContainer = (& docker compose -p nordik-milestone2 -f docker-compose.test.yml ps -q postgres).Trim()
        $verificationName = 'nordik-verification-' + [guid]::NewGuid().ToString('N').Substring(0,12)
        & docker run --rm --name $verificationName --network "container:$fixtureContainer" -v /var/run/docker.sock:/var/run/docker.sock `
            -e 'TEST_DATABASE_URL=postgres://nordik_test:synthetic-test-only@127.0.0.1:5432/nordik_baseline_test?sslmode=disable' `
            -e 'ENVOY_FIXTURE_HOST=127.0.0.1' -e "ENVOY_FIXTURE_NETWORK=container:$verificationName" `
            nordik-milestone2-verification go test '-tags=integration,envoyintegration' ./tests/integration -run '^TestEnvoyServiceRoutingAndStreaming$' -count=1
        if ($LASTEXITCODE -ne 0) { throw 'Real Envoy integration test failed.' }
    }
} finally {
    $env:TEST_DATABASE_URL = $originalFixture
    if ($startedHere -and -not $KeepFixture) {
        & docker compose -p nordik-milestone2 -f docker-compose.test.yml down
    }
    Pop-Location
}
