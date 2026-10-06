import json

import pytest

from apps.api.services.leadgen.enrichment.providers.jsonld_firmographics import extract_firmographics


@pytest.mark.parametrize("tag", [
    '<meta content="Company biography" property="og:description">',
    '<meta name = "og:description" content = "Company biography">',
    '<META CONTENT="Company biography" PROPERTY="og:description">',
])
def test_opengraph_reads_valid_attribute_order_and_whitespace(tag):
    assert extract_firmographics(tag)["description"] == "Company biography"


@pytest.mark.parametrize("description", ["Bob's company", 'Say "hello"', "Retail &amp; services"])
def test_opengraph_preserves_complete_quoted_attribute_value(description):
    import html
    quote = "\"" if "\"" not in description else "'"
    page = '<meta property="og:description" content=' + quote + html.escape(description, quote=False) + quote + ">"
    assert extract_firmographics(page)["description"] == description


@pytest.mark.parametrize("attribute", [
    'type = "application/ld+json"',
    "type='APPLICATION/LD+JSON'",
    'type="application/ld+json"',
])
def test_jsonld_script_reads_valid_type_attribute_syntax(attribute):
    page = '<script ' + attribute + '>' + json.dumps({"@type": "Organization", "name": "Acme"}) + '</script>'
    assert extract_firmographics(page)["company"] == "Acme"


def test_other_scripts_and_metadata_are_not_company_evidence():
    page = '<script type="application/json">{"@type":"Organization","name":"Wrong"}</script><meta name="description" content="Not OpenGraph">'
    assert extract_firmographics(page) == {}
