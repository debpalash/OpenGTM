//! Declarative extraction: select records and fields from one HTML or JSON
//! document. The contract mirrors `apps/server/internal/kernels/types.go`.
//!
//! Selector specs:
//!
//! * `css:<selector>` / `css:<selector>::text` — whitespace-collapsed text
//! * `css:<selector>::attr(<name>)` — attribute value (href/src resolved)
//! * `css:<selector>::html` — inner HTML
//! * `json:<JSONPath>` — value at an RFC 9535 JSONPath (JSON documents)
//! * `re:<regex>` — first participating capture group, else the whole match,
//!   searched in the record's text
//!
//! An empty CSS selector (`css:::attr(href)`) addresses the record element
//! itself. A field takes the first non-empty value among its matches and is
//! omitted when there is none.

use std::collections::BTreeMap;

use regex::{Regex, RegexBuilder};
use scraper::{ElementRef, Html, Node, Selector};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use serde_json_path::JsonPath;
use url::Url;

/// Largest accepted document, in bytes.
pub const MAX_DOCUMENT_BYTES: usize = 10 << 20;
/// Default and maximum number of records.
pub const DEFAULT_LIMIT: usize = 1000;
pub const MAX_LIMIT: usize = 10_000;
/// Maximum number of fields and the maximum length of one selector.
pub const MAX_FIELDS: usize = 256;
pub const MAX_SELECTOR_BYTES: usize = 4096;
/// Upper bound on the total size of extracted values.
pub const MAX_OUTPUT_BYTES: usize = 16 << 20;
/// Compiled-program size limit for `re:` selectors.
const REGEX_SIZE_LIMIT: usize = 1 << 20;

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct ExtractRequest {
    pub document: String,
    pub format: String,
    pub base_url: String,
    pub items: String,
    pub fields: BTreeMap<String, String>,
    pub limit: i64,
}

#[derive(Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ExtractResult {
    pub records: Vec<BTreeMap<String, String>>,
}

/// A structured, non-panicking failure. Serialized as
/// `{"error": {"code": ..., "message": ..., "field": ...}}` by [`crate::api`].
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct KernelError {
    pub code: &'static str,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub field: Option<String>,
}

impl KernelError {
    pub fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            field: None,
        }
    }

    fn selector(field: Option<&str>, message: impl Into<String>) -> Self {
        Self {
            code: "invalid_selector",
            message: message.into(),
            field: field.map(str::to_string),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Format {
    Html,
    Json,
}

enum CssMode {
    Text,
    Html,
    Attr(String),
}

enum FieldSpec {
    /// `None` selects the record element itself.
    Css(Option<Selector>, CssMode),
    Json(JsonPath),
    Re(Regex),
}

struct Field {
    name: String,
    spec: FieldSpec,
}

pub fn extract(req: &ExtractRequest) -> Result<ExtractResult, KernelError> {
    if req.document.len() > MAX_DOCUMENT_BYTES {
        return Err(KernelError::new(
            "document_too_large",
            format!(
                "document is {} bytes; the limit is {MAX_DOCUMENT_BYTES}",
                req.document.len()
            ),
        ));
    }
    let format = match req.format.as_str() {
        "" | "html" => Format::Html,
        "json" => Format::Json,
        other => {
            return Err(KernelError::new(
                "invalid_request",
                format!("unknown format {other:?}; use \"html\" or \"json\""),
            ));
        }
    };
    let limit = match req.limit {
        0 => DEFAULT_LIMIT,
        n if n < 0 || n as u64 > MAX_LIMIT as u64 => {
            return Err(KernelError::new(
                "invalid_request",
                format!("limit must be between 0 and {MAX_LIMIT}"),
            ));
        }
        n => n as usize,
    };
    if req.fields.len() > MAX_FIELDS {
        return Err(KernelError::new(
            "invalid_request",
            format!("at most {MAX_FIELDS} fields are allowed"),
        ));
    }
    let fields = req
        .fields
        .iter()
        .map(|(name, spec)| {
            Ok(Field {
                name: name.clone(),
                spec: parse_field(name, spec, format)?,
            })
        })
        .collect::<Result<Vec<_>, KernelError>>()?;

    let mut out = Output::default();
    match format {
        Format::Html => extract_html(req, &fields, limit, &mut out)?,
        Format::Json => extract_json(req, &fields, limit, &mut out)?,
    }
    Ok(ExtractResult {
        records: out.records,
    })
}

#[derive(Default)]
struct Output {
    records: Vec<BTreeMap<String, String>>,
    bytes: usize,
}

impl Output {
    fn push(&mut self, record: BTreeMap<String, String>) -> Result<(), KernelError> {
        self.bytes += record.iter().map(|(k, v)| k.len() + v.len()).sum::<usize>();
        if self.bytes > MAX_OUTPUT_BYTES {
            return Err(KernelError::new(
                "output_too_large",
                format!(
                    "extracted values exceed {MAX_OUTPUT_BYTES} bytes; narrow the selectors or lower the limit"
                ),
            ));
        }
        self.records.push(record);
        Ok(())
    }
}

fn check_len(field: Option<&str>, spec: &str) -> Result<(), KernelError> {
    if spec.len() > MAX_SELECTOR_BYTES {
        return Err(KernelError::selector(
            field,
            format!("selector is longer than {MAX_SELECTOR_BYTES} bytes"),
        ));
    }
    Ok(())
}

fn parse_field(name: &str, spec: &str, format: Format) -> Result<FieldSpec, KernelError> {
    let field = Some(name);
    check_len(field, spec)?;
    if let Some(css) = spec.strip_prefix("css:") {
        if format != Format::Html {
            return Err(KernelError::selector(
                field,
                "css: selectors require format \"html\"",
            ));
        }
        let (selector, mode) = split_css_mode(css).map_err(|m| KernelError::selector(field, m))?;
        let selector = parse_css(field, selector)?;
        Ok(FieldSpec::Css(selector, mode))
    } else if let Some(path) = spec.strip_prefix("json:") {
        if format != Format::Json {
            return Err(KernelError::selector(
                field,
                "json: selectors require format \"json\"",
            ));
        }
        Ok(FieldSpec::Json(parse_json_path(field, path)?))
    } else if let Some(pattern) = spec.strip_prefix("re:") {
        let re = RegexBuilder::new(pattern)
            .size_limit(REGEX_SIZE_LIMIT)
            .dfa_size_limit(2 * REGEX_SIZE_LIMIT)
            .build()
            .map_err(|e| KernelError::selector(field, format!("invalid regex: {e}")))?;
        Ok(FieldSpec::Re(re))
    } else {
        Err(KernelError::selector(
            field,
            format!("selector {spec:?} must start with css:, json: or re:"),
        ))
    }
}

/// Splits a trailing `::text`, `::html` or `::attr(name)` off a CSS spec.
fn split_css_mode(spec: &str) -> Result<(&str, CssMode), String> {
    if let Some(idx) = spec.rfind("::") {
        let (sel, suffix) = (&spec[..idx], spec[idx + 2..].trim());
        match suffix {
            "text" => return Ok((sel, CssMode::Text)),
            "html" => return Ok((sel, CssMode::Html)),
            _ => {
                if let Some(inner) = suffix
                    .strip_prefix("attr(")
                    .and_then(|s| s.strip_suffix(')'))
                {
                    let name = inner
                        .trim()
                        .trim_matches(['"', '\''])
                        .trim()
                        .to_ascii_lowercase();
                    if name.is_empty() {
                        return Err("::attr() needs an attribute name".into());
                    }
                    return Ok((sel, CssMode::Attr(name)));
                }
            }
        }
    }
    Ok((spec, CssMode::Text))
}

fn parse_css(field: Option<&str>, selector: &str) -> Result<Option<Selector>, KernelError> {
    let selector = selector.trim();
    if selector.is_empty() {
        return Ok(None);
    }
    Selector::parse(selector).map(Some).map_err(|e| {
        KernelError::selector(field, format!("invalid CSS selector {selector:?}: {e}"))
    })
}

fn parse_json_path(field: Option<&str>, path: &str) -> Result<JsonPath, KernelError> {
    let path = path.trim();
    let full = if path.starts_with('$') {
        path.to_string()
    } else if path.starts_with('[') || path.starts_with('.') {
        format!("${path}")
    } else {
        format!("$.{path}")
    };
    JsonPath::parse(&full)
        .map_err(|e| KernelError::selector(field, format!("invalid JSONPath {full:?}: {e}")))
}

// ── HTML ────────────────────────────────────────────────────────────────

fn extract_html(
    req: &ExtractRequest,
    fields: &[Field],
    limit: usize,
    out: &mut Output,
) -> Result<(), KernelError> {
    let items = match req.items.trim() {
        "" => None,
        spec => {
            check_len(None, spec)?;
            let css = match spec.strip_prefix("css:") {
                Some(css) => css,
                None if spec.starts_with("json:") || spec.starts_with("re:") => {
                    return Err(KernelError::selector(
                        None,
                        "items must be a CSS selector for format \"html\"",
                    ));
                }
                None => spec,
            };
            match parse_css(None, css)? {
                Some(sel) => Some(sel),
                None => return Err(KernelError::selector(None, "items selector is empty")),
            }
        }
    };

    let doc = Html::parse_document(&req.document);
    let base = effective_base(&doc, &req.base_url);
    let roots: Box<dyn Iterator<Item = ElementRef<'_>>> = match &items {
        Some(sel) => Box::new(doc.select(sel)),
        None => Box::new(std::iter::once(doc.root_element())),
    };
    for root in roots.take(limit) {
        let mut record = BTreeMap::new();
        let mut text_cache: Option<String> = None;
        for field in fields {
            let value = match &field.spec {
                FieldSpec::Css(sel, mode) => css_value(root, sel.as_ref(), mode, base.as_ref()),
                FieldSpec::Re(re) => {
                    let text = text_cache.get_or_insert_with(|| element_text(root));
                    regex_value(re, text)
                }
                FieldSpec::Json(_) => None, // rejected during parsing
            };
            if let Some(v) = value {
                record.insert(field.name.clone(), v);
            }
        }
        out.push(record)?;
    }
    Ok(())
}

/// `base_url`, adjusted by the document's `<base href>` when present.
fn effective_base(doc: &Html, base_url: &str) -> Option<Url> {
    let base = Url::parse(base_url.trim()).ok();
    let sel = Selector::parse("base[href]").ok()?;
    let href = doc
        .select(&sel)
        .next()
        .and_then(|e| e.value().attr("href"))
        .map(str::trim);
    match (base, href) {
        (Some(b), Some(h)) if !h.is_empty() => b.join(h).ok().or(Some(b)),
        (None, Some(h)) if !h.is_empty() => Url::parse(h).ok(),
        (b, _) => b,
    }
}

fn css_value(
    root: ElementRef<'_>,
    sel: Option<&Selector>,
    mode: &CssMode,
    base: Option<&Url>,
) -> Option<String> {
    let value_of = |el: ElementRef<'_>| -> Option<String> {
        let v = match mode {
            CssMode::Text => element_text(el),
            CssMode::Html => el.inner_html(),
            CssMode::Attr(name) => {
                let raw = el.value().attr(name)?.trim();
                match base {
                    Some(base) if name == "href" || name == "src" => base
                        .join(raw)
                        .map(String::from)
                        .unwrap_or_else(|_| raw.to_string()),
                    _ => raw.to_string(),
                }
            }
        };
        if v.trim().is_empty() { None } else { Some(v) }
    };
    match sel {
        None => value_of(root),
        Some(sel) => root.select(sel).find_map(value_of),
    }
}

/// Elements whose text never counts (unless selected directly).
fn is_hidden(name: &str) -> bool {
    matches!(name, "script" | "style" | "noscript" | "template" | "head")
}

/// Elements whose boundaries separate words, like a line break would.
fn is_block(name: &str) -> bool {
    matches!(
        name,
        "address"
            | "article"
            | "aside"
            | "blockquote"
            | "br"
            | "dd"
            | "div"
            | "dl"
            | "dt"
            | "fieldset"
            | "figcaption"
            | "figure"
            | "footer"
            | "form"
            | "h1"
            | "h2"
            | "h3"
            | "h4"
            | "h5"
            | "h6"
            | "header"
            | "hr"
            | "li"
            | "main"
            | "nav"
            | "ol"
            | "p"
            | "pre"
            | "section"
            | "table"
            | "tbody"
            | "td"
            | "tfoot"
            | "th"
            | "thead"
            | "tr"
            | "ul"
            | "option"
            | "title"
    )
}

/// Text content of an element with whitespace collapsed and trimmed. Block
/// boundaries count as whitespace; descendants in [`is_hidden`] are skipped.
/// Iterative, so deeply nested documents cannot exhaust the stack.
pub(crate) fn element_text(root: ElementRef<'_>) -> String {
    let mut raw = String::new();
    let root_id = root.id();
    let mut stack = vec![Some(*root)];
    while let Some(item) = stack.pop() {
        let Some(node) = item else {
            raw.push(' ');
            continue;
        };
        match node.value() {
            Node::Text(t) => raw.push_str(t),
            Node::Element(e) => {
                let name = e.name();
                if node.id() != root_id && is_hidden(name) {
                    continue;
                }
                if is_block(name) {
                    raw.push(' ');
                    stack.push(None);
                }
                stack.extend(node.children().rev().map(Some));
            }
            _ => {}
        }
    }
    collapse_whitespace(&raw)
}

fn collapse_whitespace(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for word in s.split(char::is_whitespace).filter(|w| !w.is_empty()) {
        if !out.is_empty() {
            out.push(' ');
        }
        out.push_str(word);
    }
    out
}

fn regex_value(re: &Regex, text: &str) -> Option<String> {
    let caps = re.captures(text)?;
    let m = caps
        .iter()
        .skip(1)
        .flatten()
        .next()
        .or_else(|| caps.get(0))?;
    let v = m.as_str();
    if v.is_empty() {
        None
    } else {
        Some(v.to_string())
    }
}

// ── JSON ────────────────────────────────────────────────────────────────

fn extract_json(
    req: &ExtractRequest,
    fields: &[Field],
    limit: usize,
    out: &mut Output,
) -> Result<(), KernelError> {
    let doc: Value = serde_json::from_str(&req.document).map_err(|e| {
        KernelError::new(
            "invalid_document",
            format!("document is not valid JSON: {e}"),
        )
    })?;
    let items = match req.items.trim() {
        "" => None,
        spec => {
            check_len(None, spec)?;
            let path = match spec.strip_prefix("json:") {
                Some(p) => p,
                None if spec.starts_with("css:") || spec.starts_with("re:") => {
                    return Err(KernelError::selector(
                        None,
                        "items must be a JSONPath for format \"json\"",
                    ));
                }
                None => spec,
            };
            Some(parse_json_path(None, path)?)
        }
    };
    let roots: Vec<&Value> = match &items {
        Some(path) => path.query(&doc).all(),
        None => vec![&doc],
    };
    for root in roots.into_iter().take(limit) {
        let mut record = BTreeMap::new();
        let mut text_cache: Option<String> = None;
        for field in fields {
            let value = match &field.spec {
                FieldSpec::Json(path) => path.query(root).iter().find_map(|v| json_scalar(v)),
                FieldSpec::Re(re) => {
                    let text = text_cache.get_or_insert_with(|| match root {
                        Value::String(s) => s.clone(),
                        other => other.to_string(),
                    });
                    regex_value(re, text)
                }
                FieldSpec::Css(..) => None, // rejected during parsing
            };
            if let Some(v) = value {
                record.insert(field.name.clone(), v);
            }
        }
        out.push(record)?;
    }
    Ok(())
}

/// Strings as-is, numbers and booleans as JSON text, objects/arrays as compact
/// JSON; null and empty strings count as no value.
fn json_scalar(v: &Value) -> Option<String> {
    match v {
        Value::Null => None,
        Value::String(s) if s.trim().is_empty() => None,
        Value::String(s) => Some(s.clone()),
        other => Some(other.to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn req(doc: &str, items: &str, fields: &[(&str, &str)]) -> ExtractRequest {
        ExtractRequest {
            document: doc.into(),
            items: items.into(),
            fields: fields
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
            ..Default::default()
        }
    }

    #[test]
    fn html_items_and_modes() {
        let doc = r#"<html><head><title>T</title><script>var x</script></head><body>
            <div class="m"><h3 class="n">  Ada
              Lovelace </h3><a href="/ada">p</a><p>Phone: +1 555 0100</p></div>
            <div class="m"><h3 class="n">Alan</h3><a href="https://x.test/alan" class="l">p</a></div>
            </body></html>"#;
        let mut r = req(
            doc,
            ".m",
            &[
                ("name", "css:.n::text"),
                ("url", "css:a::attr(href)"),
                ("phone", r"re:Phone:\s*([+\d ]+\d)"),
                ("html", "css:.n::html"),
                ("missing", "css:.nope"),
            ],
        );
        r.base_url = "https://example.com/team/".into();
        let out = extract(&r).unwrap();
        assert_eq!(out.records.len(), 2);
        assert_eq!(out.records[0]["name"], "Ada Lovelace");
        assert_eq!(out.records[0]["url"], "https://example.com/ada");
        assert_eq!(out.records[0]["phone"], "+1 555 0100");
        assert!(out.records[0]["html"].contains("Lovelace"));
        assert!(!out.records[0].contains_key("missing"));
        assert_eq!(out.records[1]["url"], "https://x.test/alan");
        assert!(!out.records[1].contains_key("phone"));
    }

    #[test]
    fn whole_document_and_self_selector() {
        let doc = r#"<a class="x" href="a.html"> one </a><a class="x" href="b.html">two</a>"#;
        let out = extract(&req(
            doc,
            "css:a.x",
            &[("href", "css:::attr(href)"), ("t", "css:::text")],
        ))
        .unwrap();
        assert_eq!(out.records[1]["href"], "b.html");
        assert_eq!(out.records[0]["t"], "one");
        let out = extract(&req(doc, "", &[("first", "css:a")])).unwrap();
        assert_eq!(
            out.records,
            vec![BTreeMap::from([("first".to_string(), "one".to_string())])]
        );
    }

    #[test]
    fn json_paths() {
        let doc =
            r#"{"data":{"people":[{"name":"A","age":3,"tags":["x"],"n":null},{"name":"B"}]}}"#;
        let mut r = req(
            doc,
            "json:$.data.people[*]",
            &[
                ("name", "json:$.name"),
                ("age", "json:age"),
                ("tags", "json:tags"),
                ("n", "json:n"),
            ],
        );
        r.format = "json".into();
        let out = extract(&r).unwrap();
        assert_eq!(out.records.len(), 2);
        assert_eq!(out.records[0]["age"], "3");
        assert_eq!(out.records[0]["tags"], r#"["x"]"#);
        assert!(!out.records[0].contains_key("n"));
        assert_eq!(out.records[1].len(), 1);
    }

    #[test]
    fn errors_are_structured() {
        let e = extract(&req("<p>", "", &[("a", "css:p[")])).unwrap_err();
        assert_eq!(
            (e.code, e.field.as_deref()),
            ("invalid_selector", Some("a"))
        );
        assert_eq!(
            extract(&req("<p>", "", &[("a", "xpath://p")]))
                .unwrap_err()
                .code,
            "invalid_selector"
        );
        assert_eq!(
            extract(&req("<p>", "", &[("a", "re:(")])).unwrap_err().code,
            "invalid_selector"
        );
        assert_eq!(
            extract(&req("<p>", "", &[("a", "json:$.a")]))
                .unwrap_err()
                .code,
            "invalid_selector"
        );
        assert_eq!(
            extract(&req("<p>", "[[", &[])).unwrap_err().code,
            "invalid_selector"
        );
        let mut r = req("{", "", &[]);
        r.format = "json".into();
        assert_eq!(extract(&r).unwrap_err().code, "invalid_document");
        let mut r = req("", "", &[]);
        r.limit = -1;
        assert_eq!(extract(&r).unwrap_err().code, "invalid_request");
        let r = req(&"x".repeat(MAX_DOCUMENT_BYTES + 1), "", &[]);
        assert_eq!(extract(&r).unwrap_err().code, "document_too_large");
    }

    #[test]
    fn limit_and_deep_nesting() {
        let doc = "<li>a</li>".repeat(50);
        let mut r = req(&doc, "li", &[("t", "css:::text")]);
        r.limit = 7;
        assert_eq!(extract(&r).unwrap().records.len(), 7);
        let deep = format!("{}x{}", "<span>".repeat(100_000), "</span>".repeat(100_000));
        let out = extract(&req(&deep, "", &[("t", "re:x")])).unwrap();
        assert_eq!(out.records[0]["t"], "x");
    }

    #[test]
    fn output_budget() {
        let doc = format!("<p>{}</p>", "y".repeat(1 << 20));
        let r = req(
            &doc.repeat(9),
            "p",
            &[("t", "css:::text"), ("h", "css:::html")],
        );
        assert_eq!(extract(&r).unwrap_err().code, "output_too_large");
    }
}
