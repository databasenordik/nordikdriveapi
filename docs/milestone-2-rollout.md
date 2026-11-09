# Deploy Milestone 2 to the existing development project

This is the deployment procedure for **nordikdriveapi**, in the existing Google
Cloud project **planar-ray-472112-e8**, region **us-west1**. It uses the current
SQL instance **nordikdrive**, database **postgres**, and photos bucket
**nordik-drive-photos**. Production and other applications are outside this work.

The result is three Drive services:

| Cloud Run service | What runs there | Who calls it |
| --- | --- | --- |
| nordikdriveapi | Gateway: checks authentication, applies request controls, forwards API calls | Your frontend, at its existing API URL |
| nordikdriveapi-legacy | Your existing API functionality: login, files, chat and support | The gateway, using IAM |
| nordikdriveapi-worker | Scheduled background processing | Cloud Scheduler, using IAM |

The existing development NIA service, **nordik-nia-api**, receives its small-pool
image/configuration update. A separate preview gateway is not required: the new
gateway revision is tested using a tag on the existing nordikdriveapi service.

No new Google Cloud project, billing-account change, remote state bucket, manual
GitHub deployment environment or application rewrite is required for this rollout.
Terraform uses local state for this single-operator development deployment.

## What is already finished

* The local application, PostgreSQL integration, Envoy and race checks passed.
* The Terraform checks passed, including the single-public-gateway rollout.
* Server, gateway-adapter and development NIA images have been built and published.
* Their exact image references are in .cache/milestone2-release-images.json.
* The repository is databasenordik/nordikdriveapi. Its existing GitHub-to-Google
  trust already matches master. No owner-migration command is needed.

You do not need to repeat those builds/tests or search GitHub for a missing
image. If application code changes after these builds, the affected image must
be rebuilt before deployment. A successful build does not mean Cloud Run has
been updated; the deployment steps below make that change.

Read and execute one numbered step at a time. Each step states its purpose,
where to run it, its effect and the condition for continuing. If a command fails,
stop at that command. Do not copy every block into the terminal at once.

## 1. Open one PowerShell window

**Where:** PowerShell on your Windows computer. Keep this window open throughout
these steps. The commands use the API directory even when working with NIA.

**Why:** This sets the existing deployment target and loads small helpers. A
variable such as $serverImage holds a value for later commands; it is not a
Cloud Run service by itself.

```powershell
Set-Location 'C:\Nordik Projects\nordikdriveapi'
$ErrorActionPreference = 'Stop'
$projectId = 'planar-ray-472112-e8'
$deployRegion = 'us-west1'
$python = 'C:\Python314\python.exe'
$env:CLOUDSDK_PYTHON = $python
$terraform = Join-Path (Get-Location).Path '.cache\milestone2-small-db-tools\terraform.exe'
if (-not (Test-Path -LiteralPath $terraform)) { throw 'The prepared Terraform executable is missing.' }
function Assert-M2Command {
    if ($LASTEXITCODE -ne 0) { throw 'Previous command failed. Stop here.' }
}
function Invoke-M2Terraform {
    $previousToken = $env:GOOGLE_OAUTH_ACCESS_TOKEN
    try {
        if ($args[0] -eq 'apply' -and (Test-Path 'infra\terraform\terraform.tfstate')) {
            $backupFolder = Join-Path (Get-Location).Path '.cache\milestone2-state-backups'
            New-Item -ItemType Directory -Path $backupFolder -Force | Out-Null
            Copy-Item 'infra\terraform\terraform.tfstate' (Join-Path $backupFolder ((Get-Date -Format 'yyyyMMdd-HHmmss-fff') + '.tfstate'))
        }
        $env:GOOGLE_OAUTH_ACCESS_TOKEN = gcloud auth print-access-token
        Assert-M2Command
        & $terraform '-chdir=infra/terraform' @args
        Assert-M2Command
    } finally {
        $env:GOOGLE_OAUTH_ACCESS_TOKEN = $previousToken
    }
}
```

The Terraform helper uses the existing gcloud login. It keeps the temporary
access token in memory and clears/restores it after the command. No new account
or billing configuration is created. Start Docker Desktop with Linux containers;
it supplies the SQL client used later, so a separate psql installation is unnecessary.

**Continue when:** the setup runs without an error and Docker Desktop is running.
If you close PowerShell, repeat this setup and Step 4 to reload the image values.

## 2. Check the current development deployment

**Where:** the same PowerShell window.

**Why:** Record the current revision before changing it and confirm the selected
Google account. The inventory helper strips credentials from its saved report.

```powershell
gcloud auth list
& $python scripts/milestone2_inventory.py
Assert-M2Command
$inventory = Get-Content '.cache\milestone2-live-inventory.json' -Raw | ConvertFrom-Json
$apiInventory = @($inventory.services | Where-Object { $_.name -eq 'nordikdriveapi' })[0]
$publicApiUrl = $apiInventory.url
$apiInventory.traffic
```

**Effect:** read-only cloud queries and a local metadata file. Do not dump full
service configurations or credentials into chat or committed files.

At the last check the API revision was nordikdriveapi-00335-fcp, and NIA was
nordik-nia-api-00033-8r4. Use the current output if it differs. The public API URL
comes from the service itself; do not replace it with a newly invented address.

**Continue when:** the report refers to the development API, NIA and nordikdrive
SQL instance. No production deployment commands belong in this procedure.

## 3. Save the release source in GitHub

**Why:** The published images were built from the local Milestone 2 working copy.
Commit that source so it is reviewable and recoverable. This step saves source;
initial runtime deployment is performed by Terraform later.

**3a. API source — PowerShell in the API directory**

The current API branch is bug_fixes_api. Review the changes before staging:

```powershell
git branch --show-current
git status --short
```

Stage the Milestone 2 source/configuration paths, then inspect the staged summary:

```powershell
git add -- .github .gitignore .dockerignore Dockerfile README.md cmd config docs go.mod go.sum infra internal migrations scripts tests
Assert-M2Command
git diff --cached --stat
```

The staged release must include the updated .github/workflows/cd-go.yml,
Dockerfile, cmd/gateway, worker/database code, migrations and Terraform files.
Do not add credentials, local tfvars, state or .cache files. If an unrelated file
was staged, remove it from the commit using git restore --staged followed by its
path; that keeps the local file itself.

After reviewing the staged files:

```powershell
git commit -m 'Prepare Milestone 2 development gateway and worker rollout'
Assert-M2Command
git push -u origin bug_fixes_api
Assert-M2Command
```

In https://github.com/databasenordik/nordikdriveapi/pulls create a pull request:
base **master**, compare **bug_fixes_api**. Wait for CI checks to pass, review the
changes, then merge. Include the updated workflow in the same release: the old
workflow on master automatically deploys the server, while the new workflow
only builds/publishes server and gateway images. The separate runtime workflow
runs only when manually dispatched. You do not need its GitHub variables now.

**3b. NIA source — use the existing NIA feature branch**

These commands use git -C, so your PowerShell directory stays at the API root:

```powershell
git -C 'C:\Nordik Projects\nordik_nia_api' branch --show-current
```

The checked branch was nia_deployment_fix. On that feature branch, commit only
the three pool-related files:

```powershell
git -C 'C:\Nordik Projects\nordik_nia_api' add -- backend/src/config.ts backend/src/research/database.ts tests/backend/config.test.ts
Assert-M2Command
git -C 'C:\Nordik Projects\nordik_nia_api' commit -m 'Make development NIA database pools configurable'
Assert-M2Command
git -C 'C:\Nordik Projects\nordik_nia_api' push -u origin nia_deployment_fix
Assert-M2Command
```

Keep this on the feature branch during this rollout. NIA main has an automatic
deployment workflow; merging there would start a separate deployment and its
existing workflow settings need review before that merge. The workflow file
itself has not been changed. The development image is already published and is
rolled out explicitly in Step 10.

**Continue when:** API source is saved/reviewed with passing CI, and the NIA pool
source is saved on its feature branch. If these commits already exist and Git
shows no relevant changes, this step is complete; do not create empty commits.

## 4. Load the three images already published

**Where:** the same API-root PowerShell window.

**Why:** Later commands need exact package addresses. An address ending in
@sha256 identifies one specific image. No Read-Host prompt or new build is needed.

```powershell
$images = Get-Content '.cache\milestone2-release-images.json' -Raw | ConvertFrom-Json
$serverImage = $images.serverImage
$adapterImage = $images.adapterImage
$niaImage = $images.niaImage
foreach ($image in @($serverImage,$adapterImage,$niaImage)) {
    if ($image -notmatch '@sha256:[a-f0-9]{64}$') { throw 'A published image reference is missing.' }
}
$serverImage
$adapterImage
$niaImage
```

**Effect:** sets local PowerShell variables only. The server image serves both
the API backend and worker, using different runtime settings. The adapter image
is the gateway's authorization component; Terraform also supplies pinned Envoy.

**Continue when:** all three full references print successfully.

## 5. Initialize local Terraform state and create runtime identities

**Why:** Terraform needs a record of the resources it manages. Local state is
sufficient for this operator-run development deployment. Keep it on this
workstation, out of Git and out of image builds. Do not delete it between stages.

No backend.tf is required. The checked working copy had no active remote backend
or applied state. If a backend/state was configured separately, preserve it and
resolve that existing configuration rather than creating a competing state.

```powershell
if (Test-Path 'infra\terraform\backend.tf') { throw 'An active backend exists. Preserve its state configuration before continuing.' }
if (-not (Test-Path 'infra\terraform\nordikdrive.tfvars')) {
    Copy-Item 'infra\terraform\nordikdrive.tfvars.example' 'infra\terraform\nordikdrive.tfvars'
}
Invoke-M2Terraform init
Invoke-M2Terraform plan '-var-file=nordikdrive.tfvars' '-out=bootstrap.tfplan'
Invoke-M2Terraform show bootstrap.tfplan
```

The profile starts at deployment_stage = "bootstrap". The plan should contain
service accounts, required APIs, secret metadata and scoped IAM. It must not
start application services, change SQL settings, create schedules, alter traffic
or store secret payloads. Existing SQL/bucket resources are referenced, not
recreated. Reject unexpected deletes or resources outside this scope.

After checking that saved plan:

```powershell
Invoke-M2Terraform apply bootstrap.tfplan
```

**Effect:** creates deployment identities/permissions and secret containers in
the existing project. The current API revision still serves users.

**Continue when:** Terraform apply succeeds. The local state is
infra/terraform/terraform.tfstate. Terraform maintains its own backup; keep a
separate private backup before later changes. Future commands must use this same
state. Do not initialize a second checkout against the same resources.

## 6. Preserve credentials and prepare the release settings

**Why:** Gateway/backend must use the current JWT value so existing cookies stay
valid. SMTP and Zoom must also retain their working values. The helper copies
bytes directly to Secret Manager without printing payloads.

```powershell
& $python scripts/milestone2_seed_secrets.py
Assert-M2Command
& $python scripts/milestone2_seed_secrets.py --apply
Assert-M2Command
$secretRefs = Get-Content '.cache\milestone2-transition-secret-refs.json' -Raw | ConvertFrom-Json
```

The first command shows the intended credential names. The second creates/reuses
numeric secret versions. Terraform grants the new runtime identities their
required access; the shared old runtime account receives no new secret grant.
The helper does not rotate credentials or change Cloud Run.
The JSON file contains secret IDs/version numbers, not values.

JWT/SMTP/Zoom are used by the new services. The old DB password, Gemini key and
honour secret are retained only for operator recovery before deleting the old
revision; they are not passed to the new IAM-based runtime services.

Prepare the runtime settings file from the allowlisted current settings:

```powershell
$release = Get-Content '.cache\milestone2-runtime-settings-review.tfvars.json' -Raw | ConvertFrom-Json
$release | Add-Member -NotePropertyName server_image -NotePropertyValue $serverImage -Force
$release | Add-Member -NotePropertyName adapter_image -NotePropertyValue $adapterImage -Force
$release | Add-Member -NotePropertyName jwt_secret -NotePropertyValue $secretRefs.JWT_SECRET -Force
$runtimeSecrets = @{
    gateway = @{}
    legacy = @{ GMAIL_APP_PASSWORD = $secretRefs.GMAIL_APP_PASSWORD; ZOOM_CLIENT_SECRET = $secretRefs.ZOOM_CLIENT_SECRET }
    worker = @{ GMAIL_APP_PASSWORD = $secretRefs.GMAIL_APP_PASSWORD; ZOOM_CLIENT_SECRET = $secretRefs.ZOOM_CLIENT_SECRET }
}
$release | Add-Member -NotePropertyName runtime_secrets -NotePropertyValue $runtimeSecrets -Force
$releaseFile = Join-Path (Get-Location).Path 'infra\terraform\release.tfvars.json'
[IO.File]::WriteAllText($releaseFile,($release | ConvertTo-Json -Depth 12),(New-Object Text.UTF8Encoding($false)))
```

Open release.tfvars.json locally. Check that it preserves bucket/timezone/sender,
CHAT_USE_NIA=true, NIA TLS/target/timeout, Zoom enabled/account/client IDs and the
canonical NIA audience. Existing CORS defaults stay in the application code; no
new frontend domain is invented. The base profile supplies bucket IAM grants.

**Continue when:** the numeric JWT/SMTP/Zoom references are present and the
current enabled features are preserved. Keep runtime_prerequisites_verified
false until the database work below is complete. Missing required settings are
errors to resolve, not values to guess.

## 7. Back up the development database and enable IAM login

**Why:** New services use individual service-account logins instead of sharing
the old database password. Cloud SQL must enable IAM authentication first.
A backup provides a recovery point before schema changes.

**Where:** the same PowerShell window. These are actual development-instance
changes; changing database flags can restart/reconnect database clients.

```powershell
gcloud sql backups create --instance=nordikdrive --project=$projectId --description='Before Milestone 2 deployment'
Assert-M2Command
$sqlSettings = gcloud sql instances describe nordikdrive --project=$projectId --format='json(settings.databaseFlags)' | ConvertFrom-Json
Assert-M2Command
$flags = @($sqlSettings.settings.databaseFlags | Where-Object { $null -ne $_ })
$iamOn = @($flags | Where-Object { $_.name -eq 'cloudsql.iam_authentication' -and $_.value -eq 'on' }).Count -eq 1
if (-not $iamOn) {
    if ($flags.Count -ne 0) { throw 'Other flags now exist. Preserve their values when preparing the IAM flag update.' }
    gcloud sql instances patch nordikdrive --project=$projectId '--database-flags=cloudsql.iam_authentication=on'
    Assert-M2Command
}
gcloud sql instances describe nordikdrive --project=$projectId --format='yaml(settings.databaseFlags)'
```

The latest verified flag list was empty. This block refuses to replace a newly
changed nonempty flag list; it does not silently delete existing flags. Existing
password logins remain during handover. No instance tier/size upgrade is requested.

**Continue when:** the backup completed and cloudsql.iam_authentication is on.

## 8. Create IAM database users and apply the existing migrations

**Why:** Cloud IAM permission permits login; PostgreSQL table permissions are a
separate requirement. The migrations create the limiter/job-ledger tables and
prepare legacy description columns. The grants give gateway access to its own
schema and the backend/worker the transitional permissions for current workflows.

Set the database stage in the local release file:

```powershell
$release | Add-Member -NotePropertyName deployment_stage -NotePropertyValue 'database' -Force
$release | Add-Member -NotePropertyName iam_database_authentication_verified -NotePropertyValue $true -Force
[IO.File]::WriteAllText($releaseFile,($release | ConvertTo-Json -Depth 12),(New-Object Text.UTF8Encoding($false)))
Invoke-M2Terraform plan '-var-file=nordikdrive.tfvars' '-var-file=release.tfvars.json' '-out=database.tfplan'
Invoke-M2Terraform show database.tfplan
```

Check that this adds the development SQL IAM users, without starting runtime
services or changing public traffic. Then apply that saved plan:

```powershell
Invoke-M2Terraform apply database.tfplan
& $python scripts/milestone2_migrate.py
Assert-M2Command
& $python scripts/milestone2_migrate.py --apply
Assert-M2Command
```

The migration helper gets usernames from the local Terraform state and uses
an ephemeral Docker SQL client. It retrieves the existing owner password in
memory; it does not print it, put it on the command line, or save it. It applies
002_legacy_descriptions.up.sql, 002_platform.up.sql and 002_platform_grants.sql
in order, stopping if a file fails. Keep the backup; do not run rollback SQL
blindly if an earlier file succeeded and a later file failed.

**Continue when:** all three files print Applied successfully.

## 9. Review the connection allocation before starting runtime services

**Why:** The verified database limit is 25. The Terraform safeguard permits an
allocation of 20. The old API can open 25 per instance, so it must retire before
starting the new stack. The maintenance handover below accomplishes that without
changing any shared database login's limit.

The minimal topology has the following allocation, including one replacement
instance for each controlled service:

| Consumer | Serving allocation | Replacement allowance | Total |
| --- | ---: | ---: | ---: |
| Gateway | 1 | 1 | 2 |
| API backend | 2 | 2 | 4 |
| Worker, one concurrent job | 2 | 2 | 4 |
| Cleanup job, one execution | 1 | 0 | 1 |
| NIA, two pools of one | 2 | 2 | 4 |
| Operator | 1 | 0 | 1 |
| Existing outside-client allowance supplied by you | 1 | 0 | 1 |
| Total planned allocation | | | **17 of 20** |

The outside-client row is your stated planning allowance, not a database-enforced
quota. Its application and scaling configuration remain unchanged. The last
read-only SQL check found no ordinary application sessions active; that is an
observation, not proof of an outside hard limit. If that outside client exceeds
its allocation, shared-instance contention is still possible. Do not claim its
maximum was changed or verified as one. Keep deployment concurrency controlled
and inspect actual connections during acceptance.

**Continue when:** this explicit allocation has been reviewed. Do not increase
pools/instances or run simultaneous cleanup executions without rechecking the
20-slot plan. The later review flag records this allocation review; it does not
assert that outside applications have enforced connection quotas.

## 10. Stage a SQL-free maintenance revision and verify proxy handling

**Why:** Staging a gateway alongside the old 25-connection API risks exhausting
SQL. A temporary Envoy revision uses no database connections. It also lets us
measure the actual forwarded-IP chain instead of guessing TRUSTED_PROXY_HOPS.
Staging it at 0% leaves the existing API serving normal requests.

**Where:** the same PowerShell window. Keep the current API revision name for
the handover and rollback record:

```powershell
$current = gcloud run services describe nordikdriveapi --project=$projectId --region=$deployRegion --format='json(status.traffic)' | ConvertFrom-Json
Assert-M2Command
$serving = @($current.status.traffic | Where-Object { $_.percent -eq 100 })
if ($serving.Count -ne 1) { throw 'Inspect split traffic before changing it.' }
$apiOld = $serving[0].revisionName
$maintenanceFlags = @{
    '--image' = 'envoyproxy/envoy@sha256:93a82e7584cb8181ad408e4e49477a3ae1d775f698214841eda4e9309fc06a06'
    '--command' = @('/usr/local/bin/envoy')
    '--args' = @('--config-yaml',(Get-Content 'infra\maintenance\envoy.json' -Raw),'--disable-hot-restart')
    '--max' = 1
    '--max-instances' = 1
    '--no-traffic' = $true
    '--tag' = 'm2-maintenance'
}
$maintenanceFlagsFile = Join-Path (Get-Location).Path '.cache\m2-maintenance-flags.json'
[IO.File]::WriteAllText($maintenanceFlagsFile,($maintenanceFlags | ConvertTo-Json -Depth 8),(New-Object Text.UTF8Encoding($false)))
gcloud run deploy nordikdriveapi --project=$projectId --region=$deployRegion "--flags-file=$maintenanceFlagsFile" --clear-env-vars --clear-secrets
Assert-M2Command
$staged = gcloud run services describe nordikdriveapi --project=$projectId --region=$deployRegion --format='json(status.latestReadyRevisionName,status.traffic)' | ConvertFrom-Json
Assert-M2Command
$maintenanceRevision = $staged.status.latestReadyRevisionName
$maintenanceUrl = @($staged.status.traffic | Where-Object { $_.tag -eq 'm2-maintenance' })[0].url
if (-not $maintenanceUrl -or $maintenanceRevision -eq $apiOld) { throw 'Maintenance revision did not become ready.' }
```

**Effect:** creates a zero-SQL tagged revision and lowers the development service
maximum to one; ordinary traffic stays on the saved API revision. The maintenance
template has no application credentials; the old serving revision retains its
existing environment until retirement. The preserved recovery secrets prevent
credential loss. Importing the maintenance template copies no payloads into state.

Measure forwarded headers through this tagged URL. The maintenance health route
reflects only the forwarded-IP chain, not cookies, authorization or credentials.
Use /m2-health for this public probe. Cloud Run reserves some paths ending in z,
including /healthz; the gateway's internal startup probe still uses /healthz.
See [Cloud Run reserved paths](https://docs.cloud.google.com/run/docs/known-issues#reserved_url_paths).
The deliberately fake address below verifies that a caller-supplied address is
not selected as the real client:

```powershell
$probeId = [guid]::NewGuid().ToString('N')
$probe = Invoke-WebRequest "$maintenanceUrl/m2-health?m2_probe=$probeId" -Headers @{ 'X-Forwarded-For' = '198.51.100.10' } -UseBasicParsing
$forwarded = @(([string]$probe.Headers['x-m2-forwarded-for']).Split(',') | ForEach-Object { $_.Trim() })
$logFilter = 'resource.type="cloud_run_revision" AND resource.labels.service_name="nordikdriveapi" AND httpRequest.requestUrl:"m2_probe=' + $probeId + '"'
$observedIp = ''
for ($attempt = 0; $attempt -lt 6 -and -not $observedIp; $attempt++) {
    $observedIp = gcloud logging read $logFilter --project=$projectId --limit=1 --freshness=10m --format='value(httpRequest.remoteIp)'
    Assert-M2Command
    if (-not $observedIp) { Start-Sleep -Seconds 5 }
}
$matchingClientIps = @($forwarded | Where-Object { $_ -eq $observedIp })
if (-not $observedIp -or $matchingClientIps.Count -ne 1 -or $observedIp -eq '198.51.100.10') { throw 'The proxy chain was not verified. Inspect the response/log before proceeding.' }
$proxyHops = $forwarded.Count - [array]::IndexOf($forwarded,$observedIp)
if ($proxyHops -lt 1 -or $proxyHops -gt 10) { throw 'Unexpected proxy chain.' }
$release | Add-Member -NotePropertyName trusted_proxy_hops -NotePropertyValue $proxyHops -Force
[IO.File]::WriteAllText($releaseFile,($release | ConvertTo-Json -Depth 12),(New-Object Text.UTF8Encoding($false)))
```

**Continue when:** the tagged health request succeeds and its selected client IP
matches Cloud Run's request log, not the supplied fake value. No arbitrary hop
number is entered by hand. Recheck this if an external load balancer/proxy is
introduced or the actual frontend's API ingress route differs.

## 11. Start the development maintenance interval and update NIA

**Why:** Retiring the old API frees its unbounded pool and stops its embedded
cron. New background schedules remain paused until ownership is transferred.
This is a planned development interruption: application requests receive 503
while the new stack is prepared. Do not describe this as zero downtime.

```powershell
gcloud scheduler jobs pause honour-daily --project=$projectId --location=$deployRegion
Assert-M2Command
gcloud run services update-traffic nordikdriveapi --project=$projectId --region=$deployRegion "--to-revisions=$maintenanceRevision=100"
Assert-M2Command
```

Wait for existing API requests to finish. Inspect traffic, remove any tags
pointing to apiOld, and retire only that saved revision. Deleting a revision
removes it as a direct rollback target; retain its image/configuration record.

```powershell
$handover = gcloud run services describe nordikdriveapi --project=$projectId --region=$deployRegion --format='json(status.traffic)' | ConvertFrom-Json
Assert-M2Command
$oldTags = @($handover.status.traffic | Where-Object { $_.revisionName -eq $apiOld -and $_.tag } | ForEach-Object tag)
if ($oldTags.Count -gt 0) {
    gcloud run services update-traffic nordikdriveapi --project=$projectId --region=$deployRegion "--remove-tags=$($oldTags -join ',')"
    Assert-M2Command
}
gcloud run services describe nordikdriveapi --project=$projectId --region=$deployRegion --format='yaml(status.traffic)'
# Execute after confirming no old percentage/tags and finished requests:
gcloud run revisions delete $apiOld --project=$projectId --region=$deployRegion
Assert-M2Command
```

Now stage and promote the prepared development NIA image. Its existing provider,
DB URL, runtime identity and IAM grants stay as configured; the command changes
the image, pool setting and instance limits:

```powershell
$niaBefore = gcloud run services describe nordik-nia-api --project=$projectId --region=$deployRegion --format='json(status.traffic)' | ConvertFrom-Json
Assert-M2Command
$niaOld = @($niaBefore.status.traffic | Where-Object { $_.percent -eq 100 })[0].revisionName
gcloud run deploy nordik-nia-api --project=$projectId --region=$deployRegion --image=$niaImage --update-env-vars=NIA_DB_POOL_MAX=1 --max=1 --max-instances=1 --no-traffic --tag=m2-pools
Assert-M2Command
$niaNew = gcloud run services describe nordik-nia-api --project=$projectId --region=$deployRegion --format='value(status.latestReadyRevisionName)'
Assert-M2Command
if (-not $niaNew -or $niaNew -eq $niaOld) { throw 'The NIA pool revision is not ready.' }
gcloud run services update-traffic nordik-nia-api --project=$projectId --region=$deployRegion "--to-revisions=$niaNew=100"
Assert-M2Command
gcloud run services update-traffic nordik-nia-api --project=$projectId --region=$deployRegion --remove-tags=m2-pools
Assert-M2Command
```

The temporary tag activates the zero-traffic revision for startup verification.
Without a tag, an unserved revision can remain inactive and latestReadyRevisionName
can still name the previous image. Do not promote that previous name by mistake.

Check its startup readiness; wait for old NIA calls to drain and remove any old
revision tags. Retire the saved old revision after verifying the new one. No
shared SQL role-limit change is part of this procedure.

```powershell
gcloud run services describe nordik-nia-api --project=$projectId --region=$deployRegion --format='yaml(status.traffic)'
# Only after verifying readiness, no old traffic/tags and finished calls:
gcloud run revisions delete $niaOld --project=$projectId --region=$deployRegion
Assert-M2Command
& $python scripts/milestone2_inventory.py
Assert-M2Command
```

**Continue when:** old Drive/NIA unbounded revisions are retired, NIA shows
NIA_DB_POOL_MAX=1 and both maxima are 1. honour-daily stays paused. Record the
scheduling gap for reconciliation after worker handover.

## 12. Import the existing public service and stage the new stack

**Why:** The public service already exists. Terraform must import it rather than
create a second resource with that name. The new gateway starts at 0% traffic;
maintenance continues serving the ordinary URL until acceptance succeeds.

Re-read the sanitized report after Step 11:

```powershell
$inventory = Get-Content '.cache\milestone2-live-inventory.json' -Raw | ConvertFrom-Json
$niaInventory = @($inventory.services | Where-Object { $_.name -eq 'nordik-nia-api' })[0]
$apiInventory = @($inventory.services | Where-Object { $_.name -eq 'nordikdriveapi' })[0]
if ($niaInventory.environment.NIA_DB_POOL_MAX -ne '1' -or $niaInventory.revision_max_instances -ne '1' -or $niaInventory.service_max_instances -ne '1') { throw 'NIA limits do not match the reviewed allocation.' }
if (-not $apiInventory.public_import_config_has_no_literal_credentials) { throw 'Remove remaining literal credentials/serialized configuration before import; do not copy payloads into state.' }
$release | Add-Member -NotePropertyName deployment_stage -NotePropertyValue 'cutover' -Force
$release | Add-Member -NotePropertyName enable_preview_gateway -NotePropertyValue $false -Force
$release | Add-Member -NotePropertyName public_gateway -NotePropertyValue $true -Force
$release | Add-Member -NotePropertyName public_serving_revision -NotePropertyValue $maintenanceRevision -Force
$release | Add-Member -NotePropertyName stage_public_candidate -NotePropertyValue $true -Force
$release | Add-Member -NotePropertyName runtime_prerequisites_verified -NotePropertyValue $true -Force
$release | Add-Member -NotePropertyName connection_budget_reviewed -NotePropertyValue $true -Force
[IO.File]::WriteAllText($releaseFile,($release | ConvertTo-Json -Depth 12),(New-Object Text.UTF8Encoding($false)))
if (-not (Test-Path 'infra\terraform\public-import.tf')) {
    Copy-Item 'infra\terraform\public-import.tf.example' 'infra\terraform\public-import.tf'
}
Invoke-M2Terraform plan '-var-file=nordikdrive.tfvars' '-var-file=release.tfvars.json' '-out=candidate.tfplan'
Invoke-M2Terraform show candidate.tfplan
```

Review this plan carefully. Expected: import/update nordikdriveapi in place;
create the private backend/worker, cleanup job and paused schedules; no separate
preview service; maintenance 100%, new gateway 0% tagged milestone2; no SQL
instance/tier/billing changes. Reject replacements/deletes or unrelated services.
The reviewed budget should allocate 17 against the limit of 20.

```powershell
Invoke-M2Terraform apply candidate.tfplan
Invoke-M2Terraform output connection_budget
Invoke-M2Terraform output backend_urls
gcloud run services describe nordikdriveapi --project=$projectId --region=$deployRegion --format='yaml(status.latestReadyRevisionName,status.traffic)'
```

**Continue when:** the new gateway/legacy/worker are ready and the milestone2 tag
is present at 0%. Record the gateway revision name shown. Its tagged URL is your
candidate test address. The ordinary API URL still returns maintenance responses.

## 13. Test the candidate, then promote its exact revision

**Why:** A ready process alone does not establish that login, IAM, database grants
and existing business flows work. Test with controlled development records and
recipients; this is the real development database/bucket/provider configuration.
Do not send real notification jobs merely as a health check.

| Test through the milestone2 tag | Result required before promotion |
| --- | --- |
| Cloud Run startup probes and a public request to a known API route | Probes pass; API returns the expected response/status |
| Login/refresh with your controlled test account | Correct success response and cookie |
| A synthetic upload/read/download/version | Correct content and filename; clean up the test data |
| Chat on a permitted test list | Streaming/history succeeds; unauthorized access is rejected |
| Controlled support flow | Current support/Zoom behavior succeeds with the intended recipient |
| Direct private backend/worker request without IAM | Rejected |
| Gateway-to-backend and backend-to-NIA calls | Succeed using the configured identities/audiences |
| Concurrent requests and a cold/reconnect cycle | No connection exhaustion or permission failures |

Use a cookie created before cutover to verify continuity at the original API URL
after promotion; browser host-only cookies are not automatically sent to the
separate tag hostname. Do not paste credentials or cookie values into chat.

When these checks pass, enter the exact tested revision name. This is a revision
name from the status output, not an image URI or an arbitrary future latest:

```powershell
$gatewayRevision = Read-Host 'Paste the exact gateway revision tested through the milestone2 tag'
if ($gatewayRevision -notmatch '^nordikdriveapi-[0-9]+-[a-z0-9]+$') { throw 'Unexpected development gateway revision name.' }
$release.public_serving_revision = $gatewayRevision
$release.stage_public_candidate = $false
[IO.File]::WriteAllText($releaseFile,($release | ConvertTo-Json -Depth 12),(New-Object Text.UTF8Encoding($false)))
Invoke-M2Terraform plan '-var-file=nordikdrive.tfvars' '-var-file=release.tfvars.json' '-out=promote.tfplan'
Invoke-M2Terraform show promote.tfplan
Invoke-M2Terraform apply promote.tfplan
```

**Effect:** moves the original development API URL to the tested gateway.
Verify your actual frontend immediately: login, an existing cookie, files, chat
and support. Keep the maintenance revision as a zero-SQL fallback during this
initial verification. No frontend URL or DNS change is needed.

## 14. Enable the worker schedules and finish NIA IAM

**Why:** The old cron revision and honour-daily trigger have retired/paused.
Enabling the new schedules transfers normal scheduled processing to one worker.
The new job ledger handles duplicate scheduled slots/retries. Do this only
after verifying providers/recipients and worker permissions.

```powershell
$release | Add-Member -NotePropertyName legacy_scheduling_retired_verified -NotePropertyValue $true -Force
$release | Add-Member -NotePropertyName enable_schedules -NotePropertyValue $true -Force
[IO.File]::WriteAllText($releaseFile,($release | ConvertTo-Json -Depth 12),(New-Object Text.UTF8Encoding($false)))
Invoke-M2Terraform plan '-var-file=nordikdrive.tfvars' '-var-file=release.tfvars.json' '-out=schedules.tfplan'
Invoke-M2Terraform show schedules.tfplan
Invoke-M2Terraform apply schedules.tfplan
gcloud scheduler jobs list --project=$projectId --location=$deployRegion --format='table(name,schedule,timeZone,state)'
```

The old honour-daily stays paused. Confirm real scheduled deliveries and job
ledger outcomes, including duplicate/retry handling, and reconcile the recorded
maintenance gap. This restores the application's normal scheduled work; avoid
manual runs that might send unintended real messages.

The old unauthenticated NIA caller is gone. Verify chat through the public API
uses IAM, then remove only NIA's public invoker member:

```powershell
gcloud run services remove-iam-policy-binding nordik-nia-api --project=$projectId --region=$deployRegion --member=allUsers --role=roles/run.invoker
Assert-M2Command
```

Confirm unauthenticated NIA calls fail while public API chat still works. NIA
serves development only, as you confirmed. Its other IAM members are preserved.

**Done when:** the public API flows work, worker schedules deliver correctly,
IAM boundaries work, and the reviewed connection limits remain configured.
Record images/revisions, numeric secret versions, cold/reconnect observations
and acceptance results in docs/milestone-2.md. Local tests alone do not establish
these cloud results.

## Rollback for this rollout

Before promotion, a failed candidate receives no ordinary traffic: keep
maintenance serving while correcting it. If the promoted gateway fails, set
public_serving_revision back to the saved maintenance revision and use a reviewed
plan/apply. That provides a known SQL-free maintenance response while repairing
the stack. Retain a verified gateway revision for subsequent image rollbacks.

A monolith rollback needs its saved image/settings and a new connection/schedule
allocation review. Do not run the old 25-connection API alongside the new stack,
or enable embedded cron while external schedules own jobs. Keep the current JWT
and additive platform tables. Emergency gcloud traffic changes must be reflected
in Terraform inputs immediately afterwards. Do not delete local state to undo a
deployment; stage reversal would attempt deletes and is blocked by prevent_destroy.

## Items intentionally outside this deployment

New projects/billing links, production state, a remote state migration, a separate
preview service, manual GitHub deployment-environment setup, unrelated application
deployments, broad benchmarks and future service extractions are not prerequisites
for this development release. Future milestones can address their own scope.

Technical command references:
[Cloud Run staging](https://docs.cloud.google.com/sdk/gcloud/reference/run/deploy),
[Cloud SQL IAM](https://docs.cloud.google.com/sql/docs/postgres/add-manage-iam-users),
[SQL flags](https://docs.cloud.google.com/sdk/gcloud/reference/sql/instances/patch).
