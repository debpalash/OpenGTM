"""CLI validation entry point for community connector packages."""
import argparse
import json
from pathlib import Path

from .manifest import MANIFESTS_DIR, validate_manifest_directory


def main() -> int:
    parser = argparse.ArgumentParser(description="Validate OpenGTM connector manifests")
    parser.add_argument("path", nargs="?", type=Path, default=MANIFESTS_DIR)
    parser.add_argument("--json", action="store_true", dest="as_json")
    parser.add_argument("--signature-policy", choices=["optional", "required"], default=None)
    parser.add_argument("--trust-store", type=Path, default=None)
    args = parser.parse_args()
    report = validate_manifest_directory(args.path, signature_policy=args.signature_policy, trust_store=args.trust_store)
    if args.as_json:
        print(json.dumps(report, indent=2))
    else:
        print(f"{'PASS' if report['ok'] else 'FAIL'}: {report['count']} compatible connector(s)")
        for error in report["errors"]:
            print(f"- {error['path']}: {error['error']}")
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
