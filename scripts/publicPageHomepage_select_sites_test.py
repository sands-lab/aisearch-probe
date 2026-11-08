#!/usr/bin/env python3
"""TEST-FIRST spec for the public-page day-0 scan's pure logic. Run: python3 -m pytest publicPageHomepage_select_sites_test.py
Covers: picking the site list, the contact name tag, failure types, block-page signs, the
shell-page flag, visible text, and robots.txt matching per RFC 9309.
"""
import socket, ssl, http.client, urllib.error
from publicPageHomepage_select_sites import (pick_sites, name_tag, failure_type, block_sign, visible_text,
                       is_shell_page, parse_robots, robots_allows, robots_unavailable_verdict,
                       funnel, we_may_fetch_homepage, scan, OUR_TOKEN)

def test_pick_sites():
    rows = [(f"https://top{i}.example", 1000) for i in range(3)] + \
           [(f"https://mid{i}.example", 5000) for i in range(10)] + \
           [(f"https://low{i}.example", 10000) for i in range(5)]
    a = pick_sites(rows, top_n=3, extra_n=4, seed=7)
    # every top-group site, in order, then extra_n from the next group only
    assert [o for o, _ in a[:3]] == ["https://top0.example", "https://top1.example", "https://top2.example"]
    assert len(a) == 7 and all(g == 5000 for _, g in a[3:])
    # same seed -> same pick; it is a reproducible sample
    assert a == pick_sites(rows, top_n=3, extra_n=4, seed=7)
    assert a != pick_sites(rows, top_n=3, extra_n=4, seed=8)

def test_name_tag_needs_contact():
    # the contact comes from the environment only; refuse to run without one
    try:
        name_tag({}); assert False, "must refuse without a contact"
    except SystemExit: pass
    try:
        name_tag({"PROBE_CONTACT": "  "}); assert False, "blank contact must be refused"
    except SystemExit: pass
    t = name_tag({"PROBE_CONTACT": "contact-x"})
    assert "contact-x" in t and t.startswith("research-cache-probe/")

def test_failure_type():
    U = urllib.error.URLError
    assert failure_type(U(socket.gaierror(-2, "Name or service not known"))) == "dns"
    assert failure_type(U(TimeoutError())) == "timeout"
    assert failure_type(TimeoutError()) == "timeout"
    assert failure_type(U(ConnectionRefusedError())) == "refused"
    assert failure_type(ConnectionResetError()) == "reset"
    assert failure_type(U(ConnectionResetError())) == "reset"
    assert failure_type(http.client.RemoteDisconnected("x")) == "closed"
    assert failure_type(U(ssl.SSLCertVerificationError("bad cert"))) == "tls-cert"
    assert failure_type(U(ssl.SSLError("handshake"))) == "tls"
    assert failure_type(U(OSError(101, "Network is unreachable"))) == "unreachable"
    assert failure_type(ValueError("odd")) == "other:ValueError"

def test_block_sign():
    assert block_sign(200, {}, "<html>hello</html>") is None
    assert block_sign(403, {"Server": "cloudflare"}, "<title>Attention Required! | Cloudflare</title>") == "cloudflare"
    assert block_sign(503, {}, "<title>Just a moment...</title>") == "cloudflare"
    assert block_sign(403, {"cf-mitigated": "challenge"}, "") == "cloudflare"
    assert block_sign(403, {"Server": "AkamaiGHost"}, "Access Denied ... Reference #18.abc") == "akamai"
    assert block_sign(200, {}, "please complete the captcha to continue") == "captcha"
    # a network filter (e.g. a firewall's block page) is what a block on your own network would look like
    assert block_sign(200, {}, "FortiGuard Intrusion Prevention - Access Blocked") == "network-filter"
    assert block_sign(403, {}, "<h1>Web Page Blocked</h1>") == "network-filter"
    # a plain 403 with no recognisable page is recorded by status only
    assert block_sign(403, {}, "<html>Forbidden</html>") is None

def test_visible_text():
    h = ("<html><head><title>T</title><style>.a{x:y}</style><script>var a=1</script></head>"
         "<body><p>Hello&amp; world</p><noscript>enable js</noscript><template>t</template>"
         "<div>Second   line</div></body></html>")
    assert visible_text(h).split() == ["T", "Hello&", "world", "Second", "line"]

def test_is_shell_page():
    shell = '<html><body><div id="root"></div><script src="/app.js"></script></body></html>'
    assert is_shell_page(shell, len(visible_text(shell))) is True
    js = '<html><body><noscript>You need to enable JavaScript to run this app.</noscript></body></html>'
    assert is_shell_page(js, len(visible_text(js))) is True
    real = '<html><body><div id="root">' + "<p>news story text</p>" * 200 + "</div></body></html>"
    assert is_shell_page(real, len(visible_text(real))) is False
    # little text but no app container: just a small page, not a shell
    small = "<html><body><p>Hi</p></body></html>"
    assert is_shell_page(small, len(visible_text(small))) is False

def test_robots_basic_groups():
    r = parse_robots("User-agent: *\nDisallow: /\n\nUser-agent: ChatGPT-User\nAllow: /\n")
    assert robots_allows(r, "ChatGPT-User", "/") is True
    assert robots_allows(r, "ClaudeBot", "/") is False      # falls back to *
    # agent names match case-insensitively
    assert robots_allows(r, "chatgpt-user", "/") is True

def test_robots_longest_match_wins():
    # RFC 9309 2.2.2: the most specific (longest) rule wins; Allow wins a tie
    r = parse_robots("User-agent: *\nDisallow: /\nAllow: /$\n")
    assert robots_allows(r, "x", "/") is True
    assert robots_allows(r, "x", "/page") is False
    r = parse_robots("User-agent: *\nDisallow: /a\nAllow: /a\n")
    assert robots_allows(r, "x", "/a") is True
    r = parse_robots("User-agent: *\nAllow: /\nDisallow: /*.php$\n")
    assert robots_allows(r, "x", "/index.php") is False
    assert robots_allows(r, "x", "/index.php5") is True

def test_robots_grouping_and_junk():
    # consecutive user-agent lines share one group; rules for the same agent in two
    # groups are merged; unknown lines and comments are ignored
    txt = ("# hi\nUser-agent: GPTBot\nUser-agent: ClaudeBot\nDisallow: /   # no\n"
           "Sitemap: https://x/s.xml\nCrawl-delay: 5\n\nUser-agent: ClaudeBot\nAllow: /ok\n")
    r = parse_robots(txt)
    assert robots_allows(r, "GPTBot", "/") is False
    assert robots_allows(r, "ClaudeBot", "/ok") is True
    assert robots_allows(r, "ClaudeBot", "/") is False
    assert robots_allows(r, "Other", "/") is True           # no group, no * -> allowed
    # an empty Disallow allows everything
    assert robots_allows(parse_robots("User-agent: *\nDisallow:\n"), "x", "/") is True

def test_robots_unavailable():
    # RFC 9309 2.3.1: 4xx -> no rules (allow); 5xx or unreachable -> disallow all
    assert robots_unavailable_verdict(404) is True
    assert robots_unavailable_verdict(403) is True
    assert robots_unavailable_verdict(500) is False
    assert robots_unavailable_verdict(None) is False

def test_we_may_fetch_homepage():
    # our own scanner obeys robots.txt (RFC 9309): a group naming us wins, else "*"
    assert we_may_fetch_homepage(200, "User-agent: *\nDisallow: /\n") == (False, "robots")
    assert we_may_fetch_homepage(200, "User-agent: *\nDisallow: /private\n") == (True, None)
    assert we_may_fetch_homepage(200, f"User-agent: {OUR_TOKEN}\nDisallow: /\n") == (False, "robots")
    assert we_may_fetch_homepage(200, f"User-agent: *\nDisallow: /\n\nUser-agent: {OUR_TOKEN}\nAllow: /\n") == (True, None)
    # rules aimed only at AI crawlers do not bar us
    assert we_may_fetch_homepage(200, "User-agent: GPTBot\nDisallow: /\n") == (True, None)
    # 4xx = no rules; 5xx or no answer = treat as barred
    assert we_may_fetch_homepage(404, "") == (True, None)
    assert we_may_fetch_homepage(503, "") == (False, "robots-unavailable")
    assert we_may_fetch_homepage(None, "") == (False, "robots-unavailable")

def test_barred_site_homepage_never_requested():
    # the scan must not touch the homepage of a site whose robots.txt bars us
    asked = []
    class H(dict):
        def get(self, k, d=None): return dict.get(self, k, d)
    def fake_get(url, ua):
        asked.append(url)
        if url.endswith("/robots.txt"):
            return 200, url, H({"Content-Type": "text/plain"}), b"x", b"User-agent: *\nDisallow: /\n"
        raise AssertionError("homepage was requested for a barred site")
    rec = scan("https://barred.example", 1000, "ua", get=fake_get, resolve=lambda h: ([], False))
    assert asked == ["https://barred.example/robots.txt"]
    assert rec["skipped"] == "robots" and rec["status"] is None
    # an allowed site does get its homepage
    asked.clear()
    def ok_get(url, ua):
        asked.append(url)
        if url.endswith("/robots.txt"):
            return 404, url, H(), b"", b""
        return 200, url, H({"Content-Type": "text/html"}), b"<p>hi</p>", b"<p>hi</p>"
    rec = scan("https://ok.example", 1000, "ua", get=ok_get, resolve=lambda h: ([], False))
    assert asked == ["https://ok.example/robots.txt", "https://ok.example"]
    assert rec["status"] == 200 and rec.get("skipped") is None

def test_funnel():
    recs = [
        {"status": None, "skipped": "robots"},
        {"status": None},
        {"status": 403},
        {"status": 200, "robots_query_ok": False},
        {"status": 200, "robots_query_ok": True, "shell_page": True},
        {"status": 200, "robots_query_ok": True, "shell_page": False},
    ]
    assert funnel(recs) == [("listed", 6), ("robots.txt lets our scanner in", 5), ("answered", 4),
                            ("200 OK", 3), ("robots allows all 4 providers", 2),
                            ("not a script-only shell", 1)]

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
