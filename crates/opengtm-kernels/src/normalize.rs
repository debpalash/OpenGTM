//! Normalizers with byte-for-byte parity to the Python functions the API uses
//! for entity matching today. See README.md for the chosen Python reference of
//! each normalizer and the variants that intentionally differ.
//!
//! Python returns `""` for "no value"; these functions return `None` instead.

use crate::{pystr, pyurl};
use serde::Serialize;

/// Port of `apps/api/services/dedup.py::normalize_domain`, the domain key used
/// by the company entity graph (`services/entities/graph.py`).
pub fn domain(value: &str) -> Option<String> {
    if value.is_empty() {
        return None;
    }
    let lowered = pystr::lower(value);
    let stripped = pystr::strip(&lowered);
    let url = if stripped.starts_with("http://") || stripped.starts_with("https://") {
        stripped.to_string()
    } else {
        format!("http://{stripped}")
    };
    let out = match pyurl::urlsplit_netloc(&url) {
        Ok(netloc) => {
            let host = pyurl::hostname(&netloc).unwrap_or_default();
            match host.strip_prefix("www.") {
                Some(rest) => rest.to_string(),
                None => host,
            }
        }
        // The `except Exception` branch: url.lower().replace("www.", "").strip("/")
        Err(pyurl::UrlError) => pystr::lower(&url)
            .replace("www.", "")
            .trim_matches('/')
            .to_string(),
    };
    non_empty(out)
}

/// Port of `apps/api/services/entities/people.py::_email_key`, the email
/// identifier used to resolve person entities.
pub fn email(value: &str) -> Option<String> {
    let v = pystr::lower(pystr::strip(value));
    let domain = v.rsplit('@').next().unwrap_or("");
    if v.contains('@') && domain.contains('.') {
        Some(v)
    } else {
        None
    }
}

/// Port of `apps/api/services/dedup.py::normalize_phone`: Unicode decimal
/// digits only, last 10 kept. Used for company entity blocking keys.
///
/// `default_region` is accepted for API stability but has no effect: the
/// Python reference has no region handling and `phonenumbers` is not a
/// project dependency.
pub fn phone(value: &str, _default_region: Option<&str>) -> Option<String> {
    if value.is_empty() {
        return None;
    }
    let digits: Vec<char> = value.chars().filter(|&c| pystr::is_decimal(c)).collect();
    let start = digits.len().saturating_sub(10);
    non_empty(digits[start..].iter().collect())
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct PersonName {
    pub full: String,
    pub first: String,
    pub last: String,
}

/// Port of how `apps/api/services/workbook/people_search.py::_row_data` derives
/// `full_name`/`first_name`/`last_name` (via `_split_name`), which is also the
/// stored `PersonEntity.full_name` (`name.strip()`).
///
/// No honorific, suffix or particle handling: "Dr. Jane van der Berg Jr."
/// yields first "Dr." and last "Jr.", exactly as Python does today.
pub fn person_name(value: &str) -> PersonName {
    let full = pystr::strip(value);
    let mut parts = pystr::split_whitespace(full);
    let first = parts.next().unwrap_or("");
    let last = parts.last().unwrap_or("");
    PersonName {
        full: full.to_string(),
        first: first.to_string(),
        last: last.to_string(),
    }
}

fn non_empty(s: String) -> Option<String> {
    if s.is_empty() { None } else { Some(s) }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn domain_examples() {
        assert_eq!(
            domain("https://www.Example.com/about").as_deref(),
            Some("example.com")
        );
        assert_eq!(domain("example.com:8080").as_deref(), Some("example.com"));
        assert_eq!(domain("").as_deref(), None);
        assert_eq!(domain("   ").as_deref(), None);
        assert_eq!(domain("[::1").as_deref(), Some("http://[::1"));
        assert_eq!(domain("www.www.x.com").as_deref(), Some("www.x.com"));
    }

    #[test]
    fn email_examples() {
        assert_eq!(
            email(" Jane.Doe+x@Example.COM ").as_deref(),
            Some("jane.doe+x@example.com")
        );
        assert_eq!(email("jane@localhost"), None);
        assert_eq!(email("no-at.example.com"), None);
        assert_eq!(email("a@b@c.d").as_deref(), Some("a@b@c.d"));
    }

    #[test]
    fn phone_examples() {
        assert_eq!(
            phone("+1 (415) 555-0132", None).as_deref(),
            Some("4155550132")
        );
        assert_eq!(
            phone("+91 98765 43210", Some("IN")).as_deref(),
            Some("9876543210")
        );
        assert_eq!(phone("abc", None), None);
        assert_eq!(phone("123", None).as_deref(), Some("123"));
    }

    #[test]
    fn person_name_examples() {
        let n = person_name("  Dr. Jane  van der Berg Jr. ");
        assert_eq!(n.full, "Dr. Jane  van der Berg Jr.");
        assert_eq!(n.first, "Dr.");
        assert_eq!(n.last, "Jr.");
        let n = person_name("Cher");
        assert_eq!((n.first.as_str(), n.last.as_str()), ("Cher", ""));
        assert_eq!(
            person_name(""),
            PersonName {
                full: String::new(),
                first: String::new(),
                last: String::new()
            }
        );
    }
}
