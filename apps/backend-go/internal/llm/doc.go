// Package llm is the Go port of leadgen/llm.py (the provider-agnostic
// completion client with failover) and copilot/llm_stream.py (one streamed
// chat round with failover). Behaviour is pinned by testdata/vectors.json,
// recorded from the Python code (testdata/record.py) against a fake HTTP
// server; the Python fixes taken on the way are listed in the WP-LLM report.
//
// OpenAI-compatible providers are called over plain HTTP. Anthropic uses the
// native Messages API directly (the Python client used the official SDK; the
// request bodies and the SDK's retry rules are reproduced here).
package llm
