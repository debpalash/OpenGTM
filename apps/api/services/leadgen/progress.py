"""Workspace-isolated progress events for API and worker processes.

The durable worker normally runs in a separate container from FastAPI, so an
in-memory observer alone cannot power the browser's SSE stream. Redis Pub/Sub
is the live transport and a bounded Redis list supplies reconnect backfill.
The in-process queues remain as a fail-soft development/test fallback.
"""

from __future__ import annotations

import json
import logging
import os
import threading
import time
from collections import OrderedDict, deque
from typing import Optional

logger = logging.getLogger("leadgen.progress")


class ProgressBus:
    """Thread-safe, tenant-aware progress bus with Redis cross-process fanout."""

    _instance = None
    _lock = threading.Lock()

    def __new__(cls):
        with cls._lock:
            if cls._instance is None:
                cls._instance = super().__new__(cls)
                cls._instance._events = deque(maxlen=500)
                cls._instance._subscribers = []
                cls._instance._sub_lock = threading.Lock()
                cls._instance._job_workspaces = OrderedDict()
                cls._instance._redis_client = None
                cls._instance._redis_retry_after = 0.0
            return cls._instance

    @staticmethod
    def _channel(workspace_id: str) -> str:
        return f"leadgen:progress:{workspace_id}"

    @classmethod
    def _recent_key(cls, workspace_id: str) -> str:
        return f"{cls._channel(workspace_id)}:recent"

    def bind_job(self, job_id: str, workspace_id: str) -> None:
        """Bind deep ``job_id``-only events to their authenticated workspace."""
        if not job_id or not workspace_id:
            raise ValueError("progress.bind_job requires job_id and workspace_id")
        with self._sub_lock:
            self._job_workspaces[str(job_id)] = str(workspace_id)
            self._job_workspaces.move_to_end(str(job_id))
            while len(self._job_workspaces) > 5_000:
                self._job_workspaces.popitem(last=False)

    def _workspace_for(self, data: dict) -> Optional[str]:
        job_id = data.get("job_id")
        with self._sub_lock:
            bound = self._job_workspaces.get(str(job_id)) if job_id else None
        # A server-side binding wins over an event-supplied value. This prevents
        # a deep component from accidentally routing a known job across tenants.
        return bound or data.get("workspace_id") or None

    def _get_redis(self):
        redis_url = os.getenv("REDIS_URL")
        # Unit tests and bare local CLI runs stay hermetic unless Redis is
        # explicitly configured. Docker Compose sets REDIS_URL for API/worker.
        if not redis_url:
            return None
        if self._redis_client is False and time.monotonic() < self._redis_retry_after:
            return None
        if self._redis_client not in (None, False):
            return self._redis_client
        try:
            import redis

            client = redis.Redis.from_url(
                redis_url,
                decode_responses=True,
                socket_connect_timeout=0.2,
                socket_timeout=0.5,
                health_check_interval=30,
            )
            client.ping()
            self._redis_client = client
            return client
        except Exception as exc:
            # Redis is optional for single-process development, but production
            # Compose supplies it. Avoid log/event storms while it is starting.
            if self._redis_client is not False:
                logger.warning("Progress Redis unavailable; using local fallback: %s", exc)
            self._redis_client = False
            self._redis_retry_after = time.monotonic() + 5.0
            return None

    def _forget_redis(self) -> None:
        self._redis_client = False
        self._redis_retry_after = time.monotonic() + 1.0

    def _publish_redis(self, workspace_id: str, event: dict) -> None:
        client = self._get_redis()
        if client is None:
            return
        encoded = json.dumps(event, default=str, separators=(",", ":"))
        try:
            pipe = client.pipeline(transaction=False)
            pipe.publish(self._channel(workspace_id), encoded)
            pipe.lpush(self._recent_key(workspace_id), encoded)
            pipe.ltrim(self._recent_key(workspace_id), 0, 499)
            pipe.execute()
        except Exception as exc:
            logger.warning("Progress Redis publish failed; using local fallback: %s", exc)
            self._forget_redis()

    def emit(self, event_type: str, data: dict):
        """Publish one event locally and, when scoped, to its Redis tenant."""
        workspace_id = self._workspace_for(data)
        event = {
            "type": event_type,
            "ts": time.time(),
            **data,
        }
        if workspace_id:
            event["workspace_id"] = workspace_id
        self._events.append(event)

        with self._sub_lock:
            dead = []
            for q, subscriber_workspace in self._subscribers:
                if subscriber_workspace and subscriber_workspace != workspace_id:
                    continue
                try:
                    q.append(event)
                except Exception:
                    dead.append((q, subscriber_workspace))
            for subscriber in dead:
                if subscriber in self._subscribers:
                    self._subscribers.remove(subscriber)

        # Never publish an unscoped event onto a shared channel. Such events are
        # still visible to unscoped local subscribers used by CLI/eval tooling.
        if workspace_id:
            self._publish_redis(workspace_id, event)
        return event

    def subscribe(self, workspace_id: Optional[str] = None):
        """Create a local fallback subscriber, optionally tenant-filtered."""
        q = deque(maxlen=100)
        with self._sub_lock:
            self._subscribers.append((q, workspace_id))
        return q

    def unsubscribe(self, q):
        """Remove a local subscriber queue."""
        with self._sub_lock:
            self._subscribers = [entry for entry in self._subscribers if entry[0] is not q]

    def open_redis_subscription(self, workspace_id: str):
        """Subscribe to one exact tenant channel, or return None for fallback."""
        if not workspace_id:
            raise ValueError("workspace_id is required for progress subscription")
        client = self._get_redis()
        if client is None:
            return None
        try:
            pubsub = client.pubsub(ignore_subscribe_messages=True)
            pubsub.subscribe(self._channel(workspace_id))
            return pubsub
        except Exception as exc:
            logger.warning("Progress Redis subscribe failed; using local fallback: %s", exc)
            self._forget_redis()
            return None

    def recent(self, limit: int = 50, workspace_id: Optional[str] = None) -> list:
        """Return oldest-to-newest bounded history, optionally tenant-scoped."""
        limit = max(0, min(int(limit), 500))
        if limit == 0:
            return []
        if workspace_id:
            client = self._get_redis()
            if client is not None:
                try:
                    raw = client.lrange(self._recent_key(workspace_id), 0, limit - 1)
                    if raw:
                        rows = [json.loads(item) for item in raw]
                        rows.reverse()
                        return rows
                except Exception as exc:
                    logger.warning("Progress Redis history failed; using local fallback: %s", exc)
                    self._forget_redis()
            return [
                event
                for event in self._events
                if event.get("workspace_id") == workspace_id
            ][-limit:]
        return list(self._events)[-limit:]


# Singleton
progress = ProgressBus()
