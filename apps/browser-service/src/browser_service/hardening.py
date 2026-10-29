"""Process hardening at start (review M1).

Chromium renders untrusted pages. Whatever runs as the same uid must not be
able to recover the service token, so before Playwright (and Chromium) start:

* the token is removed from ``os.environ``, so the Playwright driver and the
  browser it spawns never inherit it;
* the service process is made non-dumpable (``PR_SET_DUMPABLE=0``): its
  ``/proc/<pid>/environ`` and memory become unreadable and un-ptrace-able for
  other processes of the same uid (the initial environment block still holds
  the token, so this is what protects it).

In split mode (scripts/entrypoint.sh started as root) Chromium additionally
runs as its own uid behind a loopback Playwright server.
"""

from __future__ import annotations

import ctypes
import logging
import os
import sys

logger = logging.getLogger("browser_service.hardening")

SECRET_ENV = ("BROWSER_SERVICE_TOKEN",)
PR_SET_DUMPABLE = 4


def harden_process() -> None:
    for name in SECRET_ENV:
        os.environ.pop(name, None)
    if not sys.platform.startswith("linux"):
        return
    try:
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.prctl(PR_SET_DUMPABLE, 0, 0, 0, 0) != 0:
            logger.warning("prctl(PR_SET_DUMPABLE) failed: errno %d", ctypes.get_errno())
    except OSError:
        logger.warning("prctl unavailable; process stays dumpable")
