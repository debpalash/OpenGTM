"""
Google Maps / Places Provider — Structured business data from Google.

Replaces 3-4 Clay providers (Google Maps, Google Reviews, location data).
Uses the Google Places API (Text Search → Place Details).

Free tier: $200/month credit (covers ~5,000 lookups/month).
Docs: https://developers.google.com/maps/documentation/places/web-service

Extracts:
  - Verified phone number (formatted)
  - Physical address (formatted)
  - Business hours
  - Rating + review count
  - Website (canonical)
  - Google Maps URL
  - Business type/category
"""

import os
import time
import logging
import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.google_maps")


def _get_api_key() -> str:
    """Read Google Maps/Places API key from settings DB or environment."""
    try:
        from apps.api.database import get_setting
        return get_setting("GOOGLE_MAPS_API_KEY", "") or os.getenv("GOOGLE_MAPS_API_KEY", "")
    except Exception:
        return os.getenv("GOOGLE_MAPS_API_KEY", "")


class GoogleMapsProvider(EnrichmentProvider):
    name = "google_maps"
    capabilities = [
        "phone", "address", "description",
        "industry_tags", "company_size",
    ]
    default_confidence = 0.90  # Google data is highly reliable

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        api_key = _get_api_key()
        if not api_key:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No Google Maps API key configured",
            )

        t0 = time.time()
        company = lead.company or ""
        city = lead.city or ""

        if not company:
            return EnrichmentResult(
                provider=self.name, success=False,
                error="No company name available",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            async with httpx.AsyncClient(timeout=15) as client:
                # Step 1: Text Search to find the business
                search_query = f"{company}"
                if city:
                    search_query += f" {city}"

                resp = await client.post(
                    "https://places.googleapis.com/v1/places:searchText",
                    headers={
                        "Content-Type": "application/json",
                        "X-Goog-Api-Key": api_key,
                        "X-Goog-FieldMask": (
                            "places.id,places.displayName,places.formattedAddress,"
                            "places.internationalPhoneNumber,places.nationalPhoneNumber,"
                            "places.websiteUri,places.googleMapsUri,places.rating,"
                            "places.userRatingCount,places.types,places.primaryType,"
                            "places.regularOpeningHours,places.editorialSummary,"
                            "places.businessStatus"
                        ),
                    },
                    json={"textQuery": search_query, "maxResultCount": 3},
                )

                if resp.status_code != 200:
                    return EnrichmentResult(
                        provider=self.name, success=False,
                        error=f"Google API error: {resp.status_code}",
                        duration_ms=(time.time() - t0) * 1000,
                    )

                places = resp.json().get("places", [])
                if not places:
                    return EnrichmentResult(
                        provider=self.name, success=False,
                        error="No places found",
                        duration_ms=(time.time() - t0) * 1000,
                    )

                # Pick best match (first result is usually best)
                place = places[0]
                fields = {}

                # Phone (international format preferred)
                phone = place.get("internationalPhoneNumber") or place.get("nationalPhoneNumber")
                if phone:
                    fields["phone"] = phone

                # Address
                addr = place.get("formattedAddress")
                if addr:
                    fields["address"] = addr

                # Description
                summary = place.get("editorialSummary", {})
                if isinstance(summary, dict) and summary.get("text"):
                    fields["description"] = summary["text"][:500]

                # Industry from place types
                primary_type = place.get("primaryType", "")
                types = place.get("types", [])
                if primary_type:
                    # Convert snake_case to readable
                    industry = primary_type.replace("_", " ").title()
                    fields["industry_tags"] = industry
                elif types:
                    fields["industry_tags"] = types[0].replace("_", " ").title()

                # Rating info (useful as company signal)
                rating = place.get("rating")
                review_count = place.get("userRatingCount")
                if rating:
                    fields["google_rating"] = f"{rating}/5 ({review_count or 0} reviews)"

                # Google Maps link
                maps_url = place.get("googleMapsUri")
                if maps_url:
                    fields["google_maps_url"] = maps_url

                if fields:
                    return EnrichmentResult(
                        provider=self.name, success=True,
                        fields=fields,
                        confidence=self.default_confidence,
                        duration_ms=(time.time() - t0) * 1000,
                    )

                return EnrichmentResult(
                    provider=self.name, success=False,
                    error="No useful data from Google Places",
                    duration_ms=(time.time() - t0) * 1000,
                )

        except Exception as e:
            logger.warning(f"Google Maps error: {e}")
            return EnrichmentResult(
                provider=self.name, success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
