//! Native timing of the kernels without any WebAssembly runtime, for
//! comparison with the Go (wazero) benchmarks. Run with:
//!
//!     cargo run --release --example timing
//!
//! Prints ns per operation for the same inputs the Go benchmarks use.

use std::time::Instant;

use opengtm_kernels::api;
use serde_json::Value;

fn per_op(label: &str, iters: u32, mut f: impl FnMut()) {
    for _ in 0..iters.div_ceil(10) {
        f(); // warm up
    }
    let start = Instant::now();
    for _ in 0..iters {
        f();
    }
    let ns = start.elapsed().as_nanos() as f64 / iters as f64;
    println!("{label:<32} {ns:>12.0} ns/op");
}

fn main() {
    let dir = concat!(env!("CARGO_MANIFEST_DIR"), "/");
    let corpus: Value = serde_json::from_str(
        &std::fs::read_to_string(format!("{dir}testdata/bench_corpus.json")).unwrap(),
    )
    .unwrap();
    let single = br#"{"value":"https://www.Example.com/about?x=1"}"#;
    per_op("normalize_domain (json)", 200_000, || {
        std::hint::black_box(api::normalize_domain(single));
    });
    for kind in ["domain", "email", "phone", "person_name"] {
        let batch =
            serde_json::to_vec(&serde_json::json!({"kind": kind, "values": corpus[kind]})).unwrap();
        per_op(&format!("normalize_batch 1k {kind}"), 2_000, || {
            std::hint::black_box(api::normalize_batch(&batch));
        });
    }

    let cases: Value = serde_json::from_str(
        &std::fs::read_to_string(format!("{dir}testdata/extract_cases.json")).unwrap(),
    )
    .unwrap();
    let mut req = cases["cases"][0]["request"].clone();
    req["document"] =
        Value::String(std::fs::read_to_string(format!("{dir}testdata/team_page.html")).unwrap());
    let req = serde_json::to_vec(&req).unwrap();
    per_op("extract team page", 5_000, || {
        std::hint::black_box(api::extract(&req));
    });
}
