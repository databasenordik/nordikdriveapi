"""Collect deployment metadata without exporting credential-bearing configuration.

Cloud operations are list/describe/auth-list only. Secret payloads, request
headers, SQL contents and full environment dumps are never written or printed.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
from urllib.parse import unquote, urlsplit, urlunsplit


SAFE_ENVIRONMENT = {
    "DB_HOST", "DB_PORT", "DB_NAME", "DB_USER", "DB_AUTH_MODE", "DB_MAX_OPEN",
    "CLOUD_SQL_INSTANCE", "CLOUD_SQL_IP_TYPE", "BUCKET_NAME", "APP_TIMEZONE",
    "CHAT_USE_NIA", "NIA_AUTH_MODE", "NIA_GRPC_TARGET", "NIA_TOKEN_AUDIENCE",
    "NIA_GRPC_TLS", "NIA_GRPC_TIMEOUT_SECONDS", "ZOOM_ENABLED", "GMAIL_USER",
    "ZOOM_ACCOUNT_ID", "ZOOM_CLIENT_ID", "WEB_CONCURRENCY", "WORKERS",
    "POOL_SIZE", "DB_POOL_SIZE", "MAX_OVERFLOW", "DB_MAX_OVERFLOW",
    "SCHEDULER_MODE", "APP_ROLE", "NIA_DB_POOL_MAX",
}
SENSITIVE_NAME = re.compile(
    r"PASSWORD|SECRET|TOKEN$|(?:^|_)API_KEY$|GEMINI_KEY$|PRIVATE_KEY|CREDENTIAL", re.I
)


def public_url(value: str) -> str:
    """Keep an endpoint's host/path, stripping userinfo, query and fragment."""
    try:
        parsed = urlsplit(value)
        if not parsed.scheme or not parsed.hostname:
            return "<unrecognized URL>"
        hostname = parsed.hostname
        if ":" in hostname:
            hostname = f"[{hostname}]"
        if parsed.port is not None:
            hostname += f":{parsed.port}"
        return urlunsplit((parsed.scheme, hostname, parsed.path, "", ""))
    except ValueError:
        return "<unrecognized URL>"


def database_target(name: str, value: str) -> dict | None:
    try:
        parsed = urlsplit(value)
        if parsed.scheme not in {"postgres", "postgresql"}:
            return None
        if not parsed.hostname:
            return {"setting": name, "unparsed": True}
        return {
            "setting": name,
            "host": parsed.hostname,
            "database": unquote(parsed.path.lstrip("/")),
        }
    except ValueError:
        return None


def safe_service_snapshot(service: dict) -> dict:
    metadata = service.get("metadata", {})
    template = service.get("spec", {}).get("template", {})
    spec = template.get("spec", {})
    environment: dict[str, str] = {}
    secret_references = []
    plaintext_credentials = set()
    database_targets = []
    serialized_configuration_annotations = []
    for location, annotations in (
        ("service", metadata.get("annotations", {})),
        ("template", template.get("metadata", {}).get("annotations", {})),
    ):
        for name in annotations:
            if re.search(r"last-applied|serialized.configuration|config.dump", name, re.I):
                serialized_configuration_annotations.append(f"{location}:{name}")
    for container in spec.get("containers", []):
        for entry in container.get("env", []):
            name = entry.get("name", "")
            reference = entry.get("valueFrom", {}).get("secretKeyRef")
            if reference:
                secret_references.append({
                    "setting": name,
                    "secret_id": reference.get("name"),
                    "version": reference.get("key"),
                })
            value = entry.get("value")
            if not isinstance(value, str):
                continue
            target = database_target(name, value)
            if target:
                database_targets.append(target)
            if value and (SENSITIVE_NAME.search(name) or target):
                plaintext_credentials.add(name)
            if name in SAFE_ENVIRONMENT:
                # Even an allowlisted setting must not export URI userinfo.
                try:
                    contains_userinfo = bool(urlsplit(value).username)
                except ValueError:
                    contains_userinfo = True
                environment[name] = "<credential-bearing value omitted>" if contains_userinfo or target else value
    if environment.get("DB_HOST") and environment.get("DB_HOST") != "<credential-bearing value omitted>":
        database_targets.append({
            "setting": "DB_HOST/DB_NAME",
            "host": environment["DB_HOST"],
            "database": environment.get("DB_NAME"),
        })
    status = service.get("status", {})
    traffic = [{
        key: allocation[key]
        for key in ("revisionName", "percent", "latestRevision", "tag")
        if key in allocation
    } for allocation in status.get("traffic", [])]
    return {
        "name": metadata.get("name"),
        "url": public_url(status.get("url", "")),
        "traffic": traffic,
        "identity": spec.get("serviceAccountName"),
        "revision_max_instances": template.get("metadata", {}).get("annotations", {}).get("autoscaling.knative.dev/maxScale"),
        "service_max_instances": metadata.get("annotations", {}).get("run.googleapis.com/maxScale"),
        "request_concurrency": spec.get("containerConcurrency"),
        "images": [container.get("image") for container in spec.get("containers", [])],
        "environment": environment,
        "secret_references": secret_references,
        "plaintext_credential_setting_names": sorted(plaintext_credentials),
        "database_targets": database_targets,
        "serialized_configuration_annotation_names": serialized_configuration_annotations,
        "public_import_config_has_no_literal_credentials": not plaintext_credentials and not serialized_configuration_annotations,
    }


def gcloud_json(arguments: list[str], projection: str = ""):
    executable = shutil.which("gcloud.cmd" if os.name == "nt" else "gcloud")
    if executable is None:
        raise RuntimeError("Install the Google Cloud CLI and sign in before collecting inventory.")
    environment = os.environ.copy()
    environment.setdefault("CLOUDSDK_PYTHON", sys.executable)
    environment["CLOUDSDK_CORE_LOG_HTTP"] = "false"
    formatting = f"json({projection})" if projection else "json"
    result = subprocess.run(
        [executable, *arguments, f"--format={formatting}"],
        capture_output=True, text=True, encoding="utf-8", env=environment,
    )
    if result.returncode:
        # Do not echo raw diagnostic/configuration contents into an artifact.
        raise RuntimeError(f"gcloud {' '.join(arguments[:3])} failed (exit {result.returncode}); check authentication and permissions.")
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError("gcloud did not return JSON; check the local SDK installation.") from error


def collect(project: str, region: str, include_other_services: bool) -> dict:
    project_flag = f"--project={project}"
    region_flag = f"--region={region}"
    sql = gcloud_json([
        "sql", "instances", "describe", "nordikdrive", project_flag,
    ], "name,connectionName,databaseVersion,ipAddresses,settings.tier,settings.databaseFlags,settings.ipConfiguration.ipv4Enabled,settings.ipConfiguration.privateNetwork")
    service_names = {"nordikdriveapi", "nordik-nia-api"}
    if include_other_services:
        resources = gcloud_json(["run", "services", "list", project_flag, region_flag], "metadata.name")
        service_names.update(resource.get("metadata", {}).get("name", "") for resource in resources)
    services = []
    for name in sorted(service_names):
        if not re.fullmatch(r"[a-z][a-z0-9-]{0,62}", name):
            continue
        services.append(safe_service_snapshot(gcloud_json([
            "run", "services", "describe", name, project_flag, region_flag,
        ])))
    scheduler = gcloud_json([
        "scheduler", "jobs", "list", project_flag, f"--location={region}",
    ], "name,schedule,timeZone,state,httpTarget.uri,httpTarget.oidcToken.serviceAccountEmail,httpTarget.oidcToken.audience")
    for job in scheduler:
        target = job.get("httpTarget", {})
        if "uri" in target:
            target["uri"] = public_url(target["uri"])
    addresses = {item.get("ipAddress") for item in sql.get("ipAddresses", [])}
    consumers = [service["name"] for service in services if any(
        target.get("host") in addresses for target in service["database_targets"]
    )]
    secrets = gcloud_json(["secrets", "list", project_flag], "name,createTime")
    for secret in secrets:
        identifier = secret["name"].rsplit("/", 1)[-1]
        secret["versions"] = gcloud_json([
            "secrets", "versions", "list", identifier, project_flag,
        ], "name,state,createTime")
    return {
        "checked_at_utc": datetime.now(timezone.utc).isoformat(),
        "project": project,
        "region": region,
        "scope": "Metadata only. Production resources are never changed; --include-other-services may inspect their metadata for shared-database dependencies.",
        "active_account": gcloud_json(["auth", "list", "--filter=status:ACTIVE"], "account"),
        "sql": sql,
        "services": services,
        "services_with_literal_target_on_this_sql_instance": consumers,
        "scheduler": scheduler,
        "secrets": secrets,
        "capacity_inventory_verified": False,
        "remaining_checks": [
            "Verify deployed pool limits and processes; metadata alone does not prove them.",
            "Include tagged/draining revisions, other regions, jobs and non-Cloud-Run clients.",
            "Database targets stored in secret references require a separate authorized check.",
            "Move credential-bearing literal environment entries to secret references (or remove unused ones) before importing the public service into Terraform state.",
        ],
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", default="planar-ray-472112-e8")
    parser.add_argument("--region", default="us-west1")
    parser.add_argument("--include-other-services", action="store_true")
    parser.add_argument("--output", type=Path, default=Path(__file__).resolve().parents[1] / ".cache/milestone2-live-inventory.json")
    arguments = parser.parse_args()
    if not re.fullmatch(r"[a-z][a-z0-9-]+", arguments.project) or not re.fullmatch(r"[a-z][a-z0-9-]+", arguments.region):
        parser.error("Use a valid project ID and region.")
    try:
        snapshot = collect(arguments.project, arguments.region, arguments.include_other_services)
    except RuntimeError as error:
        print(str(error), file=sys.stderr)
        return 1
    arguments.output.parent.mkdir(parents=True, exist_ok=True)
    arguments.output.write_text(json.dumps(snapshot, indent=2) + "\n", encoding="utf-8")
    print(f"Saved sanitized inventory to {arguments.output}")
    print("No cloud resources, SQL data, credentials or deployment settings were changed.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
