"""Capture truthful README/launch media from a running local OpenGTM instance.

Usage:
    uv run python scripts/capture_launch_assets.py

The script expects the web app at http://127.0.0.1:4099 and the seeded local
demo credentials (admin/admin). Override with OPENGTM_URL, OPENGTM_USERNAME,
and OPENGTM_PASSWORD. The output is deterministic and safe to regenerate.
"""

from __future__ import annotations

import os
from pathlib import Path

from PIL import Image
from playwright.sync_api import Page, sync_playwright


ROOT = Path(__file__).resolve().parents[1]
OUTPUT = ROOT / "docs" / "assets"
BASE_URL = os.getenv("OPENGTM_URL", "http://127.0.0.1:4099")
USERNAME = os.getenv("OPENGTM_USERNAME", "admin")
PASSWORD = os.getenv("OPENGTM_PASSWORD", "admin")


def shot(page: Page, name: str) -> Path:
    path = OUTPUT / name
    page.screenshot(path=str(path), animations="disabled")
    return path


def make_gif(frames: list[Path], output: Path) -> None:
    images = [Image.open(path).convert("RGB") for path in frames]
    durations = [1300] + [1000] * (len(images) - 2) + [1800]
    images[0].save(
        output,
        save_all=True,
        append_images=images[1:],
        duration=durations,
        loop=0,
        optimize=True,
    )


def main() -> None:
    OUTPUT.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch()
        page = browser.new_page(viewport={"width": 1440, "height": 900}, device_scale_factor=1)
        page.set_default_timeout(10_000)

        page.goto(f"{BASE_URL}/login", wait_until="domcontentloaded")
        login = shot(page, "opengtm-login.png")
        page.get_by_label("Username").fill(USERNAME)
        page.get_by_label("Password").fill(PASSWORD)
        page.get_by_role("button", name="Enter OpenGTM").click()
        page.wait_for_url("**/chat")

        page.goto(f"{BASE_URL}/workbooks/demo-zero-key-firstrun", wait_until="domcontentloaded")
        page.get_by_text("Demo", exact=False).first.wait_for(timeout=15_000)
        workbook = shot(page, "opengtm-workbook.png")

        page.get_by_role("button", name="Source Engine", exact=True).click(force=True)
        page.wait_for_timeout(500)
        shot(page, "opengtm-source-engine.png")
        page.get_by_role("tab").nth(1).click(force=True)
        page.wait_for_timeout(500)
        cost = shot(page, "opengtm-cost-control.png")

        # The GIF is deliberately a short product tour rather than a huge video
        # disguised as a GIF; GitHub loads it quickly and every frame is legible.
        make_gif([login, workbook, cost, workbook], OUTPUT / "opengtm-demo.gif")
        browser.close()

    print(f"Launch assets written to {OUTPUT}")


if __name__ == "__main__":
    main()
