"""
Lead Deduplication Engine — Lightweight probabilistic record linking.

Based on splink's approach (Fellegi-Sunter model) but implemented in pure Python
with no heavy dependencies. Uses blocking + Jaro-Winkler fuzzy matching.

Algorithm:
  1. Blocking: Only compare leads sharing first 3 chars of normalized company name
  2. Field comparison: Weighted fuzzy match on (company, domain, city, phone, email)
  3. Scoring: Probability-based match score from 0 to 1
  4. Clustering: Union-Find to group duplicates into clusters

Usage:
    from apps.api.services.dedup import LeadDeduplicator
    dedup = LeadDeduplicator()
    clusters = dedup.find_duplicates(leads_list)
    merged = dedup.auto_merge(leads_list, threshold=0.85)
"""

import re
import logging
from typing import Dict, List, Optional, Tuple, Set
from dataclasses import dataclass
from urllib.parse import urlparse

logger = logging.getLogger("leadgen.dedup")


# ── Jaro-Winkler Distance ───────────────────────────────────────────────

def _jaro_similarity(s1: str, s2: str) -> float:
    """Jaro similarity between two strings (0.0 to 1.0)."""
    if s1 == s2:
        return 1.0
    if not s1 or not s2:
        return 0.0

    len1, len2 = len(s1), len(s2)
    match_distance = max(len1, len2) // 2 - 1
    if match_distance < 0:
        match_distance = 0

    s1_matches = [False] * len1
    s2_matches = [False] * len2

    matches = 0
    transpositions = 0

    for i in range(len1):
        start = max(0, i - match_distance)
        end = min(i + match_distance + 1, len2)
        for j in range(start, end):
            if s2_matches[j] or s1[i] != s2[j]:
                continue
            s1_matches[i] = True
            s2_matches[j] = True
            matches += 1
            break

    if matches == 0:
        return 0.0

    k = 0
    for i in range(len1):
        if not s1_matches[i]:
            continue
        while not s2_matches[k]:
            k += 1
        if s1[i] != s2[k]:
            transpositions += 1
        k += 1

    return (
        matches / len1
        + matches / len2
        + (matches - transpositions / 2) / matches
    ) / 3


def jaro_winkler(s1: str, s2: str, prefix_weight: float = 0.1) -> float:
    """Jaro-Winkler similarity — better than Levenshtein for short strings like company names."""
    jaro = _jaro_similarity(s1, s2)

    # Find common prefix (up to 4 chars)
    prefix_len = 0
    for i in range(min(len(s1), len(s2), 4)):
        if s1[i] == s2[i]:
            prefix_len += 1
        else:
            break

    return jaro + prefix_len * prefix_weight * (1 - jaro)


# ── Normalization ────────────────────────────────────────────────────────

# Common suffixes to strip for comparison
_COMPANY_SUFFIXES = re.compile(
    r'\b(pvt|private|ltd|limited|llp|llc|inc|incorporated|corp|corporation|'
    r'co|company|group|enterprises|solutions|technologies|tech|services|'
    r'consulting|consultants|india|global|international|intl)\b',
    re.IGNORECASE,
)

_NOISE_CHARS = re.compile(r'[.\-,&()\'"!@#$%^*+=]')


def normalize_company(name: str) -> str:
    """Normalize company name for comparison."""
    if not name:
        return ""
    name = name.lower().strip()
    name = _NOISE_CHARS.sub(" ", name)
    name = _COMPANY_SUFFIXES.sub("", name)
    name = re.sub(r'\s+', ' ', name).strip()
    return name


def normalize_domain(url: str) -> str:
    """Extract and normalize domain from URL."""
    if not url:
        return ""
    url = url.lower().strip()
    if not url.startswith(("http://", "https://")):
        url = "http://" + url
    try:
        parsed = urlparse(url)
        domain = parsed.hostname or ""
        # Strip www prefix
        if domain.startswith("www."):
            domain = domain[4:]
        return domain
    except Exception:
        return url.lower().replace("www.", "").strip("/")


def normalize_phone(phone: str) -> str:
    """Normalize phone to digits only."""
    if not phone:
        return ""
    return re.sub(r'[^\d]', '', phone)[-10:]  # Last 10 digits


def normalize_email(email: str) -> str:
    """Normalize email for comparison."""
    if not email:
        return ""
    return email.lower().strip()


# ── Field Comparison Weights ─────────────────────────────────────────────

@dataclass
class MatchResult:
    """Result of comparing two leads."""
    lead_a_id: int
    lead_b_id: int
    score: float
    field_scores: Dict[str, float]
    is_duplicate: bool


# Weights sum to 1.0
FIELD_WEIGHTS = {
    "company": 0.35,   # Company name is primary signal
    "domain": 0.30,    # Domain is strong identifier
    "phone": 0.15,     # Phone is unique if present
    "email": 0.10,     # Email domain overlap
    "city": 0.10,      # Same city boosts confidence
}


def compare_leads(lead_a: dict, lead_b: dict) -> MatchResult:
    """Compare two leads and return a match score (0.0 to 1.0)."""
    field_scores = {}

    # Company name — Jaro-Winkler
    name_a = normalize_company(lead_a.get("company", ""))
    name_b = normalize_company(lead_b.get("company", ""))
    field_scores["company"] = jaro_winkler(name_a, name_b) if name_a and name_b else 0.0

    # Domain — exact match after normalization
    domain_a = normalize_domain(lead_a.get("website", ""))
    domain_b = normalize_domain(lead_b.get("website", ""))
    if domain_a and domain_b:
        field_scores["domain"] = 1.0 if domain_a == domain_b else jaro_winkler(domain_a, domain_b)
    else:
        field_scores["domain"] = 0.0

    # Phone — exact match on last 10 digits
    phone_a = normalize_phone(lead_a.get("phone", ""))
    phone_b = normalize_phone(lead_b.get("phone", ""))
    if phone_a and phone_b and len(phone_a) >= 7:
        field_scores["phone"] = 1.0 if phone_a == phone_b else 0.0
    else:
        field_scores["phone"] = 0.0

    # Email — domain portion match
    email_a = normalize_email(lead_a.get("email", ""))
    email_b = normalize_email(lead_b.get("email", ""))
    if "@" in email_a and "@" in email_b:
        domain_ea = email_a.split("@")[1]
        domain_eb = email_b.split("@")[1]
        field_scores["email"] = 1.0 if domain_ea == domain_eb else 0.0
    else:
        field_scores["email"] = 0.0

    # City — normalized exact match
    city_a = (lead_a.get("city", "") or "").lower().strip()
    city_b = (lead_b.get("city", "") or "").lower().strip()
    if city_a and city_b:
        field_scores["city"] = 1.0 if city_a == city_b else jaro_winkler(city_a, city_b)
    else:
        field_scores["city"] = 0.0

    # Weighted score
    total = sum(
        field_scores.get(f, 0) * w
        for f, w in FIELD_WEIGHTS.items()
    )

    # Boost: if domain matches exactly, boost the score
    if field_scores["domain"] == 1.0:
        total = max(total, 0.90)

    # Boost: if phone matches exactly, very likely duplicate
    if field_scores["phone"] == 1.0 and field_scores["phone"] > 0:
        total = max(total, 0.92)

    return MatchResult(
        lead_a_id=lead_a.get("id", 0),
        lead_b_id=lead_b.get("id", 0),
        score=round(total, 4),
        field_scores={k: round(v, 3) for k, v in field_scores.items()},
        is_duplicate=total >= 0.85,
    )


# ── Union-Find for Clustering ───────────────────────────────────────────

class UnionFind:
    """Disjoint-set data structure for clustering duplicates."""

    def __init__(self):
        self.parent: Dict[int, int] = {}
        self.rank: Dict[int, int] = {}

    def find(self, x: int) -> int:
        if x not in self.parent:
            self.parent[x] = x
            self.rank[x] = 0
        if self.parent[x] != x:
            self.parent[x] = self.find(self.parent[x])  # Path compression
        return self.parent[x]

    def union(self, x: int, y: int):
        rx, ry = self.find(x), self.find(y)
        if rx == ry:
            return
        # Union by rank
        if self.rank[rx] < self.rank[ry]:
            self.parent[rx] = ry
        elif self.rank[rx] > self.rank[ry]:
            self.parent[ry] = rx
        else:
            self.parent[ry] = rx
            self.rank[rx] += 1


# ── Main Deduplicator ───────────────────────────────────────────────────

class LeadDeduplicator:
    """
    Lightweight lead deduplication engine.

    Usage:
        dedup = LeadDeduplicator(threshold=0.85)
        result = dedup.find_duplicates(leads)
        # result.clusters: dict of cluster_id -> [lead_ids]
        # result.pairs: list of MatchResult with score >= threshold
        # result.stats: summary statistics
    """

    def __init__(self, threshold: float = 0.85):
        self.threshold = threshold

    def _build_blocking_keys(self, leads: List[dict]) -> Dict[str, List[int]]:
        """
        Build blocking index: only compare leads sharing a blocking key.
        This reduces O(n²) to approximately O(n * block_size).
        """
        blocks: Dict[str, List[int]] = {}
        for i, lead in enumerate(leads):
            keys = set()

            # Block 1: First 3 chars of normalized company name
            name = normalize_company(lead.get("company", ""))
            if len(name) >= 3:
                keys.add(f"name:{name[:3]}")

            # Block 2: Domain
            domain = normalize_domain(lead.get("website", ""))
            if domain:
                keys.add(f"domain:{domain}")

            # Block 3: Phone last 7 digits
            phone = normalize_phone(lead.get("phone", ""))
            if len(phone) >= 7:
                keys.add(f"phone:{phone[-7:]}")

            # Block 4: City + first word of company
            city = (lead.get("city", "") or "").lower().strip()
            first_word = name.split()[0] if name else ""
            if city and first_word:
                keys.add(f"city:{city}:{first_word}")

            for key in keys:
                if key not in blocks:
                    blocks[key] = []
                blocks[key].append(i)

        return blocks

    def find_duplicates(self, leads: List[dict]) -> dict:
        """
        Find duplicate leads using blocking + fuzzy matching.

        Returns:
            {
                "clusters": {cluster_id: [lead_ids]},
                "pairs": [MatchResult],
                "stats": {"total": n, "duplicates": m, "unique": k},
                "merge_suggestions": [{master_id, duplicate_ids, confidence}]
            }
        """
        if len(leads) < 2:
            return {
                "clusters": {},
                "pairs": [],
                "stats": {"total": len(leads), "duplicates": 0, "unique": len(leads)},
                "merge_suggestions": [],
            }

        logger.info(f"Dedup: analyzing {len(leads)} leads...")

        # Build blocking index
        blocks = self._build_blocking_keys(leads)

        # Compare pairs within blocks
        compared: Set[Tuple[int, int]] = set()
        matches: List[MatchResult] = []
        uf = UnionFind()

        for block_key, indices in blocks.items():
            if len(indices) < 2 or len(indices) > 100:
                # Skip blocks that are too large (generic names)
                continue

            for i in range(len(indices)):
                for j in range(i + 1, len(indices)):
                    a_idx, b_idx = indices[i], indices[j]
                    pair_key = (min(a_idx, b_idx), max(a_idx, b_idx))
                    if pair_key in compared:
                        continue
                    compared.add(pair_key)

                    result = compare_leads(leads[a_idx], leads[b_idx])
                    result.lead_a_id = leads[a_idx].get("id", a_idx)
                    result.lead_b_id = leads[b_idx].get("id", b_idx)

                    if result.score >= self.threshold:
                        matches.append(result)
                        uf.union(a_idx, b_idx)

        # Build clusters
        clusters: Dict[int, List[int]] = {}
        for i in range(len(leads)):
            root = uf.find(i)
            if root not in clusters:
                clusters[root] = []
            clusters[root].append(leads[i].get("id", i))

        # Only keep clusters with 2+ members (actual duplicates)
        dup_clusters = {k: v for k, v in clusters.items() if len(v) > 1}

        # Build merge suggestions
        merge_suggestions = []
        for cluster_id, lead_ids in dup_clusters.items():
            # Pick the lead with the most data as master
            cluster_leads = [l for l in leads if l.get("id") in lead_ids]
            if not cluster_leads:
                continue

            # Score each lead by data completeness
            def completeness(l: dict) -> int:
                score = 0
                for field in ["email", "phone", "website", "description", "linkedin_url",
                              "contact_person", "company_size", "specialization"]:
                    val = l.get(field, "")
                    if val and val not in ("", "N/A", "nan"):
                        score += 1
                return score

            cluster_leads.sort(key=completeness, reverse=True)
            master = cluster_leads[0]
            duplicates = cluster_leads[1:]

            merge_suggestions.append({
                "master_id": master.get("id"),
                "master_company": master.get("company", ""),
                "duplicate_ids": [d.get("id") for d in duplicates],
                "duplicate_companies": [d.get("company", "") for d in duplicates],
                "confidence": max(
                    (m.score for m in matches
                     if {m.lead_a_id, m.lead_b_id} & set(lead_ids)),
                    default=0.0,
                ),
            })

        dup_count = sum(len(v) - 1 for v in dup_clusters.values())

        stats = {
            "total_leads": len(leads),
            "pairs_compared": len(compared),
            "duplicates_found": dup_count,
            "clusters": len(dup_clusters),
            "unique_leads": len(leads) - dup_count,
        }

        logger.info(
            f"Dedup complete: {stats['duplicates_found']} duplicates in "
            f"{stats['clusters']} clusters (compared {stats['pairs_compared']} pairs)"
        )

        return {
            "clusters": {str(k): v for k, v in dup_clusters.items()},
            "pairs": [
                {
                    "lead_a_id": m.lead_a_id,
                    "lead_b_id": m.lead_b_id,
                    "score": m.score,
                    "field_scores": m.field_scores,
                }
                for m in sorted(matches, key=lambda x: x.score, reverse=True)
            ],
            "stats": stats,
            "merge_suggestions": merge_suggestions,
        }

    def merge_leads(self, master: dict, duplicates: List[dict]) -> dict:
        """
        Merge duplicate leads into a master record.
        Fills empty fields from duplicates (waterfall merge).
        """
        merged = dict(master)

        for dup in duplicates:
            for key, value in dup.items():
                if key in ("id", "created_at"):
                    continue
                current = merged.get(key, "")
                if (not current or current in ("", "N/A", "nan", "0")) and value and value not in ("", "N/A", "nan", "0"):
                    merged[key] = value

        # Aggregate sources
        sources = {master.get("source", "")}
        for d in duplicates:
            if d.get("source"):
                sources.add(d["source"])
        sources.discard("")
        merged["source"] = "|".join(sorted(sources))

        # Use highest score
        scores = [master.get("score", 0)] + [d.get("score", 0) for d in duplicates]
        merged["score"] = max(scores)

        return merged
