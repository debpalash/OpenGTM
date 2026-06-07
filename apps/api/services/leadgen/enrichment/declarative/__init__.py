"""
Declarative provider system — add an enrichment provider as a YAML manifest
instead of a bespoke Python client.

Ported from Othmane-Khadri/YALC-the-GTM-operating-system
(src/lib/providers/declarative + capabilities). See
docs/clay-alternatives-ingestion-catalog.md §G / Phase 1.5.

A manifest (auth / endpoint / request.bodyTemplate / response.mappings /
pagination) is compiled into a DeclarativeProvider(EnrichmentProvider) that
plugs into the existing waterfall + provider registry. Manifests live in
`manifests/<capability>/<provider>.yaml`.
"""

from apps.api.services.leadgen.enrichment.declarative.manifest import (
    ProviderManifest,
    load_manifest,
    load_all_manifests,
    MANIFESTS_DIR,
)
from apps.api.services.leadgen.enrichment.declarative.compiler import (
    DeclarativeProvider,
    MissingApiKeyError,
    compile_manifest,
)

__all__ = [
    "ProviderManifest",
    "load_manifest",
    "load_all_manifests",
    "MANIFESTS_DIR",
    "DeclarativeProvider",
    "MissingApiKeyError",
    "compile_manifest",
]
