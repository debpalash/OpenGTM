//! Example OpenGTM WebAssembly provider plugin.
//!
//! `enrich` fetches `inputs.url` through the host (`opengtm_fetch`), logs
//! through `opengtm_log`, and echoes the JSON response body back as fields.
//! It also reports which secrets it can see (names only, never values), so the
//! host's secret isolation can be tested. The `spin`, `grow` and `flood`
//! exports exist only to exercise host limits (timeout, memory, output size).
use extism_pdk::*;
use serde_json::{json, Map, Value};

#[host_fn]
extern "ExtismHost" {
    fn opengtm_fetch(request: Json<Value>) -> Json<Value>;
    fn opengtm_log(level: String, message: String);
}

fn log(level: &str, message: &str) {
    // Logging is best effort; a failing host log must not fail the call.
    let _ = unsafe { opengtm_log(level.to_string(), message.to_string()) };
}

#[plugin_fn]
pub fn enrich(Json(input): Json<Value>) -> FnResult<Json<Value>> {
    let url = input
        .get("inputs")
        .and_then(|i| i.get("url"))
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    log("info", &format!("fetching {url}"));

    let Json(resp) = unsafe {
        opengtm_fetch(Json(json!({
            "method": "GET",
            "url": url,
            "headers": {"Accept": "application/json"},
        })))?
    };

    let mut fields = Map::new();
    if let Some(err) = resp.get("error") {
        log("warn", "fetch denied or failed");
        fields.insert("fetch_error".into(), err.clone());
    } else {
        fields.insert("status".into(), resp.get("status").cloned().unwrap_or(Value::Null));
        let body = resp.get("body").and_then(Value::as_str).unwrap_or_default();
        if let Ok(Value::Object(obj)) = serde_json::from_str::<Value>(body) {
            for (k, v) in obj {
                fields.insert(k, v);
            }
        }
    }

    // Report secret visibility (names only) so tests can prove isolation.
    let mut seen = Vec::new();
    for name in ["ECHO_API_KEY", "UNDECLARED_SECRET"] {
        if config::get(name)?.is_some() {
            seen.push(Value::String(name.into()));
        }
    }
    fields.insert("secrets_visible".into(), Value::Array(seen));

    let evidence = match resp.get("evidence") {
        Some(e) => vec![e.clone()],
        None => vec![],
    };
    Ok(Json(json!({
        "fields": fields,
        "confidence": 0.9,
        "cost_usd": 0.0,
        "evidence": evidence,
    })))
}

/// Never returns; the host must stop it with its wall-clock timeout.
#[plugin_fn]
pub fn spin(_: String) -> FnResult<String> {
    let mut x: u64 = 0;
    loop {
        x = std::hint::black_box(x.wrapping_add(1));
        if x == u64::MAX {
            break;
        }
    }
    Ok(String::new())
}

/// Allocates and touches `n` MiB (input is a decimal string).
#[plugin_fn]
pub fn grow(n: String) -> FnResult<String> {
    let mib: usize = n.trim().parse().unwrap_or(64);
    let mut keep: Vec<Vec<u8>> = Vec::new();
    for _ in 0..mib {
        keep.push(std::hint::black_box(vec![1u8; 1 << 20]));
    }
    Ok(format!("{}", keep.len()))
}

/// Returns `n` bytes of output (input is a decimal string).
#[plugin_fn]
pub fn flood(n: String) -> FnResult<String> {
    let size: usize = n.trim().parse().unwrap_or(1 << 20);
    Ok("x".repeat(size))
}
