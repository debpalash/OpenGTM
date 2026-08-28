from __future__ import annotations

import asyncio

import httpx

from apps.api.services.leadgen.enrichment.providers.wikidata_provider import (
    WikidataProvider,
)


def _route_client(monkeypatch, handler):
    real_init = httpx.AsyncClient.__init__

    def init(client, *args, **kwargs):
        kwargs["transport"] = httpx.MockTransport(handler)
        return real_init(client, *args, **kwargs)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)


def test_resolve_identity_requires_exact_org_and_extracts_official_domain(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        action = request.url.params.get("action")
        if action == "wbsearchentities":
            return httpx.Response(
                200,
                json={
                    "search": [
                        {
                            "id": "Q_LOOKALIKE",
                            "label": "Stripe Theory",
                            "description": "communications company",
                        },
                        {
                            "id": "Q_STRIPE",
                            "label": "Stripe, Inc.",
                            "description": "financial technology company",
                        },
                    ]
                },
            )
        return httpx.Response(
            200,
            json={
                "entities": {
                    "Q_STRIPE": {
                        "claims": {
                            "P856": [
                                {
                                    "mainsnak": {
                                        "datavalue": {
                                            "value": "https://stripe.com/"
                                        }
                                    }
                                }
                            ]
                        }
                    }
                }
            },
        )

    _route_client(monkeypatch, handler)
    result = asyncio.run(WikidataProvider().resolve_identity("Stripe"))

    assert result["status"] == "resolved"
    assert result["canonical_domain"] == "stripe.com"
    assert result["entity_id"] == "Q_STRIPE"
    assert result["evidence_url"] == "https://www.wikidata.org/wiki/Q_STRIPE"


def test_resolve_identity_returns_ambiguous_without_guessing(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.params.get("action") == "wbsearchentities"
        return httpx.Response(
            200,
            json={
                "search": [
                    {"id": "Q1", "label": "Mercury", "description": "software company"},
                    {"id": "Q2", "label": "Mercury", "description": "financial company"},
                ]
            },
        )

    _route_client(monkeypatch, handler)
    result = asyncio.run(WikidataProvider().resolve_identity("Mercury"))

    assert result == {"status": "ambiguous", "canonical_domain": ""}
