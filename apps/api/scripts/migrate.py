"""Apply the production database schema and exit.

Deployments run this as a one-shot job before starting API and worker replicas,
so migrations have a single owner and never race during multi-process startup.
"""

from apps.api.db_init import init_db
from apps.api.scripts.provision_runtime_role import provision_runtime_role


def main() -> None:
    init_db()
    provision_runtime_role()


if __name__ == "__main__":
    main()
