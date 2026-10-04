//! Exact-equality parity with the Python normalizers (parity/fixtures.json).

use opengtm_kernels::normalize;
use serde::Deserialize;
use serde_json::Value;

#[derive(Deserialize)]
struct Case {
    input: String,
    #[serde(default)]
    default_region: Option<String>,
    expected: Value,
}

#[derive(Deserialize)]
struct Fixtures {
    domain: Vec<Case>,
    email: Vec<Case>,
    phone: Vec<Case>,
    person_name: Vec<Case>,
}

fn fixtures() -> Fixtures {
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/parity/fixtures.json");
    let raw = std::fs::read_to_string(path).expect("read parity/fixtures.json");
    serde_json::from_str(&raw).expect("parse parity/fixtures.json")
}

fn check(kind: &str, cases: &[Case], f: impl Fn(&Case) -> Value) {
    let failures: Vec<String> = cases
        .iter()
        .filter_map(|c| {
            let got = f(c);
            (got != c.expected)
                .then(|| format!("{kind}({:?}) = {got}, python = {}", c.input, c.expected))
        })
        .collect();
    assert!(cases.len() > 40, "{kind}: too few parity cases");
    assert!(
        failures.is_empty(),
        "{} of {} {kind} cases differ:\n{}",
        failures.len(),
        cases.len(),
        failures.join("\n")
    );
}

#[test]
fn domain_parity() {
    check("domain", &fixtures().domain, |c| {
        normalize::domain(&c.input).into()
    });
}

#[test]
fn email_parity() {
    check("email", &fixtures().email, |c| {
        normalize::email(&c.input).into()
    });
}

#[test]
fn phone_parity() {
    check("phone", &fixtures().phone, |c| {
        normalize::phone(&c.input, c.default_region.as_deref()).into()
    });
}

#[test]
fn person_name_parity() {
    check("person_name", &fixtures().person_name, |c| {
        serde_json::to_value(normalize::person_name(&c.input)).unwrap()
    });
}
