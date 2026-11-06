"""Apply the existing Milestone 2 SQL files to the verified development database.

Default: show the target and files. --apply executes them using the existing
owner login in an ephemeral PostgreSQL client container. No password is printed,
put on the command line, or written to disk. Requires the database Terraform stage.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

from milestone2_seed_secrets import cloud, read_payload

ROOT = Path(__file__).resolve().parents[1]
FILES = ("002_legacy_descriptions.up.sql", "002_platform.up.sql", "002_platform_grants.sql")
PROJECT = "planar-ray-472112-e8"


def validate_users(users):
    for owner in ("gateway", "legacy", "worker"):
        expected = f"nordikdriveapi-{owner}@{PROJECT}.iam"
        if users.get(owner) != expected:
            raise RuntimeError("IAM database usernames do not match the development deployment")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    terraform = shutil.which("terraform") or str(ROOT / ".cache/milestone2-small-db-tools/terraform.exe")
    result = subprocess.run([terraform, f"-chdir={ROOT / 'infra/terraform'}", "output", "-json", "database_users"], capture_output=True)
    if result.returncode:
        raise RuntimeError("Apply the database Terraform stage before preparing migrations")
    users = json.loads(result.stdout)
    validate_users(users)
    for name in FILES:
        if not (ROOT / "migrations" / name).is_file():
            raise RuntimeError("A required migration file is missing")
    print("Development target: nordikdrive / postgres / 34.11.220.37")
    for name in FILES:
        print("Migration:", name)
    if not args.apply:
        print("Read-only preparation. --apply executes the listed files after the backup step.")
        return

    service = json.loads(cloud("run", "services", "describe", "nordikdriveapi", "--region=us-west1", "--format=json"))
    entries = {entry["name"]: entry for entry in service["spec"]["template"]["spec"]["containers"][0].get("env", [])}
    if (entries.get("DB_HOST", {}).get("value") != "34.11.220.37"
            or entries.get("DB_NAME", {}).get("value") != "postgres"
            or entries.get("DB_USER", {}).get("value") != "postgres"):
        raise RuntimeError("The API no longer exposes the verified development owner connection; no migration ran")
    password = read_payload(entries["DB_PASSWORD"]).decode("utf-8")
    if not password:
        raise RuntimeError("The development owner credential is empty")
    environment = dict(os.environ, PGPASSWORD=password)
    for name in FILES:
        command = ["docker", "run", "--rm", "--env", "PGPASSWORD", "--env", "PGSSLMODE=require",
                   "--env", "PGCONNECT_TIMEOUT=10", "--mount", f"type=bind,source={ROOT / 'migrations'},target=/migrations,readonly",
                   "postgres:15", "psql", "-h", "34.11.220.37", "-p", "5432", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1"]
        if name == "002_platform_grants.sql":
            for owner in ("gateway", "legacy", "worker"):
                command += ["-v", f"{owner}_user={users[owner]}"]
        command += ["-f", f"/migrations/{name}"]
        completed = subprocess.run(command, env=environment, capture_output=True)
        if completed.returncode:
            raise RuntimeError(f"Migration stopped at {name}; later files were not run. Inspect schema status before retrying")
        print("Applied:", name)


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
    except (KeyError, ValueError, OSError):
        print("Migration preparation stopped; verify local tools, state and development settings. No credentials were logged.", file=sys.stderr)
        sys.exit(1)
