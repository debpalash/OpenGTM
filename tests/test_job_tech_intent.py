"""
Job-posting tech-adoption intent parser — offline, sample-text fixtures only.
No network: every test feeds canned job-posting text to the pure parser, and
the provider-level tests stub the fetch layer so nothing leaves the process.
"""
import json

import pytest

from apps.api.services.leadgen.enrichment.providers.job_tech_intent import (
    TECH_DICTIONARY,
    analyze_job_text,
    compute_hiring_velocity,
    detect_tech_adoption_signal,
    detect_technologies,
)
from apps.api.services.leadgen.enrichment.providers.ats_hiring import _titles_to_signals
from apps.api.services.leadgen.enrichment.providers.jobspy_signals import _analyze_jobs


# ── Sample fixtures ──────────────────────────────────────────────────────
JD_DATA_ENGINEER = (
    "Senior Data Engineer. We use Snowflake and dbt for our warehouse, "
    "orchestrated with Airflow. Strong Python required. Experience with "
    "AWS and Kubernetes a plus."
)

JD_MIGRATION = (
    "Marketing Operations Manager. We are migrating to Salesforce from our "
    "legacy CRM and rolling out Marketo for campaign automation."
)

JD_COMPETITOR = (
    "Growth Engineer to own our outbound stack. You'll work daily in Clay "
    "and Apollo.io, and we're evaluating ZoomInfo for data coverage."
)


# ── 1. Tech detection from description text ──────────────────────────────
def test_detect_technologies_from_description():
    names = {t["name"] for t in detect_technologies(JD_DATA_ENGINEER)}
    assert {"Snowflake", "dbt", "Airflow", "Python", "AWS", "Kubernetes"} <= names


def test_detection_dedupes_and_sorts():
    techs = detect_technologies(JD_DATA_ENGINEER)
    names = [t["name"] for t in techs]
    assert names == sorted(names)
    assert len(names) == len(set(names))  # no dupes


def test_word_boundary_no_false_positive():
    # "go" must not match inside "google"; "ago"; etc.
    names = {t["name"] for t in detect_technologies("We use google cloud, a while ago.")}
    assert "Go" not in names
    assert "GCP" in names  # google cloud still detected


@pytest.mark.parametrize("text", ["planet cabinet management", "a clayton magnet"])
def test_no_substring_false_positives(text):
    # ".net" must not match planet/cabinet; "clay" must not match clayton;
    # "go" must not match magnet.
    assert detect_technologies(text) == []


# ── 2. Competitor-tool detection ─────────────────────────────────────────
def test_competitor_tool_detection():
    signals = detect_tech_adoption_signal(JD_COMPETITOR)
    by_tech = {s["tech"]: s for s in signals}
    assert "Clay" in by_tech
    assert "Apollo.io" in by_tech
    assert "ZoomInfo" in by_tech
    assert by_tech["Clay"]["category"] == "Competitor Tools"


# ── 3. tech_adoption_signal intent classification ────────────────────────
def test_adoption_intent_emitted_for_migration():
    signals = detect_tech_adoption_signal(JD_MIGRATION)
    by_tech = {s["tech"]: s["intent"] for s in signals}
    assert by_tech["Salesforce"] == "adopting"   # "migrating to Salesforce"
    assert by_tech["Marketo"] == "adopting"       # "rolling out Marketo"


def test_evaluating_competitor_marked_adopting():
    signals = detect_tech_adoption_signal(JD_COMPETITOR)
    by_tech = {s["tech"]: s["intent"] for s in signals}
    assert by_tech["ZoomInfo"] == "adopting"      # "evaluating ZoomInfo"


def test_plain_mention_is_using():
    signals = detect_tech_adoption_signal("Backend role. Strong Python skills required.")
    assert signals == [{"tech": "Python", "category": "Languages", "intent": "using"}]


def test_repeated_mention_is_expanding():
    text = "We love Tableau. Our analytics team builds everything in Tableau dashboards."
    signals = detect_tech_adoption_signal(text)
    by_tech = {s["tech"]: s["intent"] for s in signals}
    assert by_tech["Tableau"] == "expanding"


# ── 4. Empty / no-text degrades gracefully (no crash, no signal) ─────────
@pytest.mark.parametrize("bad", ["", "   ", None])
def test_empty_text_no_signal(bad):
    assert detect_technologies(bad or "") == []
    assert detect_tech_adoption_signal(bad or "") == []
    out = analyze_job_text(bad or "")
    assert out["technologies"] == []
    assert out["tech_adoption_signal"] == []
    assert "hiring_velocity" not in out  # no open_roles supplied


def test_text_with_no_known_tech():
    out = analyze_job_text("We are hiring a warehouse picker. Lifting required.")
    assert out["technologies"] == []
    assert out["tech_adoption_signal"] == []


# ── 5. hiring_velocity derivation ────────────────────────────────────────
@pytest.mark.parametrize("n,level", [
    (0, "none"), (1, "low"), (3, "moderate"), (7, "high"), (12, "hypergrowth"),
])
def test_hiring_velocity_levels(n, level):
    v = compute_hiring_velocity(n)
    assert v["level"] == level
    assert v["open_roles"] == n


def test_hiring_velocity_recency_passthrough():
    v = compute_hiring_velocity(6, recent_roles=4, recency_days=30)
    assert v["recent_roles"] == 4 and v["recency_days"] == 30 and v["level"] == "high"


def test_analyze_job_text_includes_velocity_when_count_given():
    out = analyze_job_text(JD_DATA_ENGINEER, open_roles=8)
    assert out["hiring_velocity"]["level"] == "high"
    assert "Snowflake" in out["technologies"]


# ── 6. Representative dictionary coverage ────────────────────────────────
def test_every_category_present():
    expected = {
        "CRM", "MarTech", "Data & Warehouse", "Databases",
        "Cloud & Infra", "Languages", "Web Frameworks", "Competitor Tools",
    }
    assert expected <= set(TECH_DICTIONARY.keys())


def test_canonical_names_unique_across_categories():
    seen = set()
    for entries in TECH_DICTIONARY.values():
        for name in entries:
            assert name not in seen, f"duplicate canonical name {name}"
            seen.add(name)


@pytest.mark.parametrize("text,expected", [
    ("Salesforce admin wanted", "Salesforce"),
    ("HubSpot marketer", "HubSpot"),
    ("Snowflake data warehouse expert", "Snowflake"),
    ("MongoDB and Redis experience", "MongoDB"),
    ("Deploy on Azure", "Azure"),
    ("Strong TypeScript skills", "TypeScript"),
    ("React frontend developer", "React"),
    ("Daily user of Apollo.io", "Apollo.io"),
])
def test_representative_detections(text, expected):
    names = {t["name"] for t in detect_technologies(text)}
    assert expected in names


# ── 7. Provider integration (offline) ────────────────────────────────────
def test_jobspy_analyze_jobs_emits_intent():
    jobs = [
        {"title": "Data Engineer", "description": JD_DATA_ENGINEER},
        {"title": "Growth Engineer", "description": JD_COMPETITOR},
    ]
    sig = _analyze_jobs(jobs)
    assert "Snowflake" in sig["technologies"]
    assert "Clay" in sig["technologies"]
    assert sig["hiring_velocity"]["open_roles"] == 2
    assert any(s["category"] == "Competitor Tools" for s in sig["tech_adoption_signal"])
    # competitor present => +5 displacement boost on top of base growth boost
    assert sig["score_boost"] >= 15
    # whole thing is JSON-serializable for the hiring_signals cell
    json.dumps(sig)


def test_jobspy_analyze_jobs_empty_no_crash():
    sig = _analyze_jobs([])
    assert sig["technologies"] == []
    assert sig["tech_adoption_signal"] == []
    assert sig["total_jobs"] == 0


def test_ats_titles_to_signals_emits_intent():
    titles = [
        "Senior Salesforce Administrator",
        "Snowflake Data Engineer",
        "React Frontend Engineer",
    ]
    s = _titles_to_signals(titles)
    assert "Salesforce" in s["technologies"]
    assert "Snowflake" in s["technologies"]
    assert s["hiring_velocity"]["open_roles"] == 3
    assert s["hiring_velocity"]["level"] == "moderate"
    json.dumps(s)


def test_ats_titles_to_signals_empty_velocity_none():
    s = _titles_to_signals([])
    assert s["technologies"] == []
    assert s["hiring_velocity"]["level"] == "none"
