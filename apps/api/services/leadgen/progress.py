"""
Progress Events — Thread-safe event broadcast for real-time UI updates.

Components push events here, and SSE endpoint streams them to the browser.
"""

import json
import threading
import time
from collections import deque
from typing import Optional


class ProgressBus:
    """Thread-safe event bus for pipeline progress events."""

    _instance = None
    _lock = threading.Lock()

    def __new__(cls):
        with cls._lock:
            if cls._instance is None:
                cls._instance = super().__new__(cls)
                cls._instance._events = deque(maxlen=500)
                cls._instance._subscribers = []
                cls._instance._sub_lock = threading.Lock()
            return cls._instance

    def emit(self, event_type: str, data: dict):
        """Push an event to all subscribers."""
        event = {
            "type": event_type,
            "ts": time.time(),
            **data,
        }
        self._events.append(event)
        with self._sub_lock:
            dead = []
            for q in self._subscribers:
                try:
                    q.append(event)
                except Exception:
                    dead.append(q)
            for q in dead:
                self._subscribers.remove(q)

    def subscribe(self):
        """Create a new subscriber queue and return it."""
        q = deque(maxlen=100)
        with self._sub_lock:
            self._subscribers.append(q)
        return q

    def unsubscribe(self, q):
        """Remove a subscriber queue."""
        with self._sub_lock:
            if q in self._subscribers:
                self._subscribers.remove(q)

    def recent(self, limit: int = 50) -> list:
        """Get recent events."""
        return list(self._events)[-limit:]


# Singleton
progress = ProgressBus()
