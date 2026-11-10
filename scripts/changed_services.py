"""Use each service's local Go dependency graph for both build and staging."""
from __future__ import annotations
import argparse
import json
import os
from pathlib import Path, PurePosixPath
import subprocess

SHARED_BUILD_FILES = {"go.mod", "go.sum", ".dockerignore", ".github/services.json",
    "scripts/changed_services.py", ".github/workflows/cd-go.yml",
    ".github/workflows/service-release.yml", ".github/workflows/deploy-runtime.yml"}

def json_stream(raw):
    decoder = json.JSONDecoder()
    while raw.strip():
        raw = raw.lstrip()
        item, offset = decoder.raw_decode(raw)
        yield item
        raw = raw[offset:]

def dependency_graph(root, entrypoint):
    result = subprocess.run(["go", "list", "-deps", "-json", entrypoint], cwd=root,
        env={**os.environ, "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"},
        text=True, encoding="utf-8", capture_output=True, check=True, timeout=120)
    directories, embedded = set(), set()
    for package in json_stream(result.stdout):
        directory = Path(package["Dir"]).resolve()
        try:
            relative = directory.relative_to(root.resolve()).as_posix()
        except ValueError:
            continue
        directories.add(relative)
        for file in package.get("EmbedFiles", []):
            embedded.add((PurePosixPath(relative) / file).as_posix())
    return {"directories": directories, "embedded": embedded}

def affected_services(changed, services, graphs):
    selected = []
    for name, service in services.items():
        graph = graphs[name]
        affected = bool(changed & SHARED_BUILD_FILES) or service["dockerfile"] in changed
        for file in changed:
            if file in graph["embedded"]:
                affected = True
            path = PurePosixPath(file)
            if path.parent.as_posix() in graph["directories"] and path.suffix in {".go", ".c", ".h", ".cc", ".cpp", ".s"} and not file.endswith("_test.go"):
                affected = True
        if affected:
            selected.append({"id": name, **service})
    return selected

def changed_paths(root, base, head):
    if not base or set(base) == {"0"}:
        command = ["git", "ls-tree", "-r", "--name-only", "-z", head]
    else:
        # Both sides of renames/deletions must participate in selection.
        command = ["git", "diff", "--no-renames", "--name-only", "-z", base, head]
    result = subprocess.run(command, cwd=root, capture_output=True, check=True)
    return {file.decode("utf-8") for file in result.stdout.split(b"\0") if file}

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True)
    parser.add_argument("--head", default="HEAD")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    services = json.loads((root / ".github/services.json").read_text())["services"]
    changed = changed_paths(root, args.base, args.head)
    graphs = {name: dependency_graph(root, value["entrypoint"]) for name, value in services.items()}
    selected = affected_services(changed, services, graphs)
    output = "matrix=" + json.dumps({"include": selected}, separators=(",", ":")) + "\nany_changed=" + str(bool(selected)).lower() + "\n"
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as stream:
            stream.write(output)
    print(output, end="")

if __name__ == "__main__":
    main()
