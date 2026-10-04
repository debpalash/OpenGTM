//! A faithful port of the parts of CPython 3.13 `urllib.parse.urlsplit` and
//! `SplitResult.hostname` that `services/dedup.py::normalize_domain` relies on,
//! including the validation that makes `urlsplit` raise `ValueError`.
//!
//! This is deliberately not a WHATWG URL parser: Python keeps spaces, percent
//! signs and non-ASCII hosts as-is, never applies IDNA, and splits the netloc
//! with simple string partitions. Matching that byte-for-byte is the point.

use std::borrow::Cow;

use crate::pystr;

/// `urlsplit` raised `ValueError`.
#[derive(Debug, PartialEq, Eq)]
pub struct UrlError;

const C0_CONTROL_OR_SPACE: fn(char) -> bool = |c| c <= ' ';

fn is_scheme_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '+' | '-' | '.')
}

/// Returns the netloc component of `urlsplit(url)`, or the error it raises.
pub fn urlsplit_netloc(url: &str) -> Result<Cow<'_, str>, UrlError> {
    let url = url.trim_start_matches(C0_CONTROL_OR_SPACE);
    if url.contains(['\t', '\r', '\n']) {
        let cleaned: String = url
            .chars()
            .filter(|c| !matches!(c, '\t' | '\r' | '\n'))
            .collect();
        return netloc_of(&cleaned).map(|n| Cow::Owned(n.to_string()));
    }
    netloc_of(url).map(Cow::Borrowed)
}

/// `urlsplit` after the unsafe-byte removal.
fn netloc_of(url: &str) -> Result<&str, UrlError> {
    let mut rest: &str = url;
    if let Some(i) = rest.find(':') {
        let first = rest.chars().next().unwrap_or('\0');
        if i > 0 && first.is_ascii_alphabetic() && rest[..i].chars().all(is_scheme_char) {
            rest = &rest[i + 1..];
        }
    }
    let mut netloc = "";
    if let Some(after) = rest.strip_prefix("//") {
        let end = after.find(['/', '?', '#']).unwrap_or(after.len());
        netloc = &after[..end];
        let open = netloc.contains('[');
        let close = netloc.contains(']');
        if open != close {
            return Err(UrlError);
        }
        if open && close {
            check_bracketed_netloc(netloc)?;
        }
    }
    check_netloc(netloc)?;
    Ok(netloc)
}

/// `_checknetloc`: reject hosts whose NFKC form introduces URL delimiters.
fn check_netloc(netloc: &str) -> Result<(), UrlError> {
    if netloc.is_ascii() {
        return Ok(());
    }
    if netloc
        .chars()
        .any(|c| pystr::NFKC_URL_DELIMITERS.contains(&c))
    {
        return Err(UrlError);
    }
    Ok(())
}

fn check_bracketed_netloc(netloc: &str) -> Result<(), UrlError> {
    let host_and_port = rpartition_after(netloc, '@');
    let hostname = match host_and_port.split_once('[') {
        Some((before, bracketed)) => {
            if !before.is_empty() {
                return Err(UrlError);
            }
            let (hostname, port) = bracketed.split_once(']').unwrap_or((bracketed, ""));
            if !port.is_empty() && !port.starts_with(':') {
                return Err(UrlError);
            }
            hostname
        }
        None => host_and_port
            .split_once(':')
            .map_or(host_and_port, |(h, _)| h),
    };
    check_bracketed_host(hostname)
}

fn check_bracketed_host(hostname: &str) -> Result<(), UrlError> {
    if let Some(rest) = hostname.strip_prefix('v') {
        // re.match(r"\Av[a-fA-F0-9]+\..+\Z", hostname); newlines were removed.
        let hex_len = rest.bytes().take_while(u8::is_ascii_hexdigit).count();
        let ok = hex_len > 0
            && rest[hex_len..]
                .strip_prefix('.')
                .is_some_and(|tail| !tail.is_empty());
        return if ok { Ok(()) } else { Err(UrlError) };
    }
    match ip_address(hostname) {
        Some(IpKind::V6) => Ok(()),
        _ => Err(UrlError),
    }
}

/// The text after the last `sep`, or the whole string (`str.rpartition(sep)[2]`).
fn rpartition_after(s: &str, sep: char) -> &str {
    s.rfind(sep).map_or(s, |i| &s[i + sep.len_utf8()..])
}

/// `SplitResult.hostname` for a netloc: `None` when empty.
pub fn hostname(netloc: &str) -> Option<Cow<'_, str>> {
    let hostinfo = rpartition_after(netloc, '@');
    let host = match hostinfo.split_once('[') {
        Some((_, bracketed)) => bracketed.split_once(']').map_or(bracketed, |(h, _)| h),
        None => hostinfo.split_once(':').map_or(hostinfo, |(h, _)| h),
    };
    if host.is_empty() {
        return None;
    }
    Some(match host.split_once('%') {
        Some((h, zone)) => Cow::Owned(format!("{}%{}", pystr::lower(h), zone)),
        // Fast path: lowercase ASCII is already its own lower().
        None if host.is_ascii() && !host.bytes().any(|b| b.is_ascii_uppercase()) => {
            Cow::Borrowed(host)
        }
        None => Cow::Owned(pystr::lower(host)),
    })
}

#[derive(Debug, PartialEq, Eq)]
enum IpKind {
    V4,
    V6,
}

/// `ipaddress.ip_address(s)` reduced to which constructor accepted it.
fn ip_address(s: &str) -> Option<IpKind> {
    if parse_ipv4(s) {
        Some(IpKind::V4)
    } else if parse_ipv6(s) {
        Some(IpKind::V6)
    } else {
        None
    }
}

fn parse_ipv4(s: &str) -> bool {
    if s.is_empty() || s.contains('/') {
        return false;
    }
    let octets: Vec<&str> = s.split('.').collect();
    octets.len() == 4 && octets.iter().all(|o| parse_octet(o))
}

fn parse_octet(o: &str) -> bool {
    if o.is_empty() || !o.bytes().all(|b| b.is_ascii_digit()) || o.len() > 3 {
        return false;
    }
    if o != "0" && o.starts_with('0') {
        return false;
    }
    o.parse::<u16>().is_ok_and(|v| v <= 255)
}

fn parse_hextet(h: &str) -> bool {
    // `int('', 16)` raises, so an empty hextet is invalid.
    !h.is_empty() && h.len() <= 4 && h.bytes().all(|b| b.is_ascii_hexdigit())
}

fn parse_ipv6(s: &str) -> bool {
    if s.contains('/') {
        return false;
    }
    // _split_scope_id
    let addr = match s.split_once('%') {
        Some((addr, scope)) => {
            if scope.is_empty() || scope.contains('%') {
                return false;
            }
            addr
        }
        None => s,
    };
    if addr.is_empty() || addr.chars().count() > 45 {
        return false;
    }
    const HEXTETS: usize = 8;
    const MAX_PARTS: usize = HEXTETS + 1;
    let mut parts: Vec<String> = addr
        .splitn(MAX_PARTS + 1, ':')
        .map(str::to_string)
        .collect();
    if parts.len() < 3 {
        return false;
    }
    if parts.last().is_some_and(|p| p.contains('.')) {
        let v4 = parts.pop().unwrap_or_default();
        if !parse_ipv4(&v4) {
            return false;
        }
        // Two placeholder hextets; their values do not matter for validity.
        parts.push("0".into());
        parts.push("0".into());
    }
    if parts.len() > MAX_PARTS {
        return false;
    }
    let n = parts.len();
    let mut skip_index = None;
    for (i, part) in parts.iter().enumerate().take(n - 1).skip(1) {
        if part.is_empty() {
            if skip_index.is_some() {
                return false;
            }
            skip_index = Some(i);
        }
    }
    let (parts_hi, parts_lo) = if let Some(skip) = skip_index {
        let mut hi = skip;
        let mut lo = n - skip - 1;
        if parts[0].is_empty() {
            hi -= 1;
            if hi > 0 {
                return false;
            }
        }
        if parts[n - 1].is_empty() {
            lo -= 1;
            if lo > 0 {
                return false;
            }
        }
        if HEXTETS as isize - (hi + lo) as isize <= 0 {
            return false;
        }
        (hi, lo)
    } else {
        if n != HEXTETS || parts[0].is_empty() || parts[n - 1].is_empty() {
            return false;
        }
        (n, 0)
    };
    parts[..parts_hi].iter().all(|p| parse_hextet(p))
        && parts[n - parts_lo..].iter().all(|p| parse_hextet(p))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn netloc_and_hostname() {
        let n = urlsplit_netloc("http://user:pw@Example.com:8080/path").unwrap();
        assert_eq!(n, "user:pw@Example.com:8080");
        assert_eq!(hostname(&n).as_deref(), Some("example.com"));
        assert_eq!(hostname(""), None);
        assert_eq!(hostname(":80"), None);
    }

    #[test]
    fn bracket_validation() {
        assert!(urlsplit_netloc("http://[::1]:80/").is_ok());
        assert!(urlsplit_netloc("http://[::1/").is_err());
        assert!(urlsplit_netloc("http://[1.2.3.4]/").is_err());
        assert!(urlsplit_netloc("http://[v1.x]/").is_ok());
        assert!(urlsplit_netloc("http://[fe80::1%eth0]/").is_ok());
        assert!(urlsplit_netloc("http://a[::1]/").is_err());
        assert!(urlsplit_netloc("http://[::1]x/").is_err());
    }

    #[test]
    fn ipv6_rules() {
        assert!(parse_ipv6("::"));
        assert!(parse_ipv6("1:2:3:4:5:6:7:8"));
        assert!(parse_ipv6("::ffff:1.2.3.4"));
        assert!(!parse_ipv6("1:2:3:4:5:6:7:8:9"));
        assert!(!parse_ipv6("1::2::3"));
        assert!(!parse_ipv6(":1:2:3:4:5:6:7"));
        assert!(!parse_ipv6("1:2:3:4:5:6:7::8"));
        assert!(!parse_ipv6("12345::"));
    }

    #[test]
    fn nfkc_delimiters_rejected() {
        assert!(urlsplit_netloc("http://a\u{2100}b.com/").is_err());
        assert!(urlsplit_netloc("http://bu\u{0308}cher.de/").is_ok());
    }
}
