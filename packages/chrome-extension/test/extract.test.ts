import { describe, expect, test } from "bun:test";
import { Window } from "happy-dom";

import {
    classifyHref,
    extractBest,
    extractRepeatedList,
    extractTables,
} from "../src/extract";
import { extractLinkedInPeople } from "../src/linkedin";

const BASE = "https://example.com/page";

function docFrom(html: string): Document {
    const window = new Window({ url: BASE });
    window.document.body.innerHTML = html;
    return window.document as unknown as Document;
}

describe("classifyHref", () => {
    test("mailto → email, query stripped", () => {
        expect(classifyHref("mailto:jane@acme.com?subject=hi")).toEqual({
            column: "email",
            value: "jane@acme.com",
        });
    });

    test("linkedin profile → linkedin_url, tracking params stripped", () => {
        expect(classifyHref("https://www.linkedin.com/in/jane-doe/?miniProfileUrn=urn%3Ali")).toEqual({
            column: "linkedin_url",
            value: "https://www.linkedin.com/in/jane-doe",
        });
    });

    test("relative /in/ path resolves against a linkedin base", () => {
        expect(classifyHref("/in/jane-doe", "https://www.linkedin.com/search/")).toEqual({
            column: "linkedin_url",
            value: "https://www.linkedin.com/in/jane-doe",
        });
    });

    test("non-profile linkedin URL and plain sites → website", () => {
        expect(classifyHref("https://www.linkedin.com/company/acme")?.column).toBe("website");
        expect(classifyHref("https://acme.com/about")).toEqual({
            column: "website",
            value: "https://acme.com/about",
        });
    });

    test("junk hrefs → null", () => {
        expect(classifyHref("#top")).toBeNull();
        expect(classifyHref("javascript:void(0)")).toBeNull();
        expect(classifyHref("mailto:not-an-email")).toBeNull();
        expect(classifyHref("tel:+123456")).toBeNull();
        expect(classifyHref("")).toBeNull();
    });
});

describe("extractTables", () => {
    test("header cells become columns, body rows become rows", () => {
        const doc = docFrom(`
            <table>
                <thead><tr><th>Name</th><th>Company</th><th>Email</th></tr></thead>
                <tbody>
                    <tr><td>Jane Doe</td><td>Acme</td><td>jane@acme.com</td></tr>
                    <tr><td>John Roe</td><td>Globex</td><td>john@globex.com</td></tr>
                </tbody>
            </table>`);
        const tables = extractTables(doc);
        expect(tables).toHaveLength(1);
        expect(tables[0]!.source).toBe("table");
        expect(tables[0]!.columns).toEqual(["Name", "Company", "Email"]);
        expect(tables[0]!.rows).toEqual([
            ["Jane Doe", "Acme", "jane@acme.com"],
            ["John Roe", "Globex", "john@globex.com"],
        ]);
    });

    test("headerless table gets synthesized column names, ragged rows padded", () => {
        const doc = docFrom(`
            <table>
                <tr><td>Jane</td><td>Acme</td></tr>
                <tr><td>John</td></tr>
            </table>`);
        const tables = extractTables(doc);
        expect(tables[0]!.columns).toEqual(["column_1", "column_2"]);
        expect(tables[0]!.rows).toEqual([
            ["Jane", "Acme"],
            ["John", ""],
        ]);
    });

    test("empty-body tables are dropped", () => {
        const doc = docFrom(`<table><thead><tr><th>Only</th><th>Header</th></tr></thead></table>`);
        expect(extractTables(doc)).toHaveLength(0);
    });
});

describe("extractRepeatedList", () => {
    const LIST = `
        <div id="results">
            <div class="card"><span class="n">Jane Doe</span><span class="t">CEO</span>
                <a href="mailto:jane@acme.com">email</a>
                <a href="https://www.linkedin.com/in/jane-doe">li</a></div>
            <div class="card"><span class="n">John Roe</span><span class="t">CTO</span>
                <a href="https://globex.com">site</a>
                <a href="https://www.linkedin.com/in/john-roe">li</a></div>
            <div class="card"><span class="n">Mary Major</span><span class="t">VP Sales</span>
                <a href="mailto:mary@initech.com">email</a>
                <a href="https://www.linkedin.com/in/mary-major">li</a></div>
        </div>`;

    test("finds the repeated card group and classifies hrefs into columns", () => {
        const result = extractRepeatedList(docFrom(LIST), BASE);
        expect(result).not.toBeNull();
        expect(result!.source).toBe("list");
        expect(result!.rows).toHaveLength(3);
        expect(result!.columns).toEqual(
            expect.arrayContaining(["email", "linkedin_url", "website"]),
        );

        const li = result!.columns.indexOf("linkedin_url");
        const email = result!.columns.indexOf("email");
        const website = result!.columns.indexOf("website");
        expect(result!.rows[0]![email]).toBe("jane@acme.com");
        expect(result!.rows[0]![li]).toBe("https://www.linkedin.com/in/jane-doe");
        expect(result!.rows[1]![email]).toBe("");
        expect(result!.rows[1]![website]).toBe("https://globex.com/");
        // Leaf texts land in aligned text_N columns.
        expect(result!.rows[2]).toContain("Mary Major");
        expect(result!.rows[2]).toContain("VP Sales");
    });

    test("picks the LARGEST repeated group, not a smaller one", () => {
        const doc = docFrom(`
            <ul id="nav"><li class="x">A</li><li class="x">B</li><li class="x">C</li></ul>
            ${LIST}
            <div class="card">stray sibling elsewhere</div>`);
        const result = extractRepeatedList(doc, BASE);
        // Both groups have 3 items, but the cards carry more leaf text → higher score.
        expect(result!.rows.some((r) => r.includes("Jane Doe"))).toBe(true);
    });

    test("fewer than 3 similar siblings → null", () => {
        const doc = docFrom(`<div><p class="a">one</p><p class="a">two</p></div>`);
        expect(extractRepeatedList(doc, BASE)).toBeNull();
    });
});

describe("extractBest", () => {
    test("empty page → null", () => {
        expect(extractBest(docFrom(``), BASE)).toBeNull();
    });

    test("page with only prose → null", () => {
        expect(extractBest(docFrom(`<article><h1>Title</h1><p>Just some text.</p></article>`), BASE)).toBeNull();
    });

    test("prefers a rich table over an incidental nav list", () => {
        const doc = docFrom(`
            <ul><li class="nav">Home</li><li class="nav">About</li><li class="nav">Blog</li></ul>
            <table>
                <thead><tr><th>Name</th><th>Email</th></tr></thead>
                <tbody>
                    <tr><td>Jane</td><td>jane@acme.com</td></tr>
                    <tr><td>John</td><td>john@globex.com</td></tr>
                    <tr><td>Mary</td><td>mary@initech.com</td></tr>
                </tbody>
            </table>`);
        const best = extractBest(doc, BASE);
        expect(best!.source).toBe("table");
        expect(best!.columns).toEqual(["Name", "Email"]);
    });
});

describe("extractLinkedInPeople", () => {
    test("classic entity-result markup → full_name/title/location/linkedin_url", () => {
        const doc = docFrom(`
            <ul>
                <li class="reusable-search__result-container">
                    <div class="entity-result">
                        <a href="https://www.linkedin.com/in/jane-doe?miniProfileUrn=x">
                            <span aria-hidden="true">Jane Doe</span>
                            <span class="visually-hidden">View Jane Doe's profile</span>
                        </a>
                        <div class="entity-result__primary-subtitle">CEO at Acme</div>
                        <div class="entity-result__secondary-subtitle">Berlin, Germany</div>
                    </div>
                </li>
                <li class="reusable-search__result-container">
                    <div class="entity-result">
                        <a href="https://www.linkedin.com/in/john-roe">
                            <span aria-hidden="true">John Roe</span>
                        </a>
                        <div class="entity-result__primary-subtitle">CTO at Globex</div>
                        <div class="entity-result__secondary-subtitle">Paris, France</div>
                    </div>
                </li>
            </ul>`);
        const result = extractLinkedInPeople(doc);
        expect(result).not.toBeNull();
        expect(result!.source).toBe("linkedin");
        expect(result!.columns).toEqual(["full_name", "title", "location", "linkedin_url"]);
        expect(result!.rows).toEqual([
            ["Jane Doe", "CEO at Acme", "Berlin, Germany", "https://www.linkedin.com/in/jane-doe"],
            ["John Roe", "CTO at Globex", "Paris, France", "https://www.linkedin.com/in/john-roe"],
        ]);
    });

    test("items without a profile link are skipped; none at all → null", () => {
        const doc = docFrom(`
            <div class="entity-result"><span>Upsell banner, no profile link</span></div>`);
        expect(extractLinkedInPeople(doc)).toBeNull();
    });
});
