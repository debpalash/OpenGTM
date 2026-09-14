"""Detached Ed25519 signatures for declarative connector manifests."""

import base64
import hashlib
import json
import os
import re
import secrets
import tempfile
import zipfile
from pathlib import Path

import yaml
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

SIGNATURE_VERSION = "1"
BUNDLE_FILES = {"connector.yaml", "connector.yaml.sig"}


def canonical_manifest(path: Path) -> bytes:
    raw = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not isinstance(raw, dict):
        raise ValueError("manifest is not a mapping")
    return json.dumps(raw, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def load_trust_store(path: Path) -> dict[str, dict]:
    if not path.exists(): return {}
    raw = json.loads(path.read_text(encoding="utf-8"))
    if raw.get("version") != 1 or not isinstance(raw.get("keys"), list): raise ValueError("invalid connector trust store")
    return {item["key_id"]: item for item in raw["keys"]}


def sign_manifest(path: Path, private_key_path: Path, key_id: str) -> Path:
    key = serialization.load_pem_private_key(private_key_path.read_bytes(), password=None)
    if not isinstance(key, Ed25519PrivateKey): raise ValueError("connector signing key must be Ed25519")
    payload = canonical_manifest(path); signature = key.sign(payload)
    envelope = {"signature_version": SIGNATURE_VERSION, "algorithm": "Ed25519", "key_id": key_id, "manifest_sha256": hashlib.sha256(payload).hexdigest(), "signature": base64.b64encode(signature).decode("ascii")}
    output = Path(f"{path}.sig")
    output.write_text(json.dumps(envelope, sort_keys=True, indent=2) + "\n", encoding="utf-8")
    return output


def verify_manifest(path: Path, trust_store_path: Path) -> dict:
    signature_path = Path(f"{path}.sig")
    if not signature_path.exists(): return {"status": "unsigned", "key_id": None, "publisher": None}
    try:
        envelope = json.loads(signature_path.read_text(encoding="utf-8")); key_id = envelope.get("key_id")
        if envelope.get("signature_version") != SIGNATURE_VERSION or envelope.get("algorithm") != "Ed25519": raise ValueError("unsupported connector signature envelope")
        trusted = load_trust_store(trust_store_path); entry = trusted.get(key_id)
        if entry is None: return {"status": "untrusted", "key_id": key_id, "publisher": None}
        payload = canonical_manifest(path)
        if not hashlib.sha256(payload).hexdigest() == envelope.get("manifest_sha256"): raise ValueError("manifest digest mismatch")
        public_key = Ed25519PublicKey.from_public_bytes(base64.b64decode(entry["public_key"], validate=True))
        public_key.verify(base64.b64decode(envelope["signature"], validate=True), payload)
        return {"status": "trusted", "key_id": key_id, "publisher": entry.get("publisher", key_id)}
    except Exception as exc:
        return {"status": "invalid", "key_id": locals().get("key_id"), "publisher": None, "error": str(exc)}


def package_manifest(path: Path, output: Path | None = None) -> Path:
    signature_path = Path(f"{path}.sig")
    if not signature_path.exists(): raise ValueError("sign the connector before packaging it")
    output = output or path.with_suffix(".ogc")
    entries = (("connector.yaml", path.read_bytes()), ("connector.yaml.sig", signature_path.read_bytes()))
    with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, content in entries:
            info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0)); info.compress_type = zipfile.ZIP_DEFLATED; info.external_attr = 0o100644 << 16
            archive.writestr(info, content)
    return output


def install_bundle(bundle: Path, destination: Path, trust_store: Path, *, replace: bool = False) -> dict:
    from .manifest import load_manifest, validate_manifest_directory
    with zipfile.ZipFile(bundle, "r") as archive:
        names = {item.filename for item in archive.infolist()}
        if names != BUNDLE_FILES: raise ValueError("connector bundle must contain only connector.yaml and connector.yaml.sig")
        if any(item.file_size > 1_000_000 or item.compress_size > 1_000_000 for item in archive.infolist()): raise ValueError("connector bundle exceeds the 1 MB file limit")
        manifest_bytes = archive.read("connector.yaml"); signature_bytes = archive.read("connector.yaml.sig")
    with tempfile.TemporaryDirectory(prefix="opengtm-connector-") as temporary:
        stage = Path(temporary); manifest_path = stage / "connector.yaml"; signature_path = stage / "connector.yaml.sig"
        manifest_path.write_bytes(manifest_bytes); signature_path.write_bytes(signature_bytes)
        result = verify_manifest(manifest_path, trust_store)
        if result["status"] != "trusted": raise ValueError(f"connector signature is {result['status']}")
        report = validate_manifest_directory(stage, signature_policy="required", trust_store=trust_store)
        if not report["ok"]: raise ValueError(report["errors"][0]["error"])
        manifest = load_manifest(manifest_path)
        if not re.fullmatch(r"[a-z][a-z0-9_]{1,63}", manifest.capability): raise ValueError("connector capability is not a safe package path")
        target_dir = destination / manifest.capability; target = target_dir / f"{manifest.name}.yaml"; target_signature = Path(f"{target}.sig")
        if (target.exists() or target_signature.exists()) and not replace: raise FileExistsError(f"connector {manifest.name} is already installed")
        target_dir.mkdir(parents=True, exist_ok=True)
        staged_target = target_dir / f".{manifest.name}.{secrets.token_hex(8)}.yaml"
        staged_signature = Path(f"{staged_target}.sig")
        staged_target.write_bytes(manifest_bytes); staged_signature.write_bytes(signature_bytes)
        previous_manifest = target.read_bytes() if target.exists() else None
        previous_signature = target_signature.read_bytes() if target_signature.exists() else None
        try:
            os.replace(staged_signature, target_signature)
            os.replace(staged_target, target)
        except Exception:
            # The manifest and detached signature are one logical unit but need
            # two filesystem moves. Restore the exact prior pair (or remove a
            # half-installed new pair) if either move fails.
            for path, previous in (
                (target, previous_manifest),
                (target_signature, previous_signature),
            ):
                if previous is None:
                    path.unlink(missing_ok=True)
                else:
                    path.write_bytes(previous)
            raise
        finally:
            staged_target.unlink(missing_ok=True)
            staged_signature.unlink(missing_ok=True)
        return {"id": manifest.name, "capability": manifest.capability, "manifest": str(target), "signature": result}
