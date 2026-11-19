"""Tests for fill_rate_report.py: python3 -I scripts/bench/test_fill_rate_report.py"""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fill_rate_report as r  # noqa: E402

COLUMNS = [
    {"id": "em", "type": "waterfall", "target_field": "email"},
    {"id": "ver", "type": "waterfall", "target_field": "email_verify"},
    {"id": "ph", "type": "waterfall", "target_field": "phone"},
    {"id": "hq", "type": "research"},
]

# column|lead|value|provider|status|error|meta
CELLS = [
    # email: a guessed info@ from ddg_email, a real address from a page, a role-named real one from a page, a miss
    "em|1|info@a.io|ddg_email|complete||",
    "em|2|jane.doe@b.io|deep_scraper|complete||",
    "em|3|info@c.io|deep_scraper|complete||",
    "em|4||||error|no_data|",
    # verify: one real verdict, one `unknown` verdict, one miss
    "ver|1|valid|smtp|complete||",
    "ver|2|unknown|smtp|complete||",
    "ver|3||||error|no_email_to_verify|",
    "ver|4||||error|no_email_to_verify|",
    # phone: 1 of 4
    "ph|1|+1 555|local_business|complete||",
    "ph|2||||error|no_data|",
    "ph|3||||error|no_data|",
    "ph|4||||error|no_data|",
    # research needs an LLM key: 0 of 4
    "hq|1||||error|No LLM provider is configured: add a key in Settings|",
    "hq|2||||error|No LLM provider is configured: add a key in Settings|",
    "hq|3||||error|No LLM provider is configured: add a key in Settings|",
    "hq|4||||error|No LLM provider is configured: add a key in Settings|",
]


class ThreeNumbers(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        with open(os.path.join(self.d, "run1_cells.psv"), "w") as f:
            f.write("\n".join(CELLS) + "\n")
        with open(os.path.join(self.d, "columns.json"), "w") as f:
            json.dump(COLUMNS, f)
        self.rep = r.build_report(self.d, commit="abc", rows=4, lf_filled=8, status="completed",
                                  up_s=10, up_to_done_s=100, run_s=90, build_s=5, reruns=[])

    def test_all_filled(self):
        a = self.rep["all_filled"]
        self.assertEqual((a["filled"], a["total"]), (5, 16))  # 3 emails + 1 verdict + 1 phone; "unknown" is no fill

    def test_excluding_guessed_emails_drops_only_the_guess(self):
        g = self.rep["excluding_guessed_emails"]
        self.assertEqual(g["guessed_emails"], 1)  # info@a.io from ddg_email; info@c.io was found on a page
        self.assertEqual((g["filled"], g["total"]), (4, 16))

    def test_excluding_key_columns_removes_the_research_column_and_its_cells(self):
        k = self.rep["excluding_key_columns"]
        self.assertEqual(k["excluded_columns"], ["hq"])
        self.assertEqual((k["filled"], k["total"]), (5, 12))

    def test_the_numbers_are_separate_fields_not_one_blend(self):
        for key in ("all_filled", "excluding_guessed_emails", "excluding_key_columns", "excluding_environment_limited"):
            self.assertIn("rate", self.rep[key])
        self.assertNotEqual(self.rep["all_filled"]["rate"], self.rep["excluding_key_columns"]["rate"])

    def test_unknown_verdicts_are_reported_apart_and_are_not_a_fill(self):
        self.assertEqual(self.rep["verify_verdict_unknown"], 1)
        self.assertEqual(self.rep["per_column"]["ver"]["filled"], 1)  # the "valid" one only

    def test_without_a_blocked_port_nothing_is_environment_limited(self):
        e = self.rep["excluding_environment_limited"]
        self.assertEqual(e["excluded_columns"], [])
        self.assertEqual((e["filled"], e["total"]), (5, 16))

    def test_text_shows_all_lines(self):
        text = r.render_text(self.rep)
        for needle in ("ALL FILLED", "EXCLUDING GUESSED EMAILS", "EXCLUDING KEY-REQUIRING COLUMNS",
                       "EXCLUDING ENVIRONMENT-LIMITED COLUMNS"):
            self.assertIn(needle, text)


PORT25 = ("smtp_port_blocked: SMTP port 25 is blocked on this server, so addresses cannot be verified; "
          "use a verification provider key")


class EnvironmentLimited(unittest.TestCase):
    """A column whose cells say port 25 is blocked cannot be filled on this machine: it is listed, and left out of
    the environment-limited rate, never silently dropped from the others."""

    def setUp(self):
        self.d = tempfile.mkdtemp()
        cells = ["em|1|jane@a.io|deep_scraper|complete||", "em|2|||error|no_data|",
                 f"ver|1|||error|{PORT25}|", f"ver|2|||error|{PORT25}|",
                 "ph|1|+1 555|local_business|complete||", "ph|2|||error|no_data|"]
        with open(os.path.join(self.d, "run1_cells.psv"), "w") as f:
            f.write("\n".join(cells) + "\n")
        with open(os.path.join(self.d, "columns.json"), "w") as f:
            json.dump(COLUMNS[:3], f)
        self.rep = r.build_report(self.d, commit="abc", rows=2, lf_filled=0, status="complete",
                                  up_s=1, up_to_done_s=1, run_s=1, build_s=1, reruns=[])

    def test_the_verify_column_is_listed_as_environment_limited(self):
        e = self.rep["excluding_environment_limited"]
        self.assertEqual(e["excluded_columns"], ["ver"])
        self.assertEqual((e["filled"], e["total"]), (2, 4))
        self.assertTrue(self.rep["per_column"]["ver"]["environment_limited"])
        self.assertFalse(self.rep["per_column"]["em"]["environment_limited"])

    def test_all_filled_keeps_every_cell(self):
        a = self.rep["all_filled"]
        self.assertEqual((a["filled"], a["total"]), (2, 6))

    def test_text_names_the_column(self):
        text = r.render_text(self.rep)
        self.assertIn("EXCLUDING ENVIRONMENT-LIMITED COLUMNS: 2/4", text)
        self.assertIn("ver", text.split("EXCLUDING ENVIRONMENT-LIMITED COLUMNS")[1])


if __name__ == "__main__":
    unittest.main()
