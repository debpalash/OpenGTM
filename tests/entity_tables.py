"""Tables that saving people touches (person identity + company lookup)."""

from apps.api.services.entities.models import (
    CompanyEntity, CompanyIdentifier, PersonEmployment, PersonEntity, PersonIdentifier,
)

PERSON_TABLES = [
    CompanyEntity.__table__, CompanyIdentifier.__table__, PersonEntity.__table__,
    PersonIdentifier.__table__, PersonEmployment.__table__,
]
