"""ProviderStat (Pillar 2) — per-provider, per-field performance ledger.

Feeds the cost-aware planner: learned hit-rate, latency, cost, and cooldown
state replace the hardcoded free-first/BYOK-last ordering in DEFAULT_WATERFALLS.
"""

from sqlalchemy import Column, String, Integer, Float, DateTime, UniqueConstraint
from sqlalchemy.sql import func

from apps.api.database import Base


class ProviderStat(Base):
    __tablename__ = "provider_stats"
    __table_args__ = (UniqueConstraint("provider", "field", name="uq_provider_field"),)

    id = Column(Integer, primary_key=True, autoincrement=True)
    provider = Column(String(100), nullable=False, index=True)
    field = Column(String(100), nullable=False, index=True)

    attempts = Column(Integer, default=0)
    hits = Column(Integer, default=0)
    total_confidence = Column(Float, default=0.0)   # ÷ hits = avg confidence
    total_latency_ms = Column(Float, default=0.0)   # ÷ attempts = avg latency
    total_cost_usd = Column(Float, default=0.0)

    cooldown_until = Column(DateTime, nullable=True)  # set on rate-limit; skipped while in future
    updated_at = Column(DateTime, server_default=func.now(), onupdate=func.now())

    @property
    def hit_rate(self) -> float:
        return (self.hits / self.attempts) if self.attempts else 0.0

    @property
    def avg_confidence(self) -> float:
        return (self.total_confidence / self.hits) if self.hits else 0.0

    @property
    def avg_latency_ms(self) -> float:
        return (self.total_latency_ms / self.attempts) if self.attempts else 0.0

    def to_api(self) -> dict:
        return {
            "provider": self.provider, "field": self.field,
            "attempts": self.attempts, "hits": self.hits,
            "hit_rate": round(self.hit_rate, 3),
            "avg_confidence": round(self.avg_confidence, 3),
            "avg_latency_ms": round(self.avg_latency_ms, 1),
            "total_cost_usd": round(self.total_cost_usd, 4),
            "cooldown_until": self.cooldown_until.isoformat() if self.cooldown_until else None,
        }
