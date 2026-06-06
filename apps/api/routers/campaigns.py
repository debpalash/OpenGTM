"""
Campaigns Router — AI-powered outreach email generation.

Uses the LLM client to generate personalized cold emails based on lead data.
"""

from fastapi import APIRouter, Depends
from pydantic import BaseModel
from typing import List, Optional

from apps.api.core.tenancy import WorkspaceCtx, current_workspace

router = APIRouter(prefix="/api/campaigns", tags=["campaigns"])


class GenerateRequest(BaseModel):
    lead_ids: List[int]
    value_proposition: str
    tone: str = "professional"
    extra_context: str = ""


class GeneratedEmail(BaseModel):
    company: str
    subject: str
    body: str


class GenerateResponse(BaseModel):
    emails: List[GeneratedEmail]


@router.post("/generate", response_model=GenerateResponse)
async def generate_outreach(req: GenerateRequest, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Generate personalized outreach emails for the given lead IDs."""
    from apps.api.services.leadgen.llm import llm

    db = ctx.lead_db()
    emails = []

    for lead_id in req.lead_ids[:10]:  # Cap at 10
        try:
            lead = db.get_lead(lead_id)
            if not lead or not lead.email:
                continue

            prompt = _build_email_prompt(lead, req)
            result = await llm.extract_json(prompt, system=_EMAIL_SYSTEM_PROMPT)

            if result and result.get("subject") and result.get("body"):
                emails.append(GeneratedEmail(
                    company=lead.company,
                    subject=result["subject"],
                    body=result["body"],
                ))
        except Exception:
            continue

    db.close()
    return GenerateResponse(emails=emails)


_EMAIL_SYSTEM_PROMPT = """You are an expert B2B sales copywriter. Generate personalized cold outreach emails.

Rules:
- Keep subject lines under 50 chars, curiosity-driven
- Keep body under 150 words
- Reference specific company details (name, size, industry)
- Include a clear CTA (call/meeting)
- No generic templates — each email must feel personally crafted
- Be respectful and professional
- Return JSON: {"subject": "...", "body": "..."}"""


def _build_email_prompt(lead, req) -> str:
    """Build the LLM prompt for email generation."""
    company_info = f"Company: {lead.company}"
    if lead.city:
        company_info += f"\nLocation: {lead.city}"
    if lead.company_size:
        company_info += f"\nSize: {lead.company_size} employees"
    if lead.specialization:
        company_info += f"\nIndustry: {lead.specialization}"
    if lead.description:
        company_info += f"\nAbout: {lead.description[:200]}"
    if lead.contact_person:
        company_info += f"\nContact: {lead.contact_person}"
    if lead.glassdoor_rating:
        company_info += f"\nGlassdoor: {lead.glassdoor_rating}/5"

    tone_guide = {
        "professional": "Professional and polished",
        "friendly": "Warm, conversational, and approachable",
        "direct": "Direct, bold, and to the point",
        "consultative": "Consultative — position as a helpful advisor",
    }

    return f"""Generate a personalized cold outreach email for:

{company_info}

My value proposition: {req.value_proposition}

Tone: {tone_guide.get(req.tone, req.tone)}

{f"Additional context: {req.extra_context}" if req.extra_context else ""}

Return a JSON object with "subject" and "body" fields."""
