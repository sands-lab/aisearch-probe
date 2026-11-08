"""Tests for sharingApi_score.py: the page match, the download window, and the verdicts."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import sharingApi_score as g  # noqa: E402

U = "https://agentfetchlab.com/depthhtmlpage/sh-run1-ob/2048"


def rec(t, path, ua="Mozilla/5.0 ChatGPT-User", bot="openai-query", host="agentfetchlab.com"):
    return {"ts_unix": t, "page_key": path, "url": f"https://{host}{path}",
            "user_agent": ua, "bot": bot, "ts": "2026-10-28T00:00:00Z", "status_code": 200}


class Grade(unittest.TestCase):
    def test_same_page_counts_every_size_not_the_control(self):
        self.assertTrue(g.same_page("/depthhtmlpage/sh-run1-ob/2048", U))
        self.assertTrue(g.same_page("/depthhtmlpage/sh-run1-ob/4096", U))
        self.assertTrue(g.same_page("/depthhtmlpage/sh-run1-ob", U))
        self.assertFalse(g.same_page("/depthhtmlpage/sh-run1-ob-ctl/2048", U))

    def test_window_own_probes_and_other_host(self):
        recs = [
            rec(99, "/depthhtmlpage/sh-run1-ob/2048"),                 # before sending
            rec(110, "/depthhtmlpage/sh-run1-ob/2048"),                # counts
            rec(120, "/depthhtmlpage/sh-run1-ob/2048", ua="probe-liveness/1", bot="probe-liveness"),
            rec(130, "/depthhtmlpage/sh-run1-ob/2048", host="probe.attentionhub.org"),
            rec(100 + g.SENT_WINDOW + 1, "/depthhtmlpage/sh-run1-ob/2048"),  # too late
        ]
        self.assertEqual(len(g.downloads(recs, U, 100)), 1)

    def test_verdicts_follow_the_runner(self):
        ok = {"fetched": True, "marker": True}
        silent = {"fetched": False, "marker": True}
        self.assertEqual(g.verdict([ok, silent, ok, silent])[0], "SHARED")
        self.assertEqual(g.verdict([ok, silent, ok, silent])[1], "still shared at the late step")
        self.assertEqual(g.verdict([ok, ok, ok])[0], "NOT SHARED")
        self.assertEqual(g.verdict([ok, silent, {"fetched": False}])[0], "VOID")       # control failed
        self.assertEqual(g.verdict([{"fetched": False}, silent, ok])[0], "VOID")       # no warm download
        self.assertEqual(g.verdict([ok, {"fetched": False, "marker": False}, ok])[0], "VOID")
        self.assertEqual(g.verdict([ok, dict(silent, leaked=True), ok])[0], "CAN'T TELL")
        self.assertEqual(g.verdict([dict(ok, error="429"), silent, ok])[0], "VOID")
        self.assertEqual(g.verdict([ok, silent])[0], "INCOMPLETE")


if __name__ == "__main__":
    unittest.main()
