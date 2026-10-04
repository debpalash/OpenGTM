//! Python `str` semantics that the normalizers depend on.
//!
//! The parity target is CPython 3.13 (Unicode 15.1). Rust's `char` predicates
//! differ in small ways (for example, Python treats U+001C..U+001F as
//! whitespace and Rust does not), so the tables here are copied from Python
//! rather than derived from `std`.

/// `str.isspace()` in CPython 3.13: bidi class WS/B/S or category Zs.
pub fn is_space(c: char) -> bool {
    matches!(
        c,
        '\u{09}'..='\u{0D}'
            | '\u{1C}'..='\u{20}'
            | '\u{85}'
            | '\u{A0}'
            | '\u{1680}'
            | '\u{2000}'..='\u{200A}'
            | '\u{2028}'
            | '\u{2029}'
            | '\u{202F}'
            | '\u{205F}'
            | '\u{3000}'
    )
}

/// `str.strip()` with no arguments.
pub fn strip(s: &str) -> &str {
    s.trim_matches(is_space)
}

/// `str.split()` with no arguments: runs of whitespace separate fields and
/// leading/trailing whitespace produces no empty fields.
pub fn split_whitespace(s: &str) -> impl Iterator<Item = &str> {
    s.split(is_space).filter(|p| !p.is_empty())
}

/// `str.lower()`. Rust and CPython both apply full case mapping including the
/// final-sigma rule. Uppercase letters added after Unicode 15.1 (CPython 3.13)
/// are unassigned to Python, so they are kept as-is here too.
pub fn lower(s: &str) -> String {
    if s.is_ascii() {
        return s.to_ascii_lowercase();
    }
    if !s.chars().any(is_post_15_1_upper) {
        return s.to_lowercase();
    }
    // Lowercase the runs between such letters separately. To Python they are
    // neither cased nor case-ignorable, so they also end any final-sigma
    // context, exactly like a run boundary does.
    let mut out = String::with_capacity(s.len());
    let mut run_start = 0;
    for (i, c) in s.char_indices() {
        if is_post_15_1_upper(c) {
            out.push_str(&s[run_start..i].to_lowercase());
            out.push(c);
            run_start = i + c.len_utf8();
        }
    }
    out.push_str(&s[run_start..].to_lowercase());
    out
}

/// Code points that Rust (Unicode 17) lowercases but CPython 3.13 (Unicode
/// 15.1) leaves unchanged. Found by comparing `char::to_lowercase` with
/// `str.lower()` for every code point.
fn is_post_15_1_upper(c: char) -> bool {
    matches!(
        c as u32,
        0x1C89
            | 0xA7CB
            | 0xA7CC
            | 0xA7CE
            | 0xA7D2
            | 0xA7D4
            | 0xA7DA
            | 0xA7DC
            | 0x10D50..=0x10D65
            | 0x16EA0..=0x16EB8
    )
}

/// Unicode decimal digits (category Nd) as matched by Python's `re` `\d` and
/// `str.isdecimal()`, Unicode 15.1. Generated from CPython 3.13.
const ND_RANGES: &[(u32, u32)] = &[
    (0x30, 0x39),
    (0x660, 0x669),
    (0x6F0, 0x6F9),
    (0x7C0, 0x7C9),
    (0x966, 0x96F),
    (0x9E6, 0x9EF),
    (0xA66, 0xA6F),
    (0xAE6, 0xAEF),
    (0xB66, 0xB6F),
    (0xBE6, 0xBEF),
    (0xC66, 0xC6F),
    (0xCE6, 0xCEF),
    (0xD66, 0xD6F),
    (0xDE6, 0xDEF),
    (0xE50, 0xE59),
    (0xED0, 0xED9),
    (0xF20, 0xF29),
    (0x1040, 0x1049),
    (0x1090, 0x1099),
    (0x17E0, 0x17E9),
    (0x1810, 0x1819),
    (0x1946, 0x194F),
    (0x19D0, 0x19D9),
    (0x1A80, 0x1A89),
    (0x1A90, 0x1A99),
    (0x1B50, 0x1B59),
    (0x1BB0, 0x1BB9),
    (0x1C40, 0x1C49),
    (0x1C50, 0x1C59),
    (0xA620, 0xA629),
    (0xA8D0, 0xA8D9),
    (0xA900, 0xA909),
    (0xA9D0, 0xA9D9),
    (0xA9F0, 0xA9F9),
    (0xAA50, 0xAA59),
    (0xABF0, 0xABF9),
    (0xFF10, 0xFF19),
    (0x104A0, 0x104A9),
    (0x10D30, 0x10D39),
    (0x11066, 0x1106F),
    (0x110F0, 0x110F9),
    (0x11136, 0x1113F),
    (0x111D0, 0x111D9),
    (0x112F0, 0x112F9),
    (0x11450, 0x11459),
    (0x114D0, 0x114D9),
    (0x11650, 0x11659),
    (0x116C0, 0x116C9),
    (0x11730, 0x11739),
    (0x118E0, 0x118E9),
    (0x11950, 0x11959),
    (0x11C50, 0x11C59),
    (0x11D50, 0x11D59),
    (0x11DA0, 0x11DA9),
    (0x11F50, 0x11F59),
    (0x16A60, 0x16A69),
    (0x16AC0, 0x16AC9),
    (0x16B50, 0x16B59),
    (0x1D7CE, 0x1D7FF),
    (0x1E140, 0x1E149),
    (0x1E2F0, 0x1E2F9),
    (0x1E4F0, 0x1E4F9),
    (0x1E950, 0x1E959),
    (0x1FBF0, 0x1FBF9),
];

/// Python `re` `\d` for `str` patterns.
pub fn is_decimal(c: char) -> bool {
    if c.is_ascii() {
        return c.is_ascii_digit();
    }
    let cp = c as u32;
    ND_RANGES
        .binary_search_by(|&(lo, hi)| {
            if hi < cp {
                std::cmp::Ordering::Less
            } else if lo > cp {
                std::cmp::Ordering::Greater
            } else {
                std::cmp::Ordering::Equal
            }
        })
        .is_ok()
}

/// Non-ASCII code points whose NFKC form contains one of `/?#@:` (CPython 3.13,
/// Unicode 15.1). `urllib.parse` rejects a netloc containing any of them.
pub const NFKC_URL_DELIMITERS: &[char] = &[
    '\u{2047}', '\u{2048}', '\u{2049}', '\u{2100}', '\u{2101}', '\u{2105}', '\u{2106}', '\u{2A74}',
    '\u{FE13}', '\u{FE16}', '\u{FE55}', '\u{FE56}', '\u{FE5F}', '\u{FE6B}', '\u{FF03}', '\u{FF0F}',
    '\u{FF1A}', '\u{FF1F}', '\u{FF20}',
];

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn whitespace_matches_python() {
        assert!(is_space('\u{1C}'));
        assert!(is_space('\u{A0}'));
        assert!(!is_space('\u{200B}'));
        assert_eq!(strip("\u{1F} a b \u{3000}"), "a b");
        let parts: Vec<_> = split_whitespace("  a \t b\u{2003}c ").collect();
        assert_eq!(parts, ["a", "b", "c"]);
    }

    #[test]
    fn lower_matches_unicode_15_1() {
        assert_eq!(lower("ABC"), "abc");
        assert_eq!(lower("İ"), "i\u{307}");
        assert_eq!(lower("ΟΔΟΣ"), "\u{3BF}\u{3B4}\u{3BF}\u{3C2}");
        assert_eq!(lower("A\u{A7CB}B"), "a\u{A7CB}b");
        // U+A7CB ends the final-sigma context like an unassigned letter does.
        assert_eq!(lower("ΑΣ\u{A7CB}"), "ας\u{A7CB}");
        assert_eq!(lower("\u{A7CB}ΣΑ"), "\u{A7CB}σα");
    }

    #[test]
    fn decimal_digits() {
        assert!(is_decimal('7'));
        assert!(is_decimal('\u{0663}'));
        assert!(is_decimal('\u{FF15}'));
        assert!(!is_decimal('\u{00B2}'));
        assert!(!is_decimal('a'));
    }
}
