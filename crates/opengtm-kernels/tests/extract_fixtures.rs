//! Runs testdata/extract_cases.json through the JSON entry point.

use serde_json::Value;

fn testdata(name: &str) -> String {
    let path = format!("{}/testdata/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"))
}

#[test]
fn extract_cases() {
    let cases: Value = serde_json::from_str(&testdata("extract_cases.json")).unwrap();
    let cases = cases["cases"].as_array().unwrap();
    assert!(cases.len() >= 10);
    for case in cases {
        let name = case["name"].as_str().unwrap();
        let mut request = case["request"].clone();
        if let Some(file) = case["document_file"].as_str() {
            request["document"] = Value::String(testdata(file));
        }
        let out = opengtm_kernels::api::extract(&serde_json::to_vec(&request).unwrap());
        let got: Value = serde_json::from_slice(&out).unwrap();
        if let Some(expected) = case.get("expected") {
            assert_eq!(&got, expected, "case {name:?}");
        } else {
            let want = &case["expected_error"];
            assert_eq!(got["error"]["code"], want["code"], "case {name:?}: {got}");
            assert_eq!(
                got["error"].get("field"),
                want.get("field"),
                "case {name:?}: {got}"
            );
            assert!(
                got["error"]["message"]
                    .as_str()
                    .is_some_and(|m| !m.is_empty()),
                "case {name:?}"
            );
        }
    }
}
