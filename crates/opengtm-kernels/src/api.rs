//! JSON-in/JSON-out entry points shared by the Extism exports and native tests.
//!
//! Every function returns JSON bytes and never panics on bad input. Failures
//! use one envelope for all exports:
//!
//! ```json
//! {"error": {"code": "invalid_selector", "message": "...", "field": "name"}}
//! ```
//!
//! `field` is present only when the error belongs to one extract field. Codes:
//! `invalid_request`, `request_too_large`, `invalid_selector`,
//! `invalid_document`, `document_too_large`, `output_too_large`.

use std::borrow::Cow;

use serde::{Deserialize, Serialize};

use crate::extract::{self, ExtractRequest, KernelError, MAX_DOCUMENT_BYTES, null_default};
use crate::normalize::{self, PersonName};

/// Upper bound on any request body. JSON escaping can expand a 10 MiB
/// document, so this is larger than [`MAX_DOCUMENT_BYTES`].
pub const MAX_REQUEST_BYTES: usize = 2 * MAX_DOCUMENT_BYTES + (1 << 20);
/// Upper bound on values in one batch call.
pub const MAX_BATCH_VALUES: usize = 100_000;

#[derive(Serialize)]
struct ErrorEnvelope<'a> {
    error: &'a KernelError,
}

fn encode<T: Serialize>(result: Result<T, KernelError>) -> Vec<u8> {
    let encoded = match &result {
        Ok(v) => serde_json::to_vec(v),
        Err(e) => serde_json::to_vec(&ErrorEnvelope { error: e }),
    };
    // Serializing these plain structs cannot fail; keep a valid envelope anyway.
    encoded.unwrap_or_else(|_| {
        br#"{"error":{"code":"internal","message":"encoding failed"}}"#.to_vec()
    })
}

fn decode<'a, T: Deserialize<'a>>(input: &'a [u8]) -> Result<T, KernelError> {
    if input.len() > MAX_REQUEST_BYTES {
        return Err(KernelError::new(
            "request_too_large",
            format!(
                "request is {} bytes; the limit is {MAX_REQUEST_BYTES}",
                input.len()
            ),
        ));
    }
    serde_json::from_slice(input)
        .map_err(|e| KernelError::new("invalid_request", format!("invalid request JSON: {e}")))
}

// Strings borrow from the request unless they contain JSON escapes.
#[derive(Deserialize)]
struct ValueRequest<'a> {
    #[serde(default, borrow, deserialize_with = "null_default")]
    value: Cow<'a, str>,
    #[serde(default)]
    default_region: Option<String>,
}

#[derive(Serialize)]
struct ValueResponse {
    value: Option<String>,
}

pub fn normalize_domain(input: &[u8]) -> Vec<u8> {
    encode(decode::<ValueRequest>(input).map(|r| ValueResponse {
        value: normalize::domain(&r.value),
    }))
}

pub fn normalize_email(input: &[u8]) -> Vec<u8> {
    encode(decode::<ValueRequest>(input).map(|r| ValueResponse {
        value: normalize::email(&r.value),
    }))
}

pub fn normalize_phone(input: &[u8]) -> Vec<u8> {
    encode(decode::<ValueRequest>(input).map(|r| ValueResponse {
        value: normalize::phone(&r.value, r.default_region.as_deref()),
    }))
}

pub fn normalize_person_name(input: &[u8]) -> Vec<u8> {
    match decode::<ValueRequest>(input) {
        Ok(r) => encode(Ok(normalize::person_name(&r.value))),
        Err(e) => encode::<()>(Err(e)),
    }
}

#[derive(Deserialize)]
struct BatchRequest<'a> {
    kind: String,
    #[serde(default, borrow, deserialize_with = "null_default")]
    values: Vec<Cow<'a, str>>,
    #[serde(default)]
    default_region: Option<String>,
}

#[derive(Serialize)]
#[serde(untagged)]
enum BatchValues<'a> {
    Strings(Vec<Option<String>>),
    Names(Vec<PersonName<'a>>),
}

#[derive(Serialize)]
struct BatchResponse<'a> {
    values: BatchValues<'a>,
}

/// `{"kind": "domain|email|phone|person_name", "values": [...]}` →
/// `{"values": [...]}` in input order. `person_name` returns objects.
pub fn normalize_batch(input: &[u8]) -> Vec<u8> {
    let r = match decode::<BatchRequest>(input) {
        Ok(r) => r,
        Err(e) => return encode::<()>(Err(e)),
    };
    encode(batch_values(&r).map(|values| BatchResponse { values }))
}

fn batch_values<'a>(r: &'a BatchRequest<'_>) -> Result<BatchValues<'a>, KernelError> {
    if r.values.len() > MAX_BATCH_VALUES {
        return Err(KernelError::new(
            "request_too_large",
            format!("at most {MAX_BATCH_VALUES} values per batch"),
        ));
    }
    let region = r.default_region.as_deref();
    let strings = |f: &dyn Fn(&str) -> Option<String>| {
        BatchValues::Strings(r.values.iter().map(|v| f(v)).collect())
    };
    Ok(match r.kind.as_str() {
        "domain" => strings(&normalize::domain),
        "email" => strings(&normalize::email),
        "phone" => strings(&|v| normalize::phone(v, region)),
        "person_name" => {
            BatchValues::Names(r.values.iter().map(|v| normalize::person_name(v)).collect())
        }
        other => {
            return Err(KernelError::new(
                "invalid_request",
                format!("unknown kind {other:?}; use domain, email, phone or person_name"),
            ));
        }
    })
}

pub fn extract(input: &[u8]) -> Vec<u8> {
    encode(decode::<ExtractRequest>(input).and_then(|r| extract::extract(&r)))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn call(f: fn(&[u8]) -> Vec<u8>, input: &str) -> serde_json::Value {
        serde_json::from_slice(&f(input.as_bytes())).unwrap()
    }

    #[test]
    fn envelopes() {
        assert_eq!(
            call(normalize_domain, r#"{"value":"WWW.A.com"}"#),
            serde_json::json!({"value":"a.com"})
        );
        assert_eq!(
            call(normalize_email, r#"{"value":"x"}"#),
            serde_json::json!({"value":null})
        );
        assert_eq!(
            call(normalize_domain, "nope")["error"]["code"],
            "invalid_request"
        );
        assert_eq!(
            call(normalize_domain, r#"{"value":null}"#),
            serde_json::json!({"value":null})
        );
        // Python can hold lone surrogates; Rust strings cannot.
        assert_eq!(
            call(normalize_email, r#"{"value":"a\ud800@b.c"}"#)["error"]["code"],
            "invalid_request"
        );
        assert_eq!(
            call(
                extract,
                r#"{"document":"<p>x</p>","fields":null,"items":null}"#
            ),
            serde_json::json!({"records":[{}]})
        );
        assert_eq!(
            call(normalize_person_name, r#"{"value":"Ada King Lovelace"}"#),
            serde_json::json!({"full":"Ada King Lovelace","first":"Ada","last":"Lovelace"})
        );
        assert_eq!(
            call(
                normalize_batch,
                r#"{"kind":"phone","values":["+1 415 555 0100","x"]}"#
            ),
            serde_json::json!({"values":["4155550100",null]})
        );
        assert_eq!(
            call(normalize_batch, r#"{"kind":"zip","values":[]}"#)["error"]["code"],
            "invalid_request"
        );
        let e = call(extract, r#"{"document":"<p>","fields":{"a":"css:p["}}"#);
        assert_eq!(e["error"]["code"], "invalid_selector");
        assert_eq!(e["error"]["field"], "a");
        assert_eq!(
            call(
                extract,
                r#"{"document":"<p>hi</p>","fields":{"a":"css:p"}}"#
            ),
            serde_json::json!({"records":[{"a":"hi"}]})
        );
    }
}
