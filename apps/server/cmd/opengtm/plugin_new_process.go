package main

import (
	"fmt"
	"strings"
)

// Scaffolds for runtime "process" (Python, opengtm_sdk). Placeholders:
// __NAME__ (plugin name), __ENV__ (secret name), __KIND__, __INPUT__, __EXTRA__.

const processManifest = `# Python __KIND__ (runtime: process, opengtm_sdk). Docs: docs/plugins/python.md
manifest_version: "2"
name: __NAME__
kind: __KIND__
runtime: process
version: 0.1.0
author: your-github-handle
license: Apache-2.0
description: Describe what this plugin does.
capabilities:
  network: ["https://api.example.com/v1"]   # fetch() may only reach these
  secrets: [__SECRETS__]                    # ctx.secret() may only read these
limits:
  timeout_seconds: 20
  max_pages: 5                              # fetch budget per run
  memory_mb: 128                            # address-space limit for the process
inputs: { __INPUT__: string }
__EXTRA__process:
  command: [python3, main.py]
`

var processBodies = map[string]string{
	"provider": `from opengtm_sdk import provider, run


@provider
def enrich(ctx, domain: str):
    r = ctx.fetch(
        "https://api.example.com/v1/lookup",
        params={"domain": domain},
        headers={"Authorization": "Bearer " + ctx.secret("__ENV__")},
    )
    if r.status == 404:
        return None  # no answer for this domain; not a failure
    return {"company_size": r.json()["data"]["employees"]}


if __name__ == "__main__":
    run()
`,
	"scraper": `from html.parser import HTMLParser

from opengtm_sdk import Record, run, scraper


class Team(HTMLParser):
    """Collects (name, role) from <div class="team-member"> blocks."""

    def __init__(self):
        super().__init__()
        self.people, self._field = [], None

    def handle_starttag(self, tag, attrs):
        cls = dict(attrs).get("class", "")
        if "team-member" in cls:
            self.people.append({})
        elif "name" in cls.split():
            self._field = "full_name"
        elif "role" in cls.split():
            self._field = "title"

    def handle_data(self, data):
        if self._field and self.people and data.strip():
            self.people[-1][self._field] = data.strip()
            self._field = None


@scraper
def team(ctx, domain: str):
    url = f"https://{domain}/team"
    page = ctx.fetch(url)  # robots.txt and the per-domain rate limit apply
    parser = Team()
    parser.feed(page.text)
    for person in parser.people:
        yield Record(person, evidence=ctx.evidence(url))


if __name__ == "__main__":
    run()
`,
	"function": `from opengtm_sdk import function, run


@function
def call(ctx, text: str):
    return text.strip().lower()


if __name__ == "__main__":
    run()
`,
	"tool": `from opengtm_sdk import tool, run


@tool
def invoke(ctx, query: str):
    return {"echo": query}


if __name__ == "__main__":
    run()
`,
}

func processScaffold(kind string) (map[string]string, error) {
	body, ok := processBodies[kind]
	if !ok {
		return nil, fmt.Errorf("process scaffolds exist for provider, scraper, function and tool (not %s)", kind)
	}
	input, secrets, extra := "domain", "__ENV__", ""
	switch kind {
	case "provider":
		extra = "outputs: company\ncapability: company_size\n"
	case "scraper":
		secrets, extra = "", "outputs: person\n"
	case "function":
		input, secrets = "text", ""
	case "tool":
		input, secrets = "query", ""
	}
	m := strings.NewReplacer("__KIND__", kind, "__INPUT__", input, "__EXTRA__", extra, "__SECRETS__", secrets).Replace(processManifest)
	if kind == "scraper" {
		m = strings.Replace(m, `["https://api.example.com/v1"]   # fetch()`, `["https://*.example.com"]        # fetch()`, 1)
	}
	files := map[string]string{
		"plugin.yaml": m,
		"main.py":     body,
		".gitignore":  "__pycache__/\n",
	}
	switch kind {
	case "provider":
		files["fixtures/basic/case.yaml"] = `description: A known domain returns its employee count.
input: { domain: acme.example }
secrets: { __ENV__: test-key }
http:
  - method: GET
    url: https://api.example.com/v1/lookup?domain=acme.example
    status: 200
    headers: { Content-Type: application/json }
    body: '{"data": {"employees": 120}}'
expect:
  match: exact
  fields: { company_size: 120 }
`
		files["fixtures/not_found/case.yaml"] = `description: An unknown domain is a normal "no result", not a failure.
input: { domain: unknown.example }
secrets: { __ENV__: test-key }
http:
  - method: GET
    url: https://api.example.com/v1/lookup?domain=unknown.example
    status: 404
    body: '{}'
expect:
  error: no_data
  fields: {}
`
	case "scraper":
		files["fixtures/basic/case.yaml"] = `description: One team page with two people.
input: { domain: www.example.com }
http:
  - method: GET
    url: https://www.example.com/team
    status: 200
    headers: { Content-Type: text/html }
    body_file: team.html
expect:
  match: exact
  records:
    - { full_name: Ada Lovelace, title: CEO }
    - { full_name: Alan Turing, title: CTO }
`
		files["fixtures/basic/team.html"] = `<html><body>
<div class="team-member"><h3 class="name">Ada Lovelace</h3><p class="role">CEO</p></div>
<div class="team-member"><h3 class="name">Alan Turing</h3><p class="role">CTO</p></div>
</body></html>
`
	case "function":
		files["fixtures/basic/case.yaml"] = "description: Pure function, no HTTP.\ninput: { text: \"  Hello \" }\nhttp: []\nexpect:\n  fields: { result: hello }\n"
	case "tool":
		files["fixtures/basic/case.yaml"] = "description: Echo tool, no HTTP.\ninput: { query: ping }\nhttp: []\nexpect:\n  fields: { output: { echo: ping } }\n"
	}
	return files, nil
}
