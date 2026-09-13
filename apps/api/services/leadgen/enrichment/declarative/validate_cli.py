"""CLI validation entry point for community connector packages."""
import argparse
import json
from pathlib import Path

from .manifest import MANIFESTS_DIR, validate_manifest_directory


def main() -> int:
    parser = argparse.ArgumentParser(description="Validate OpenGTM connector manifests")
    parser.add_argument("path", nargs="?", type=Path, default=MANIFESTS_DIR)
    parser.add_argument("--json", action="store_true", dest="as_json")
    args = parser.parse_args()
    report = validate_manifest_directory(args.path)
    if args.as_json:
        print(json.dumps(report, indent=2))
    else:
        print(f"{'PASS' if report['ok'] else 'FAIL'}: {report['count']} compatible connector(s)")
        for error in report["errors"]:
            print(f"- {error['path']}: {error['error']}")
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
