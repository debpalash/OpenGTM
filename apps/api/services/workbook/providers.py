"""
Provider Registry — Maps provider names to EnrichmentProvider instances.

This is the glue between the Workbook column config (which stores provider names
as strings) and the actual provider implementations. When a workbook cell needs
enrichment, the registry resolves the provider name to a callable instance.
"""

import logging
from typing import Dict, List, Optional
from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult

logger = logging.getLogger("workbook.providers")


# ── Global Registry ──────────────────────────────────────────────────────

_registry: Dict[str, EnrichmentProvider] = {}


def register_provider(provider: EnrichmentProvider):
    """Register a provider instance in the global registry."""
    _registry[provider.name] = provider
    logger.info(f"Registered provider: {provider.name} (capabilities: {provider.capabilities})")


def get_provider(name: str) -> Optional[EnrichmentProvider]:
    """Get a provider by name. Returns None if not found."""
    return _registry.get(name)


def list_providers() -> List[dict]:
    """List all registered providers with their metadata."""
    return [
        {
            "name": p.name,
            "capabilities": p.capabilities,
            "confidence": p.default_confidence,
        }
        for p in _registry.values()
    ]


def get_providers_for_capability(capability: str) -> List[EnrichmentProvider]:
    """Get all providers that can provide a specific field."""
    return [p for p in _registry.values() if p.can_provide(capability)]


# ── Auto-register existing providers ──────────────────────────────────────

def _init_providers():
    """Initialize and register all built-in providers."""
    try:
        from apps.api.services.leadgen.enrichment.providers.mailscout_verify import MailScoutVerifyProvider
        register_provider(MailScoutVerifyProvider())
    except Exception as e:
        logger.warning(f"Failed to register mailscout: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.crosslinked import CrossLinkedProvider
        register_provider(CrossLinkedProvider())
    except Exception as e:
        logger.warning(f"Failed to register crosslinked: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.facebook_pages import FacebookPageProvider
        register_provider(FacebookPageProvider())
    except Exception as e:
        logger.warning(f"Failed to register facebook_pages: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.jobspy_signals import JobSpySignalProvider
        register_provider(JobSpySignalProvider())
    except Exception as e:
        logger.warning(f"Failed to register jobspy_signals: {e}")

    # ── BYOK API Providers (existing code, now registered) ──

    try:
        from apps.api.services.leadgen.enrichment.providers.hunter_io import HunterProvider
        register_provider(HunterProvider())
    except Exception as e:
        logger.warning(f"Failed to register hunter_io: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.apollo_io import ApolloProvider
        register_provider(ApolloProvider())
    except Exception as e:
        logger.warning(f"Failed to register apollo_io: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.abstract_api import AbstractAPIProvider
        register_provider(AbstractAPIProvider())
    except Exception as e:
        logger.warning(f"Failed to register abstract_api: {e}")

    # ── Wrapped Scraper Providers (existing modules as EnrichmentProvider) ──

    try:
        from apps.api.services.leadgen.enrichment.providers.ddg_email_provider import DDGEmailProvider
        register_provider(DDGEmailProvider())
    except Exception as e:
        logger.warning(f"Failed to register ddg_email: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.website_scraper_provider import WebsiteScraperProvider
        register_provider(WebsiteScraperProvider())
    except Exception as e:
        logger.warning(f"Failed to register website_scraper: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.social_finder_provider import SocialFinderProvider
        register_provider(SocialFinderProvider())
    except Exception as e:
        logger.warning(f"Failed to register social_finder: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.decision_maker_provider import DecisionMakerProvider
        register_provider(DecisionMakerProvider())
    except Exception as e:
        logger.warning(f"Failed to register decision_maker: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.ddg_company_provider import DDGCompanyProvider
        register_provider(DDGCompanyProvider())
    except Exception as e:
        logger.warning(f"Failed to register ddg_company: {e}")

    # ── BYOK API Providers (new implementations) ──

    try:
        from apps.api.services.leadgen.enrichment.providers.numverify import NumVerifyProvider
        register_provider(NumVerifyProvider())
    except Exception as e:
        logger.warning(f"Failed to register numverify: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.ipinfo import IPInfoProvider
        register_provider(IPInfoProvider())
    except Exception as e:
        logger.warning(f"Failed to register ipinfo: {e}")

    # ── Internal Utility Providers ──

    try:
        from apps.api.services.leadgen.enrichment.providers.lead_scorer_provider import LeadScorerProvider
        register_provider(LeadScorerProvider())
    except Exception as e:
        logger.warning(f"Failed to register lead_scorer: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.tech_stack_provider import TechStackProvider
        register_provider(TechStackProvider())
    except Exception as e:
        logger.warning(f"Failed to register tech_stack: {e}")

    # ── Phase 1: Clay-tier API Providers (free tiers available) ──

    try:
        from apps.api.services.leadgen.enrichment.providers.snovio import SnovioProvider
        register_provider(SnovioProvider())
    except Exception as e:
        logger.warning(f"Failed to register snovio: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.people_data_labs import PeopleDataLabsProvider
        register_provider(PeopleDataLabsProvider())
    except Exception as e:
        logger.warning(f"Failed to register people_data_labs: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.prospeo import ProspeoProvider
        register_provider(ProspeoProvider())
    except Exception as e:
        logger.warning(f"Failed to register prospeo: {e}")

    # ── Phase 2: Deep Scrapers + Cheap APIs ──

    try:
        from apps.api.services.leadgen.enrichment.providers.deep_scraper import DeepScraperProvider
        register_provider(DeepScraperProvider())
    except Exception as e:
        logger.warning(f"Failed to register deep_scraper: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.google_maps import GoogleMapsProvider
        register_provider(GoogleMapsProvider())
    except Exception as e:
        logger.warning(f"Failed to register google_maps: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.debounce import DebounceProvider
        register_provider(DebounceProvider())
    except Exception as e:
        logger.warning(f"Failed to register debounce: {e}")

    # ── Phase 3: OSS-Powered Scrapers (zero cost, no API keys) ──

    try:
        from apps.api.services.leadgen.enrichment.providers.holehe_verify import HoleheProvider
        register_provider(HoleheProvider())
    except Exception as e:
        logger.warning(f"Failed to register holehe: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.email_harvester import EmailHarvesterProvider
        register_provider(EmailHarvesterProvider())
    except Exception as e:
        logger.warning(f"Failed to register email_harvester: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.company_intel import CompanyIntelProvider
        register_provider(CompanyIntelProvider())
    except Exception as e:
        logger.warning(f"Failed to register company_intel: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.local_business import LocalBusinessProvider
        register_provider(LocalBusinessProvider())
    except Exception as e:
        logger.warning(f"Failed to register local_business: {e}")

    # ── Phase 4: Free firmographics + roster (from research/ goldmine clones) ──

    try:
        from apps.api.services.leadgen.enrichment.providers.jsonld_firmographics import JsonLdFirmographicsProvider
        register_provider(JsonLdFirmographicsProvider())
    except Exception as e:
        logger.warning(f"Failed to register jsonld_firmographics: {e}")

    try:
        # Inert unless STAFFSPY_SESSION_FILE is set + `pip install staffspy`.
        from apps.api.services.leadgen.enrichment.providers.staffspy_provider import StaffSpyRosterProvider
        register_provider(StaffSpyRosterProvider())
    except Exception as e:
        logger.warning(f"Failed to register staffspy: {e}")

    logger.info(f"Provider registry initialized: {len(_registry)} providers loaded")


# Auto-initialize on import
_init_providers()
