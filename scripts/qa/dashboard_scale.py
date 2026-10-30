"""Check adaptive scale, saved preferences, portals, and workbook pointer geometry.

Reads native workspace data. Intercepts width saves so workbook data is unchanged.
"""
import argparse
import json
import os
import re
import urllib.parse
import urllib.request
from pathlib import Path
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument("--engine", choices=["chromium", "webkit"], default="chromium")
parser.add_argument("--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE"))
parser.add_argument("--output", default=".run/ui-scale")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output / args.engine
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip("/")
username, password = os.environ.get("OPENGTM_QA_USERNAME"), os.environ.get("OPENGTM_QA_PASSWORD")
if not username or not password:
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD")
request = urllib.request.Request(base + "/auth/token", data=urllib.parse.urlencode({"username": username, "password": password}).encode())
with urllib.request.urlopen(request, timeout=15) as response:
    token = json.load(response)["access_token"]
report = {"engine": args.engine, "checks": [], "page_errors": []}

def geometry(page):
    return page.evaluate("""()=>({scale:OpenGTMScale.getSnapshot(), height:innerHeight,
      main:document.querySelector('#main-content').getBoundingClientRect().toJSON(),
      sidebar:document.querySelector('[data-slot="sidebar-container"]')?.getBoundingClientRect().toJSON(),
      width:innerWidth, scroll:document.documentElement.scrollWidth})""")

def audit(page, name):
    page.add_script_tag(path=str(root / "apps/web/node_modules/axe-core/axe.min.js"))
    result = page.evaluate("async()=>await axe.run(document)")
    report["checks"].append({"name": name, "violations": result["violations"]})
    assert not result["violations"], (name, result["violations"])

with sync_playwright() as p:
    browser = getattr(p, args.engine).launch(headless=True, executable_path=args.browser_path if args.engine == "chromium" else None)
    def context(width=1280, height=832, touch=False, dpr=1):
        c = browser.new_context(viewport={"width": width, "height": height}, has_touch=touch, device_scale_factor=dpr, reduced_motion="reduce")
        c.add_init_script('localStorage.setItem("yupcha_token",' + json.dumps(token) + ');localStorage.setItem("theme","dark");')
        return c
    try:
        for name, width, height, touch, dpr, expected in [
            ("retina-laptop",1280,832,False,2,85),
            ("large-desktop",1920,1080,False,1,100),
            ("narrow-window",900,700,False,1,100),
            ("touch-laptop",1280,832,True,1,100),
            ("phone",390,844,True,2,100),
            ("reflow",320,700,False,1,100),
        ]:
            c=context(width,height,touch,dpr)
            page=c.new_page()
            page.on("pageerror",lambda error:report["page_errors"].append(str(error)))
            page.goto(base+"/overview",wait_until="domcontentloaded")
            expect(page.locator(".overview-metric")).to_have_count(4)
            g=geometry(page)
            assert g["scale"]==f"auto:{expected}", (name,g)
            assert abs(g["main"]["height"]-height)<2, (name,g)
            assert g["scroll"]<=width+1, (name,g)
            if width>=1024 and not touch:
                assert abs(g["sidebar"]["width"]-192*expected/100)<2, (name,g)
            report["checks"].append({"name":name,"geometry":g})
            page.mouse.click(width-10,height-10)
            page.screenshot(path=str(out/f"{name}.png"))
            page.evaluate("OpenGTMTheme.setPreference('light')")
            audit(page,name+"-light")
            page.evaluate("OpenGTMTheme.setPreference('dark')")
            audit(page,name+"-dark")
            if name=="retina-laptop":
                page.get_by_title("Account menu",exact=True).click()
                page.get_by_role("menuitem",name=re.compile("^UI scale")).click()
                auto=page.get_by_role("menuitemradio",name="Auto · fit window")
                expect(auto).to_have_attribute("aria-checked","true")
                box=page.get_by_role("menu").first.bounding_box()
                assert 0<=box["x"] and box["x"]+box["width"]<=width and box["y"]+box["height"]<=height
                anchor=page.get_by_title("Account menu",exact=True).bounding_box()
                assert 0<=box["x"]-(anchor["x"]+anchor["width"])<=10,(box,anchor)
                assert abs(box["y"]+box["height"]-anchor["y"]-anchor["height"])<2,(box,anchor)
                page.get_by_role("menuitemradio",name="Comfortable · 100%").click()
                page.keyboard.press("Escape")
                assert page.evaluate("OpenGTMScale.getSnapshot()") == "100:100"
                page.reload(wait_until="domcontentloaded")
                expect(page.locator(".overview-metric")).to_have_count(4)
                assert page.evaluate("OpenGTMScale.getSnapshot()") == "100:100"
                page.get_by_title("Account menu",exact=True).click()
                page.get_by_role("menuitem",name=re.compile("^UI scale")).click()
                page.get_by_role("menuitemradio",name="Compact · 85%").click()
                page.keyboard.press("Escape")
                assert page.evaluate("OpenGTMScale.getSnapshot()") == "85:85"
                # Cross-tab updates use the same persistent preference store.
                other=c.new_page()
                other.goto(base+"/overview",wait_until="domcontentloaded")
                expect(other.locator(".overview-metric")).to_have_count(4)
                other.evaluate("OpenGTMScale.setPreference('auto')")
                expect(page.locator("html")).to_have_attribute("data-ui-scale","85")
                page.wait_for_function("OpenGTMScale.getSnapshot()==='auto:85'")
                other.close()
                # Playwright resizes the emulated viewport, not the native
                # headless window. Model a matching logical window resize.
                page.evaluate("Object.defineProperties(window,{outerWidth:{get:()=>innerWidth,configurable:true},outerHeight:{get:()=>innerHeight,configurable:true}})")
                page.set_viewport_size({"width":1920,"height":1080})
                expect(page.locator("html")).to_have_attribute("data-ui-scale","100")
                page.set_viewport_size({"width":1280,"height":832})
                expect(page.locator("html")).to_have_attribute("data-ui-scale","85")
                page.get_by_role("link",name="Companies",exact=True).first.click()
                expect(page.locator(".records-table")).to_be_visible()
                page.locator(".workbench-header-actions").get_by_role("button",name=re.compile("^Import")).click()
                dialog=page.get_by_role("dialog")
                expect(dialog).to_be_visible()
                box=dialog.bounding_box()
                assert box["x"]>=0 and box["y"]>=0 and box["x"]+box["width"]<=1281 and box["y"]+box["height"]<=833,box
                audit(page,"import-dialog-85")
                page.keyboard.press("Escape")
                page.get_by_role("link",name="Overview",exact=True).click()
                expect(page.locator(".overview-metric")).to_have_count(4)
                workbook=page.locator(".overview-workbook").first
                if workbook.count():
                    workbook.click()
                    separator=page.get_by_role("separator",name="Resize",exact=False).first
                    expect(separator).to_be_visible()
                    saved=[]
                    def save(route):
                        body=route.request.post_data_json
                        col=urllib.parse.unquote(route.request.url.split("/")[-2])
                        saved.append(body)
                        route.fulfill(json={"column_id":col,"width":body["width"]})
                    page.route("**/columns/*/width",save)
                    before=separator.get_attribute("aria-valuenow")
                    box=separator.bounding_box()
                    page.mouse.move(box["x"]+box["width"]/2,box["y"]+box["height"]/2)
                    page.mouse.down()
                    page.mouse.move(box["x"]+box["width"]/2+51,box["y"]+box["height"]/2,steps=8)
                    page.mouse.up()
                    expect(separator).to_have_attribute("aria-valuenow",str(int(before)+60))
                    assert saved and abs(saved[0]["width"]-int(before)-60)<1,saved
                    report["checks"].append({"name":"workbook-resize-85","visual_drag":51,"layout_delta":60})
                    page.screenshot(path=str(out/"workbook-85.png"))
            c.close()
        assert not report["page_errors"],report["page_errors"]
    finally:
        (out/"results.json").write_text(json.dumps(report,indent=2)+"\n")
        browser.close()
print(json.dumps({"engine":args.engine,"checks":len(report["checks"]),"page_errors":report["page_errors"]}))
