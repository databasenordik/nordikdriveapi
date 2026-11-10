"""Check new platform core coverage from a combined Go profile.

Go can emit the same block once per test binary with -coverpkg=./.... Merge
those observations before counting statements; never average package percents.
Reused monolith domain code and process entrypoints are reported separately.
"""

import pathlib
import sys


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: python scripts/check-platform-coverage.py PROFILE")
    blocks = {}
    for line in pathlib.Path(sys.argv[1]).read_text().splitlines()[1:]:
        location, statements, executions = line.rsplit(" ", 2)
        statements, executions = int(statements), int(executions)
        if location in blocks:
            if blocks[location][0] != statements:
                raise SystemExit("inconsistent coverage blocks; regenerate the profile")
            blocks[location][1] |= executions > 0
        else:
            blocks[location] = [statements, executions > 0]

    groups = {
        "gateway library": lambda path: "/internal/gateway/" in path,
        "notifications service": lambda path: "/internal/notifications/" in path,
        "new worker core": lambda path: any(
            path.endswith("/internal/jobs/" + name + ".go")
            for name in ("runner", "http", "registry")
        ),
    }
    failed = False
    for name, includes in groups.items():
        selected = [
            value
            for location, value in blocks.items()
            if includes(location.rsplit(":", 1)[0].replace("\\", "/"))
        ]
        total = sum(count for count, _ in selected)
        covered = sum(count for count, hit in selected if hit)
        percent = 100 * covered / total if total else 0
        print(f"{name}: {percent:.1f}% ({covered}/{total} statements)")
        failed |= not total or covered * 100 < total * 90
    if failed:
        raise SystemExit("each new platform core requires 90% combined coverage")


if __name__ == "__main__":
    main()
