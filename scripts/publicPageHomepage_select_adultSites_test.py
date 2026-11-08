#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_select_adultSites.py. Run: python3 family_dns_check_test.py
Cloudflare's 1.1.1.1 for Families (1.1.1.3) answers 0.0.0.0 for a domain it classes as malware or
adult content. These tests pin how a DNS-over-HTTPS JSON reply becomes a verdict.
"""
from publicPageHomepage_select_adultSites import verdict, host_of

A, AAAA, CNAME = 1, 28, 5

def reply(status=0, answers=()):
    return {"Status": status, "Answer": [{"name": n, "type": t, "data": d} for n, t, d in answers]}

def test_real_address_is_allowed():
    assert verdict(reply(answers=[("chess.com.", A, "104.18.137.67")])) == "allowed"
    assert verdict(reply(answers=[("www.x.com.", CNAME, "x.cdn.net."), ("x.cdn.net.", A, "1.2.3.4")])) == "allowed"

def test_zero_address_is_blocked():
    assert verdict(reply(answers=[("bad.example.", A, "0.0.0.0")])) == "adult-blocked"
    assert verdict(reply(answers=[("bad.example.", AAAA, "::")])) == "adult-blocked"
    # blocked after following an alias
    assert verdict(reply(answers=[("www.bad.example.", CNAME, "bad.example."), ("bad.example.", A, "0.0.0.0")])) == "adult-blocked"

def test_no_address():
    assert verdict(reply(status=3)) == "no-address"            # NXDOMAIN: the name does not exist
    assert verdict(reply(status=0)) == "no-address"            # exists, but no address records
    assert verdict(reply(answers=[("a.", CNAME, "b.")])) == "no-address"

def test_server_failure_is_error_not_a_guess():
    assert verdict(reply(status=2)) == "error"                 # SERVFAIL
    assert verdict({}) == "error"

def test_host_of():
    assert host_of("https://www.chess.com") == "www.chess.com"
    assert host_of("https://www.chess.com,1000") == "www.chess.com"
    assert host_of("m.youtube.com") == "m.youtube.com"

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
