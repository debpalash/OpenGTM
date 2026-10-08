# Reply to issue 20 (draft, not sent)

Edit before sending. It commits you publicly to the direction and to opening pull
requests. Replace each `[link]` once the issue or document exists.

---

Thanks for the thorough reports, and for raising this before writing code.

I reproduced all five bugs on main, and the fixes are ready with regression tests. They will ship as a patch release on their own. Your read of the model is accurate: the lead ID is still the join key for audiences, watches, signals, destinations, outreach and the MCP tools.

To your questions:

1. **Feasible?** Yes, in stages. The entity tables, employment history, and merge and undo exist. The work is an expand and contract migration that keeps Lead as a compatibility view, with each phase behind a flag and reversible.
2. **Direction?** Yes. Shared Company and Person records with independent enrichment, monitoring and activation is where I want OpenGTM to go.
3. **Collaboration?** Yes, and I would stay the main maintainer, as you suggest.

Proposed next steps:

- Review the bug-fix pull requests [link]. You know these paths best.
- Read the design draft [link] and answer the four questions at the end of it.
- Pick a first deliverable. Two good candidates are person merge and alias decisions [link], and read-only Companies and People screens [link].
- I am adding CI so pull requests get tests before anything lands [link].

No schema changes until we agree the design.
