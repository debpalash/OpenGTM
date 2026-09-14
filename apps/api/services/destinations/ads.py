"""Consent-aware paid-media audience drivers.

Only normalized, SHA-256 identifiers leave OpenGTM. Credentials are resolved
from the workspace secret store and are never persisted with a destination.
"""

import hashlib
import json
import re
from dataclasses import dataclass

import httpx

from apps.api.services.workspace.secrets import get_secret


AD_DESTINATION_TYPES = {"meta_ads", "google_ads", "linkedin_ads"}


@dataclass
class AdBatchResult:
    success: bool
    summary: str
    error: str | None = None
    external_id: str | None = None


def hash_email(value: str) -> str:
    normalized = "".join(str(value or "").split()).lower()
    return hashlib.sha256(normalized.encode()).hexdigest() if "@" in normalized else ""


def hash_phone(value: str) -> str:
    normalized = re.sub(r"\D", "", str(value or ""))
    return hashlib.sha256(normalized.encode()).hexdigest() if 7 <= len(normalized) <= 15 else ""


def hashed_identifiers(snapshot: dict) -> dict[str, str]:
    identifiers = {}
    email = hash_email(snapshot.get("email", ""))
    phone = hash_phone(snapshot.get("phone", "") or snapshot.get("mobile_phone", ""))
    if email:
        identifiers["email"] = email
    if phone:
        identifiers["phone"] = phone
    return identifiers


def _secret(workspace_id: str, key: str) -> str:
    value = get_secret(workspace_id, key)
    if not value:
        raise RuntimeError(f"missing workspace secret {key}")
    return value


async def _request(client: httpx.AsyncClient, method: str, url: str, **kwargs) -> httpx.Response:
    response = await client.request(method, url, **kwargs)
    if response.status_code in {401, 403}:
        raise RuntimeError(f"platform authorization or API approval required ({response.status_code})")
    response.raise_for_status()
    return response


async def sync_ad_batch(workspace_id: str, dtype: str, config: dict, snapshots: list[dict], operation: str = "add") -> AdBatchResult:
    """Upload one bounded batch and return one result shared by its deliveries."""
    if operation not in {"add", "remove"}:
        raise ValueError("ad sync operation must be add or remove")
    operation_past = "added" if operation == "add" else "removed"
    rows = []
    for snapshot in snapshots:
        row = hashed_identifiers(snapshot)
        # REMOVE reconciliation reads hashes persisted by the successful ADD.
        # Accept those exact SHA-256 values without hashing them a second time.
        if operation == "remove":
            for key in ("email", "phone"):
                value = str(snapshot.get(key) or "").strip().lower()
                if re.fullmatch(r"[0-9a-f]{64}", value):
                    row[key] = value
        rows.append(row)
    rows = [row for row in rows if row]
    if not rows:
        return AdBatchResult(False, "", "no valid email or phone identifiers")

    async with httpx.AsyncClient(timeout=30) as client:
        if dtype == "meta_ads":
            token = _secret(workspace_id, "META_ACCESS_TOKEN")
            audience_id = config["custom_audience_id"]
            schema = [key.upper() for key in ("email", "phone") if any(key in row for row in rows)]
            data = [[row.get(key.lower(), "") for key in schema] for row in rows]
            response = await _request(
                client, "POST" if operation == "add" else "DELETE", f"https://graph.facebook.com/{config.get('api_version', 'v23.0')}/{audience_id}/users",
                data={"access_token": token, "payload": json.dumps({"schema": schema, "data": data})},
            )
            body = response.json()
            return AdBatchResult(True, f"{operation_past} {len(rows)} hashed users", external_id=str(body.get("session_id") or audience_id))

        if dtype == "linkedin_ads":
            token = _secret(workspace_id, "LINKEDIN_ACCESS_TOKEN")
            segment_id = config["segment_id"]
            elements = [{"action": "ADD" if operation == "add" else "REMOVE", "userIds": [
                {"idType": "SHA256_EMAIL", "idValue": row["email"]}
            ]} for row in rows if row.get("email")]
            if not elements:
                return AdBatchResult(False, "", "LinkedIn requires a valid email identifier")
            await _request(client, "POST", f"https://api.linkedin.com/rest/dmpSegments/{segment_id}/users", headers={
                "Authorization": f"Bearer {token}", "Linkedin-Version": config.get("api_version", "202607"),
                "X-Restli-Protocol-Version": "2.0.0", "X-RestLi-Method": "BATCH_CREATE",
            }, json={"elements": elements})
            return AdBatchResult(True, f"{operation_past} {len(elements)} hashed users", external_id=str(segment_id))

        if dtype == "google_ads":
            token = _secret(workspace_id, "GOOGLE_ADS_ACCESS_TOKEN")
            developer_token = _secret(workspace_id, "GOOGLE_ADS_DEVELOPER_TOKEN")
            customer_id, user_list_id = config["customer_id"], config["user_list_id"]
            version = config.get("api_version", "v23")
            base = f"https://googleads.googleapis.com/{version}/customers/{customer_id}"
            headers = {"Authorization": f"Bearer {token}", "developer-token": developer_token}
            login_id = config.get("login_customer_id")
            if login_id:
                headers["login-customer-id"] = login_id
            created = await _request(client, "POST", f"{base}/offlineUserDataJobs:create", headers=headers, json={
                "job": {"type": "CUSTOMER_MATCH_USER_LIST", "customerMatchUserListMetadata": {
                    "userList": f"customers/{customer_id}/userLists/{user_list_id}",
                    "consent": {"adUserData": "GRANTED", "adPersonalization": "GRANTED"},
                }}
            })
            job_name = created.json()["resourceName"]
            operation_key = "create" if operation == "add" else "remove"
            operations = [{operation_key: {"userIdentifiers": [
                *([{"hashedEmail": row["email"]}] if row.get("email") else []),
                *([{"hashedPhoneNumber": row["phone"]}] if row.get("phone") else []),
            ]}} for row in rows]
            await _request(client, "POST", f"https://googleads.googleapis.com/{version}/{job_name}:addOperations", headers=headers,
                           json={"enablePartialFailure": True, "operations": operations})
            await _request(client, "POST", f"https://googleads.googleapis.com/{version}/{job_name}:run", headers=headers, json={})
            return AdBatchResult(True, f"queued {operation} for {len(rows)} hashed users", external_id=job_name)

    return AdBatchResult(False, "", f"unsupported ad destination '{dtype}'")
