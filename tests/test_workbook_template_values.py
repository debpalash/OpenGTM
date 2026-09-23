import json
import pytest


def test_unknown_output_placeholder_never_opens_http_client(monkeypatch):
    import asyncio
    from apps.api.services.workbook import output
    monkeypatch.setattr(output, "_is_safe_public_url", lambda _: (True, ""))
    def forbidden(*args, **kwargs):
        pytest.fail("Invalid output must not open an HTTP client")
    monkeypatch.setattr(output.httpx, "AsyncClient", forbidden)
    with pytest.raises(ValueError, match="Unknown column reference: missing"):
        asyncio.run(output._send_webhook({"url": "https://fixture.example.test", "body": {"email": "{missing}"}}, {}, []))
    result = asyncio.run(output.execute_output_column(
        {"destination": "webhook", "destination_config": {"url": "https://fixture.example.test", "body": {"email": "{missing}"}}},
        {}, [], "fixture", 1))
    assert result["success"] is False and result["value"] is None
    assert "Unknown column reference" in result["error"]
    assert output._resolve_deep({"optional": "{email}"}, {"email": None}, []) == {"optional": ""}

from apps.api.services.workbook.ai_column import _get_row_values, _resolve_prompt
from apps.api.services.workbook.enrichment import _get_lead_values
from apps.api.services.workbook.output import _resolve_deep


@pytest.mark.parametrize("reverse", [False, True])
def test_duplicate_display_names_require_exact_ids_before_projection(reverse):
    from apps.api.services.workbook.ai_column import build_ai_prompt
    columns = [{"id": "first", "name": "Email"}, {"id": "second", "name": "Email"}]
    if reverse:
        columns.reverse()
    row = {"first": "one@example.test", "second": "two@example.test"}
    for render in (lambda template: build_ai_prompt(template, row, columns),
                   lambda template: _resolve_deep(template, row, columns)):
        for template in ("{Email}", "{EMAIL}", "{email}"):
            with pytest.raises(ValueError, match="Ambiguous column reference"):
                render(template)
        assert "one@example.test" in render("{first}")
        assert "two@example.test" in render("{second}")


@pytest.mark.parametrize("value,expected", [(0, "0"), (False, "False"), (None, ""), ("", ""), (12, "12")])
def test_values_survive_prompt_and_output_rendering(value, expected):
    columns = [{"id": "score", "name": "Fit Score", "type": "formula"},
               {"id": "input", "type": "lead_field", "lead_field": "raw"}]
    cells = _get_row_values({"score": {"value": value}}, columns)
    assert cells["score"] == cells["Fit Score"] == expected
    assert _resolve_prompt("{score}|{FIT SCORE}|{fit_score}", cells) == "|".join([expected] * 3)
    # Direct resolver callers can pass typed values, not only flattened strings.
    assert _resolve_prompt("{score}", {"score": value}) == expected
    row = {"score": value, "raw": value, "extra": value}
    flattened = _get_lead_values(row, columns)
    assert flattened == {"score": expected, "Fit Score": expected, "input": expected, "raw": expected, "extra": expected}
    assert _resolve_deep({"fields": ["{score}", "{input}", "{extra}"], "typed": value}, row, columns) == {
        "fields": [expected] * 3, "typed": value}


@pytest.mark.parametrize("reverse", [False, True])
def test_stable_ids_win_over_other_columns_display_aliases(reverse):
    columns = [{"id": "email", "name": "Contact email", "type": "formula"},
               {"id": "other", "name": "email", "type": "formula"}]
    pairs = [("email", "right@example.test"), ("other", "wrong@example.test")]
    if reverse:
        pairs.reverse()
        columns.reverse()
    row = dict(pairs)
    for values in (_get_row_values(row, columns), _get_lead_values(row, columns)):
        assert _resolve_prompt("{email}", values) == "right@example.test"
        assert _resolve_prompt("{Contact email}", values) == "right@example.test"
    assert _resolve_deep("{Contact email}", {**row, "Contact email": "stale"}, columns) == "right@example.test"


@pytest.mark.parametrize("keys,reference", [(["Email", "EMAIL"], "email"), (["Fit_Score Name", "Fit Score_Name"], "fit_score_name")])
@pytest.mark.parametrize("reverse", [False, True])
def test_ambiguous_fallback_references_do_not_choose_insertion_order(keys, reference, reverse):
    pairs = list(zip(keys, ["first private value", "second private value"]))
    if reverse:
        pairs.reverse()
    with pytest.raises(ValueError, match="Ambiguous column reference") as error:
        _resolve_prompt("{" + reference + "}", dict(pairs))
    assert "private value" not in str(error.value)
    assert _resolve_prompt("{" + keys[0] + "}", dict(pairs)) == "first private value"
    assert _resolve_prompt("{" + reference + "}", {key: "same" for key in keys}) == "same"


def test_default_sequencer_map_drops_absent_optional_fields():
    from apps.api.services.workbook import output
    lead = output._map_lead_fields(
        output._SEQUENCER_DEFAULT_FIELD_MAP,
        {"email": "a@b.co", "company": "Acme"}, [])
    assert lead == {"email": "a@b.co", "company_name": "Acme"}


def test_sequencer_map_ambiguous_reference_fails_before_vendor_call(monkeypatch):
    import asyncio
    from apps.api.services.integrations import instantly
    from apps.api.services.workbook import output

    async def forbidden(*args, **kwargs):
        pytest.fail("Ambiguous field map must not reach the vendor")
    monkeypatch.setattr(instantly, "add_lead_to_campaign", forbidden)
    monkeypatch.setattr(output, "_resolve", lambda *a, **k: (_ for _ in ()).throw(
        ValueError("Ambiguous column reference: name. Use an exact column ID.")))
    result = asyncio.run(output.execute_output_column(
        {"destination": "instantly",
         "destination_config": {"campaign_id": "c1", "field_map": {"name": "first_name"}}},
        {"email": "a@b.co"}, [], "fixture", 1))
    assert result["success"] is False and result["value"] is None
    assert "Ambiguous column reference" in result["error"]

def test_default_sequencer_map_reaches_vendor_without_optional_fields(monkeypatch):
    import asyncio
    from apps.api.services.integrations import instantly
    from apps.api.services.workbook import output
    sent = {}

    async def fake_add(campaign_id, lead, **kwargs):
        sent.update(lead)
        return {"success": True}
    monkeypatch.setattr(instantly, "add_lead_to_campaign", fake_add)
    result = asyncio.run(output.execute_output_column(
        {"destination": "instantly", "destination_config": {"campaign_id": "c1"}},
        {"email": "a@b.co"}, [], "fixture", 1))
    assert result["success"] is True
    assert sent == {"email": "a@b.co"}


def test_json_string_webhook_body_escapes_values_with_quotes(monkeypatch):
    import asyncio
    import httpx
    from apps.api.services.workbook import output
    monkeypatch.setattr(output, "_is_safe_public_url", lambda _: (True, ""))
    sent = {}

    def handler(request):
        sent["type"] = request.headers.get("content-type")
        sent["body"] = request.content
        return httpx.Response(200, json={"ok": True})
    real = httpx.AsyncClient
    monkeypatch.setattr(output.httpx, "AsyncClient",
                        lambda **kw: real(transport=httpx.MockTransport(handler), **kw))
    note = 'Said "hi"\nthen left'
    result = asyncio.run(output._send_webhook(
        {"url": "https://fixture.example.test", "body": '{"note": "{icebreaker}"}'},
        {"icebreaker": note}, []))
    assert result["success"] is True
    assert sent["type"] == "application/json"
    assert json.loads(sent["body"]) == {"note": note}
