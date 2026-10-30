# OpenGTM records

OpenGTM maintains a shared company and people warehouse. Workbooks research and enrich those records; audiences select them for monitoring and activation.

## Language

**Company**: An organization with an identity and accepted company facts, whether or not any people have been discovered.
_Avoid_: Lead when referring to an organization.

**Person**: An individual with an identity and accepted personal facts that survive changes of employer.
_Avoid_: Contact when that would imply identity belongs to one company.

**Employment**: A time-bounded role belonging to a Person, linked to a Company when the employer is known. Multiple observations and previous roles remain part of its history.

**Accepted fact**: The current value of a field owned by a Company, Person, or Employment. A manually accepted value takes precedence over an unreviewed imported or provider value.

**Observation**: A source's assertion about a fact, including when and where it was observed. An observation can be retained without replacing an accepted fact.

**Workbook row**: A reference to a shared Company or Person, together with local research, formulas, and execution history. Shared facts belong to the referenced record.

**Audience**: A saved selection of Companies or People with membership history. Membership refers to the shared record's identity.

**Legacy Lead**: A compatibility record from the earlier combined company/contact model. Its identifier remains useful for historical jobs and deliveries; it does not own shared facts.

**External identity**: A source-scoped identifier linking an imported row to a Company or Person. A Clay row identifier is unique within its source table, not across a whole workspace.

**Migration preview**: A read-only explanation of how imported columns, identities, company relationships, and conflicts will be handled before records are written.
