"""Translate provider outcomes into explicitly labeled settlement inputs."""
from copy import deepcopy


class UncertainProviderAccounting(ValueError):
    """The response does not establish whether a paid lookup was charged."""


def accounting_envelope(provider: str, response: dict | None, estimated_microusd: int) -> dict:
    if type(estimated_microusd) is not int or not 0 <= estimated_microusd <= 2**63 - 1:
        raise ValueError("Invalid catalog exposure")
    if not isinstance(response, dict) or response.get("provider") != provider or type(response.get("success")) is not bool:
        raise UncertainProviderAccounting("Missing or mismatched provider result")
    evidence = response.get("billing_evidence")
    if evidence is not None:
        if not isinstance(evidence, dict):
            raise UncertainProviderAccounting("Invalid billing evidence")
        amount, reference = evidence.get("charged_microusd"), evidence.get("reference")
        if (type(amount) is not int or not 0 <= amount <= 2**63 - 1 or
                not isinstance(reference, str) or not reference.strip()):
            raise UncertainProviderAccounting("Billing evidence needs amount and reference")
        basis = "vendor_confirmed" if amount else "confirmed_nonbillable"
    elif estimated_microusd == 0 or response["success"]:
        amount, basis = estimated_microusd, "catalog_estimate"
    else:
        raise UncertainProviderAccounting("Paid unsuccessful lookup has no billing evidence")
    return {"charged_microusd": amount, "accounting_basis": basis, "result": deepcopy(response)}
