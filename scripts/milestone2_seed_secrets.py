"""Copy the development API's current credentials to Secret Manager without printing payloads.

Dry run by default. --apply creates missing metadata/versions. Runtime access is
managed by Terraform; this helper grants no access to the old shared identity.
It does not update Cloud Run, SQL or traffic. Run locally
as the authorized operator; never redirect raw service descriptions into files.
"""

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

PROJECT = "planar-ray-472112-e8"
REGION = "us-west1"
SERVICE = "nordikdriveapi"
SECRETS = {
    "JWT_SECRET": "nordikdrive-m2-jwt",
    "GMAIL_APP_PASSWORD": "nordikdrive-m2-smtp",
    "ZOOM_CLIENT_SECRET": "nordikdrive-m2-zoom",
    "DB_PASSWORD": "nordikdrive-m2-transition-db",
    "GEMINI_KEY": "nordikdrive-m2-transition-gemini",
    "HONOUR_JOB_SECRET": "nordikdrive-m2-transition-honour",
}


def cloud(*args, payload=None):
    executable = shutil.which("gcloud.cmd") or shutil.which("gcloud")
    if not executable:
        raise RuntimeError("gcloud is not installed or on PATH")
    environment = dict(os.environ, CLOUDSDK_PYTHON=sys.executable,
                       CLOUDSDK_CORE_DISABLE_PROMPTS="1", CLOUDSDK_CORE_LOG_HTTP="false")
    result = subprocess.run([executable, *args, f"--project={PROJECT}", "--quiet"],
                            input=payload, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            env=environment, check=False)
    if result.returncode:
        # SDK diagnostics can contain the resource/configuration. Do not relay them.
        raise RuntimeError(f"gcloud {args[0]} {args[1]} failed; check operator permissions locally")
    return result.stdout


def read_payload(entry):
    if "value" in entry:
        return entry["value"].encode("utf-8")
    reference = entry.get("valueFrom", {}).get("secretKeyRef", {})
    if not reference.get("name") or not reference.get("key"):
        raise RuntimeError("Credential setting has no supported literal value or secret reference")
    return cloud("secrets", "versions", "access", reference["key"],
                 f"--secret={reference['name']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    service = json.loads(cloud("run", "services", "describe", SERVICE,
                               f"--region={REGION}", "--format=json"))
    template = service["spec"]["template"]["spec"]
    entries = {item["name"]: item for item in template["containers"][0].get("env", [])}
    if "JWT_SECRET" not in entries:
        raise RuntimeError("Current JWT_SECRET is missing; cannot preserve existing cookies")
    available = {key: name for key, name in SECRETS.items() if key in entries}
    missing = sorted(set(SECRETS) - set(available))
    print("Development service:", SERVICE)
    print("Copy exact current values for:", ", ".join(available))
    if missing:
        print("Absent settings (not copied):", ", ".join(missing))
    if not args.apply:
        print("Dry run only. Run again with --apply after reviewing these development secret IDs.")
        return

    known = {item["name"].rsplit("/", 1)[-1]
             for item in json.loads(cloud("secrets", "list", "--format=json(name)"))}
    references = {}
    for key, name in available.items():
        payload = read_payload(entries[key])
        if not payload:
            raise RuntimeError(f"{key} is empty; resolve it before copying")
        if name not in known:
            cloud("secrets", "create", name, "--replication-policy=automatic")
        versions = json.loads(cloud("secrets", "versions", "list", name,
                                   "--format=json(name,state)"))
        enabled = sorted((int(item["name"].rsplit("/", 1)[-1]) for item in versions
                          if item.get("state") == "ENABLED"), reverse=True)
        version = next((str(number) for number in enabled
                        if cloud("secrets", "versions", "access", str(number),
                                 f"--secret={name}") == payload), None)
        if version is None:
            # Refuse to silently change a JWT secret already provisioned differently.
            if key == "JWT_SECRET" and versions:
                raise RuntimeError("Existing Milestone 2 JWT versions differ from the current API; resolve before proceeding")
            created = json.loads(cloud("secrets", "versions", "add", name,
                                       "--data-file=-", "--format=json(name)", payload=payload))
            version = created["name"].rsplit("/", 1)[-1]
        references[key] = {"secret_id": name, "version": version}
        print(f"{key} -> {name}:{version}")
    destination = Path(".cache/milestone2-transition-secret-refs.json")
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(references, indent=2) + "\n", encoding="utf-8")
    print("Numeric references only saved to", destination)
    print("Cloud Run configuration and traffic have not been changed.")


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as error:
        # Only fixed, value-free messages created above are surfaced.
        print(str(error), file=sys.stderr)
        sys.exit(1)
    except (KeyError, ValueError):
        # Never include the raw exception from parsing credential-bearing API data.
        print("Secret preparation stopped. Verify current settings and IAM locally; no payloads were logged.", file=sys.stderr)
        sys.exit(1)
