import json

import pytest

from apps.api.services.leadgen.enrichment.providers.jsonld_firmographics import extract_firmographics


def fields_for(same_as):
    organization = {"@type": "Organization", "name": "Relax", "sameAs": same_as}
    return extract_firmographics('<script type="application/ld+json">' + json.dumps(organization) + '</script>')


@pytest.mark.parametrize("url", [
    "https://relax.com/about", "https://relax.com/about?source=twitter.com",
    "https://relax.com/linkedin.com/about", "https://relax.com/facebook.com/about",
])
def test_company_same_as_links_are_not_social_profiles(url):
    assert fields_for([url]) == {"company": "Relax"}


@pytest.mark.parametrize("url, field", [
    ("https://www.linkedin.com/company/relax", "linkedin_url"),
    ("https://TWITTER.COM/relax", "twitter_url"),
    ("https://x.com/relax", "twitter_url"),
    ("https://www.facebook.com/relax", "facebook_url"),
])
def test_social_profile_hosts_preserve_original_url(url, field):
    assert fields_for(url) == {"company": "Relax", field: url}


def test_non_social_same_as_does_not_preempt_valid_profile():
    assert fields_for(["https://relax.com/about", "https://x.com/relax"]) == {
        "company": "Relax", "twitter_url": "https://x.com/relax",
    }
