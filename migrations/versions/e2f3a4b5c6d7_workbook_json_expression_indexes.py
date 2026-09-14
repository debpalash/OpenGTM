"""Index workbook JSON search and common saved-view sort expressions.

Revision ID: e2f3a4b5c6d7
Revises: d1e2f3a4b5c6
"""

from alembic import op
import sqlalchemy as sa


revision = "e2f3a4b5c6d7"
down_revision = "d1e2f3a4b5c6"
branch_labels = None
depends_on = None

SEARCH_INDEXES = (
    ("ix_workbook_rows_data_trgm", "data"),
    ("ix_workbook_rows_enrichments_trgm", "enrichments"),
)

# These are the fields used most often for workbook identity, segmentation, and
# activation.  The trailing position/id terms let PostgreSQL satisfy stable
# keyset ordering directly from the index after selecting a workbook.
SORT_FIELDS = (
    "company",
    "website",
    "email",
    "contact_person",
    "contact_title",
    "city",
    "state",
    "company_size",
    "score",
    "score_tier",
    "status",
    "source",
)


def upgrade() -> None:
    if op.get_bind().dialect.name != "postgresql":
        return

    # pg_trgm preserves the existing literal-substring search semantics while
    # making leading-wildcard JSON searches indexable.
    op.execute(sa.text("CREATE EXTENSION IF NOT EXISTS pg_trgm"))
    for index_name, column_name in SEARCH_INDEXES:
        op.execute(sa.text(
            f'CREATE INDEX "{index_name}" ON workbook_rows '
            f'USING gin (lower(CAST("{column_name}" AS text)) gin_trgm_ops)'
        ))
    for field in SORT_FIELDS:
        op.execute(sa.text(
            f'CREATE INDEX "ix_workbook_rows_sort_{field}" ON workbook_rows '
            f'(workbook_id, (COALESCE(data ->> \'{field}\', \'\')), position, id)'
        ))


def downgrade() -> None:
    if op.get_bind().dialect.name != "postgresql":
        return
    for field in reversed(SORT_FIELDS):
        op.execute(sa.text(f'DROP INDEX IF EXISTS "ix_workbook_rows_sort_{field}"'))
    for index_name, _ in reversed(SEARCH_INDEXES):
        op.execute(sa.text(f'DROP INDEX IF EXISTS "{index_name}"'))
