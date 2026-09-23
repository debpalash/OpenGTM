"""Provider 'Test' surfaces the real provider error, whatever its body shape."""

import pytest

from apps.api.routers.settings import _provider_error_message


@pytest.mark.parametrize("body, expected", [
    # Google's OpenAI-compatible endpoint returns a list (previously crashed with
    # "'list' object has no attribute 'get'", hiding a leaked-key 403).
    ([{"error": {"code": 403, "message": "Your API key was reported as leaked.",
                 "status": "PERMISSION_DENIED"}}], "Your API key was reported as leaked."),
    ({"error": {"message": "Invalid API key"}}, "Invalid API key"),
    ({"error": {"code": "model_not_found"}}, "model_not_found"),
    ({"error": "rate limited"}, "rate limited"),
    ({"message": "Unauthorized"}, "Unauthorized"),
    (None, "<html>Bad gateway</html>"),
])
def test_provider_error_message_handles_every_body_shape(body, expected):
    assert _provider_error_message(body, "<html>Bad gateway</html>") == expected
