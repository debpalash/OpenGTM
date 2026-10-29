from playwright.sync_api import sync_playwright
from pathlib import Path
import urllib.request, urllib.parse, json
import argparse, os

parser = argparse.ArgumentParser(
    description="Capture the dashboard in both themes and scan rendered screens with axe. Temporarily switches the active workspace and restores it on exit."
)
parser.add_argument(
    "--engine", choices=["chromium", "firefox", "webkit"], default="chromium"
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--axe-path", default="apps/web/node_modules/axe-core/axe.min.js")
parser.add_argument("--output", default=".run/dashboard-matrix")
parser.add_argument("--workspace-id")
parser.add_argument("--workbook-id")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
base = args.base_url.rstrip("/")
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
shots = out / "captures"
shots.mkdir(exist_ok=True)
headers = {}


def req(method, path, body=None, form=False):
    data = (
        None
        if body is None
        else (
            urllib.parse.urlencode(body).encode() if form else json.dumps(body).encode()
        )
    )
    r = urllib.request.Request(
        base + path,
        data=data,
        method=method,
        headers={
            **headers,
            "Content-Type": "application/x-www-form-urlencoded"
            if form
            else "application/json",
        },
    )
    with urllib.request.urlopen(r, timeout=30) as o:
        return json.load(o)


creds = {
    "USERNAME": os.environ.get("OPENGTM_QA_USERNAME"),
    "PASSWORD": os.environ.get("OPENGTM_QA_PASSWORD"),
}
if not all(creds.values()):
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD in the environment.")
headers["Authorization"] = (
    "Bearer "
    + req(
        "POST",
        "/auth/token",
        {"username": creds["USERNAME"], "password": creds["PASSWORD"]},
        True,
    )["access_token"]
)
prior = req("GET", "/api/workspaces")["active_id"]
ws = args.workspace_id or prior
if not ws:
    parser.error("No accessible workspace. Create one before scanning.")
headers["X-Workspace-Id"] = ws
summary = req("GET", "/api/records/summary")
workbook = args.workbook_id or (
    summary["workbooks"][0]["id"] if summary["workbooks"] else None
)
routes = [
    ("overview", "/overview"),
    ("chat", "/chat"),
    ("companies", "/companies"),
    ("people", "/people"),
    ("workbooks", "/workbooks"),
    ("audiences", "/audiences"),
    ("signals", "/signals"),
    ("outreach", "/outreach"),
    ("analytics", "/analytics"),
    ("settings", "/settings"),
    ("sources", "/sources"),
    ("automations", "/automations"),
]
if workbook:
    routes.insert(5, ("workbook", "/workbooks/" + workbook))
metric_js = r"""() => {
 const visible=e=>{const r=e.getBoundingClientRect(),s=getComputedStyle(e);return r.width>0&&r.height>0&&s.visibility!=='hidden'&&s.display!=='none'};
 const within=e=>{const r=e.getBoundingClientRect();return visible(e)&&r.bottom>0&&r.top<innerHeight&&r.right>0&&r.left<innerWidth};
 const text=[...document.querySelectorAll('body *')].filter(e=>within(e)&&e.children.length===0&&e.textContent.trim());
 const fonts={};for(const e of text){const s=getComputedStyle(e);const k=s.fontSize;fonts[k]=(fonts[k]||0)+1}
 const controls=[...document.querySelectorAll('button,a[href],input:not([type=hidden]),select,textarea,[role=button],[role=checkbox]')].filter(within);
 const small=controls.filter(e=>{const r=e.getBoundingClientRect();return r.width<24||r.height<24}).map(e=>{const r=e.getBoundingClientRect();return {tag:e.tagName,name:e.getAttribute('aria-label')||e.textContent.trim().slice(0,70)||e.getAttribute('title')||'',width:+r.width.toFixed(1),height:+r.height.toFixed(1)}});
 const tokens={};const cs=getComputedStyle(document.documentElement);for(const t of ['--gtm-accent','--gtm-accent-foreground','--gtm-control-md','--gtm-focus-ring','--t-font-color-primary','--t-font-color-secondary','--t-font-color-tertiary','--t-font-color-light','--t-background-primary','--t-background-secondary','--t-border-color-strong','--t-border-radius-md','--gtm-font-sans'])tokens[t]=cs.getPropertyValue(t).trim();
 const regions=[...document.querySelectorAll('*')].filter(e=>within(e)&&e.scrollWidth>e.clientWidth+2&&e.clientWidth>100&&['auto','scroll','hidden'].includes(getComputedStyle(e).overflowX)).map(e=>({tag:e.tagName,role:e.getAttribute('role'),class:e.className?.baseVal??e.className,width:e.clientWidth,scrollWidth:e.scrollWidth,overflowX:getComputedStyle(e).overflowX}));
 return {url:location.pathname,title:document.title,viewport:{width:innerWidth,height:innerHeight},documentWidth:document.documentElement.scrollWidth,headings:[...document.querySelectorAll('h1,h2,h3')].filter(visible).map(e=>({level:e.tagName,text:e.textContent.trim()})),mainCount:document.querySelectorAll('main,[role=main]').length,skipLinkCount:[...document.querySelectorAll('a')].filter(e=>/skip.*content/i.test(e.textContent)).length,visibleTextSizeCounts:fonts,visibleControls:controls.length,under24Candidates:small,horizontalRegions:regions,tokens,tableRows:document.querySelectorAll('tbody tr').length,grids:[...document.querySelectorAll('[role=grid]')].map(e=>({name:e.getAttribute('aria-label'),rowcount:e.getAttribute('aria-rowcount'),colcount:e.getAttribute('aria-colcount')}))};
}"""
results = []
errors = []


def capture(page, label, path, theme, width=1440, height=1000, scan=True):
    page.set_viewport_size({"width": width, "height": height})
    page.add_init_script("(s)=>{}") if False else None
    page.evaluate(
        "(theme)=>window.OpenGTMTheme?.setPreference(theme)", theme
    ) if page.url.startswith(base) else None
    page.goto(base + path, wait_until="domcontentloaded")
    page.wait_for_selector("main", timeout=20000)
    try:
        page.wait_for_load_state("networkidle", timeout=3500)
    except:
        pass
    page.wait_for_function(
        "!document.querySelector('main [role=status] .animate-spin')", timeout=15000
    )
    page.wait_for_timeout(180)
    page.add_script_tag(path=str(root / args.axe_path))
    res = page.evaluate(metric_js)
    res["label"] = label
    res["theme"] = theme
    res["screenshot"] = label + ".png"
    if scan:
        audit = page.evaluate(
            "async()=>await axe.run(document,{runOnly:{type:'tag',values:['wcag2a','wcag2aa','wcag21a','wcag21aa','wcag22aa','best-practice']}})"
        )
        res["axe"] = {
            k: audit[k]
            for k in ["testEngine", "testEnvironment", "violations", "incomplete"]
        }
    page.screenshot(path=str(shots / (label + ".png")), full_page=False)
    results.append(res)
    (out / "browser-results.json").write_text(
        json.dumps(
            {
                "workspace_id": ws,
                "workbook_id": workbook,
                "results": results,
                "page_errors": errors,
            },
            indent=2,
        )
    )
    print(
        label,
        "rules",
        len(res.get("axe", {}).get("violations", [])),
        "small",
        len(res["under24Candidates"]),
        "h1",
        sum(h["level"] == "H1" for h in res["headings"]),
        flush=True,
    )


try:
    req("POST", "/api/workspaces/switch", {"workspace_id": ws})
    with sync_playwright() as p:
        browser = getattr(p, args.engine).launch(
            headless=True,
            executable_path=args.browser_path if args.engine == "chromium" else None,
        )
        context = browser.new_context(
            viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
        )
        context.add_init_script("(x)=>{}") if False else None
        context.add_init_script(
            'localStorage.setItem("yupcha_token",'
            + json.dumps(headers["Authorization"][7:])
            + ');localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(ws)
            + ');localStorage.setItem("theme","light")'
        )
        page = context.new_page()
        page.on("pageerror", lambda e: errors.append(str(e)))
        for theme in ["light", "dark"]:
            for label, path in routes:
                # Apply the scheme after each full navigation: the init script seeds light only on first load.
                page.goto(base + path, wait_until="domcontentloaded")
                page.evaluate(
                    '(t)=>{localStorage.setItem("theme",t);window.OpenGTMTheme?.setPreference(t)}',
                    theme,
                )
                context.add_init_script(
                    'localStorage.setItem("theme",' + json.dumps(theme) + ")"
                )
                capture(page, label + "-" + theme, path, theme)
        for theme in ["light", "dark"]:
            context.add_init_script(
                'localStorage.setItem("theme",' + json.dumps(theme) + ")"
            )
            for width in [320, 390]:
                for label, path in [
                    ("companies", "/companies"),
                    ("people", "/people"),
                    ("overview", "/overview"),
                ] + ([("workbook", "/workbooks/" + workbook)] if workbook else []):
                    capture(
                        page,
                        label + "-" + theme + "-" + str(width),
                        path,
                        theme,
                        width,
                        844,
                    )
        browser.close()
finally:
    headers.pop("X-Workspace-Id", None)
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})

if errors or any(result.get("axe", {}).get("violations") for result in results):
    raise SystemExit(
        "Dashboard matrix found accessibility violations or page errors; inspect browser-results.json."
    )
