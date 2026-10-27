"""Review M1: Chromium runs with its sandbox, never sees the service token, and
can run as its own uid behind a loopback Playwright server (split mode)."""

import json
import subprocess
import sys
import types

import pytest
from conftest import TOKEN

from browser_service.browser import CHROMIUM_ARGS, BrowserPool
from browser_service.config import ConfigError, Settings


def test_sandbox_is_on_by_default_and_can_be_turned_off():
    env = {"BROWSER_SERVICE_TOKEN": TOKEN}
    assert Settings.from_env(env).chromium_sandbox is True
    assert Settings.from_env({**env, "BROWSER_CHROMIUM_SANDBOX": "false"}).chromium_sandbox is False


@pytest.mark.parametrize(
    "ws,ok",
    [
        ("ws://127.0.0.1:8095/abc123", True),
        ("ws://localhost:8095/abc123", True),
        ("ws://browser-chromium:8095/abc", False),
        ("ws://10.0.0.1:8095/abc", False),
        ("http://127.0.0.1:8095/abc", False),
        ("ws://127.0.0.1/abc", False),
    ],
)
def test_playwright_server_url_must_be_loopback(ws, ok):
    env = {"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_PLAYWRIGHT_WS": ws}
    if ok:
        assert Settings.from_env(env).playwright_ws == ws
    else:
        with pytest.raises(ConfigError):
            Settings.from_env(env)


def test_settings_repr_hides_the_server_path():
    s = Settings.from_env({"BROWSER_SERVICE_TOKEN": TOKEN, "BROWSER_PLAYWRIGHT_WS": "ws://127.0.0.1:8095/sekretpath"})
    assert "sekretpath" not in repr(s)


def test_harden_process_drops_the_token_and_turns_off_dumping():
    code = (
        "import ctypes, os\n"
        "os.environ['BROWSER_SERVICE_TOKEN'] = 'x' * 40\n"
        "from browser_service.hardening import harden_process\n"
        "harden_process()\n"
        "libc = ctypes.CDLL(None)\n"
        "print('BROWSER_SERVICE_TOKEN' in os.environ, libc.prctl(3, 0, 0, 0, 0))\n"  # PR_GET_DUMPABLE
    )
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, timeout=30, check=True)
    assert out.stdout.split() == ["False", "0"]


def test_browser_env_has_no_secrets(monkeypatch):
    from browser_service.browser import browser_env

    monkeypatch.setenv("BROWSER_SERVICE_TOKEN", "x" * 40)
    monkeypatch.setenv("SOME_API_KEY", "k")
    env = browser_env()
    assert "BROWSER_SERVICE_TOKEN" not in env and "SOME_API_KEY" not in env
    assert set(env) <= {"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ", "FONTCONFIG_PATH", "XDG_CACHE_HOME"}


class _Browser:
    def is_connected(self):
        return True

    async def close(self):
        pass


def _fake_driver(monkeypatch, module: str, record: dict):
    class Chromium:
        async def launch(self, **kw):
            record["launch"] = kw
            return _Browser()

        async def connect(self, ws, **kw):
            record["connect"] = (ws, kw)
            return _Browser()

    class PW:
        chromium = Chromium()

        async def stop(self):
            pass

    class Starter:
        async def start(self):
            return PW()

    mod = types.ModuleType(module)
    mod.async_playwright = lambda: Starter()
    monkeypatch.setitem(sys.modules, module, mod)


PROXY = types.SimpleNamespace(server_url="http://127.0.0.1:9")


async def test_local_launch_uses_the_sandbox_and_a_clean_env(monkeypatch):
    rec: dict = {}
    _fake_driver(monkeypatch, "playwright.async_api", rec)
    monkeypatch.setenv("BROWSER_SERVICE_TOKEN", "x" * 40)
    p = BrowserPool(PROXY)
    await p.start()
    kw = rec["launch"]
    assert kw["chromium_sandbox"] is True and kw["args"] == CHROMIUM_ARGS
    assert "BROWSER_SERVICE_TOKEN" not in kw["env"]
    await p.stop()
    rec.clear()
    p = BrowserPool(PROXY, sandbox=False)
    await p.start()
    assert rec["launch"]["chromium_sandbox"] is False
    await p.stop()


async def test_split_mode_connects_to_the_playwright_server(monkeypatch):
    rec: dict = {}
    _fake_driver(monkeypatch, "playwright.async_api", rec)
    p = BrowserPool(PROXY, remote_ws="ws://127.0.0.1:8095/abc")
    await p.start()
    assert "launch" not in rec
    ws, kw = rec["connect"]
    assert ws == "ws://127.0.0.1:8095/abc"
    opts = json.loads(kw["headers"]["x-playwright-launch-options"])
    assert opts["chromiumSandbox"] is True and opts["headless"] is True
    assert opts["proxy"] == {"server": "http://127.0.0.1:9"} and opts["args"] == CHROMIUM_ARGS
    await p.stop()


def test_app_passes_sandbox_and_server_settings():
    from browser_service.app import create_app
    from conftest import make_settings

    app = create_app(make_settings(chromium_sandbox=False, playwright_ws="ws://127.0.0.1:8095/p", maps_stealth=True))
    deps = app.state.deps
    assert deps.pool.sandbox is False and deps.pool.remote_ws == "ws://127.0.0.1:8095/p"
    assert deps.stealth_pool.driver == "patchright" and deps.stealth_pool.sandbox is False


def test_entrypoint_script_exists_and_splits_users():
    from pathlib import Path

    script = (Path(__file__).parent.parent / "scripts" / "entrypoint.sh").read_text()
    assert "setpriv" in script and "--reuid=10002" in script and "--reuid=10001" in script
    assert "run-server" in script and "env -i" in script
    assert "BROWSER_SERVICE_TOKEN" not in script.split("env -i", 1)[1].split("\n", 1)[0]
