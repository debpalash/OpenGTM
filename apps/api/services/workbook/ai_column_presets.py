"""
AI-column prompt library — ready-to-ship presets for workbook AI columns.

Ported/adapted from forma-norden/clay-claude-code-skill-pack
(clay-ai-column-prompts.md) + sachacoldiq/ColdIQ (claygent-guide.md). See
docs/research/clay-alternatives-ingestion-catalog.md (top-10 #8).

Each preset follows the 5-part structure (ROLE / CONTEXT / TASK / FORMAT /
FALLBACK) and uses an explicit INSUFFICIENT_DATA sentinel so a missing-data
answer is easy to filter — the single biggest quality lever for AI columns.
Prompts reference workbook columns with {placeholder} (resolved per row).

`column_type` maps to Yupcha's column kinds:
  - ai_formula : one LLM call over the row's existing data
  - research   : bounded web-research agent (Claygent-style)
"""

from typing import Dict, List

# Sentinel the prompts emit when the row lacks enough data — filterable in-grid.
INSUFFICIENT = "INSUFFICIENT_DATA"

PRESETS: List[Dict] = [
    {
        "id": "icp_fit_score",
        "name": "ICP Fit Score (0-100)",
        "category": "qualification",
        "column_type": "ai_formula",
        "output_format": "number",
        "description": "Score how well a company matches your ICP. Number only — easy to sort/filter.",
        "prompt": (
            "ROLE: You are a B2B sales qualification analyst.\n"
            "CONTEXT: Company: {company}. Website: {website}. City: {city}. "
            "Size: {company_size}. Description: {description}.\n"
            "TASK: Score 0-100 how well this company fits an ICP of mid-market B2B companies "
            "that would buy sales-intelligence software. Weight: firmographic fit 40, "
            "buying-signal/intent 25, technographic fit 20, trigger events 15.\n"
            "FORMAT: Output ONLY the integer score (0-100). No words, no symbols.\n"
            f"FALLBACK: If there is not enough data to judge, output {INSUFFICIENT}."
        ),
    },
    {
        "id": "company_pain",
        "name": "Likely Pain Points",
        "category": "research",
        "column_type": "ai_formula",
        "output_format": "text",
        "description": "Infer the company's most likely operational pain points from its profile.",
        "prompt": (
            "ROLE: You are a B2B growth strategist.\n"
            "CONTEXT: Company: {company} ({website}). What they do: {description}. "
            "Size: {company_size}.\n"
            "TASK: Identify the 2 most likely operational pain points this specific company faces, "
            "grounded in their business model and size — not generic platitudes.\n"
            "FORMAT: Two short bullet points, max 12 words each.\n"
            f"FALLBACK: If the profile is too thin to infer real pains, output {INSUFFICIENT}."
        ),
    },
    {
        "id": "opening_line",
        "name": "Personalized Opening Line",
        "category": "personalization",
        "column_type": "ai_formula",
        "output_format": "text",
        "description": "A one-sentence cold-email opener referencing something specific about the company.",
        "prompt": (
            "ROLE: You are an expert cold-email copywriter.\n"
            "CONTEXT: Company: {company}. About: {description}. City: {city}. Website: {website}.\n"
            "TASK: Write ONE personalized opening line for a cold email that references something "
            "specific and true about this company. No flattery, no 'I came across your company'.\n"
            "FORMAT: One sentence, max 20 words, plain text.\n"
            f"FALLBACK: If you cannot reference anything specific, output {INSUFFICIENT}."
        ),
    },
    {
        "id": "seniority_tier",
        "name": "Contact Seniority Tier",
        "category": "qualification",
        "column_type": "ai_formula",
        "output_format": "text",
        "description": "Classify a contact's seniority from their title — one word, easy to filter.",
        "prompt": (
            "ROLE: You classify B2B contact seniority.\n"
            "CONTEXT: Contact: {contact_person}. Title: {contact_title}. Company: {company}.\n"
            "TASK: Classify the seniority tier of this contact.\n"
            "FORMAT: Output exactly one of: C_SUITE, VP, DIRECTOR, MANAGER, IC, UNKNOWN. "
            "Nothing else.\n"
            f"FALLBACK: If there is no title, output UNKNOWN."
        ),
    },
    {
        "id": "company_one_liner",
        "name": "Company One-Liner",
        "category": "research",
        "column_type": "ai_formula",
        "output_format": "text",
        "description": "A crisp one-sentence description of what the company does.",
        "prompt": (
            "ROLE: You write crisp company descriptions.\n"
            "CONTEXT: Company: {company}. Website: {website}. Notes: {description}.\n"
            "TASK: In one sentence, state what this company does and who they serve.\n"
            "FORMAT: One sentence, max 25 words, no marketing fluff.\n"
            f"FALLBACK: If you don't know what they do, output {INSUFFICIENT}."
        ),
    },
    {
        "id": "tech_stack_research",
        "name": "Tech-Stack Detection (web research)",
        "category": "research",
        "column_type": "research",
        "output_format": "text",
        "description": "Research the company's website/job posts to detect their tech stack.",
        "prompt": (
            "ROLE: You are a technographic researcher.\n"
            "CONTEXT: Company: {company}. Website: {website}.\n"
            "TASK: Search the company's site and job postings to determine the key technologies "
            "they use (languages, frameworks, cloud, major SaaS tools). Only report tech you find "
            "concrete evidence for — never guess.\n"
            "FORMAT: Comma-separated list of technologies.\n"
            f"FALLBACK: If you find no concrete tech evidence, output {INSUFFICIENT}."
        ),
    },
    {
        "id": "account_brief",
        "name": "3-Bullet Account Brief",
        "category": "research",
        "column_type": "research",
        "output_format": "text",
        "description": "A short pre-call account brief: what they do, a recent signal, and an angle.",
        "prompt": (
            "ROLE: You are an SDR prepping for an account.\n"
            "CONTEXT: Company: {company}. Website: {website}. Notes: {description}.\n"
            "TASK: Research this company and produce a 3-bullet brief: (1) what they do, "
            "(2) one recent, concrete signal (hiring, funding, launch, news), (3) a relevant "
            "outreach angle. Cite the source for the signal.\n"
            "FORMAT: Exactly 3 bullets.\n"
            f"FALLBACK: If you can't find a concrete recent signal, still give bullets 1 and 3 and "
            f"write 'no recent signal found' for bullet 2."
        ),
    },
]

_BY_ID = {p["id"]: p for p in PRESETS}


def list_presets(category: str = None) -> List[Dict]:
    if category:
        return [p for p in PRESETS if p["category"] == category]
    return list(PRESETS)


def get_preset(preset_id: str) -> Dict:
    return _BY_ID.get(preset_id)


def categories() -> List[str]:
    return sorted({p["category"] for p in PRESETS})
