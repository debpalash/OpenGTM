"""Detached Ed25519 signatures for declarative connector manifests."""

import base64
import hashlib
import json
from pathlib import Path

import yaml
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

SIGNATURE_VERSION = "1"


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
