#!/usr/bin/env python3
"""Flag adult-content sites with Cloudflare's 1.1.1.1 for Families resolver (1.1.1.3).
Cloudflare documents that this resolver "returns the address 0.0.0.0 instead of the real
address" for domains it classes as malware or adult content
(https://developers.cloudflare.com/1.1.1.1/setup/#1111-for-families). It uses Cloudflare's own
domain categories, so it matches the category-based filtering of Habib et al., IMC '26, for the
adult labels; it does NOT cover their CIPA or Gambling labels.

One DNS-over-HTTPS lookup per site, one at a time, 2/s. Nothing is sent to the sites themselves,
and no account or token is needed.
Pure logic is unit-tested in family_dns_check_test.py.
Usage: publicPageHomepage_select_adultSites.py <sites.txt> <out.jsonl>
"""
import sys, json, time, urllib.request, urllib.parse
from collections import Counter

DOH = "https://family.cloudflare-dns.com/dns-query"
PACE = 0.5
A, AAAA = 1, 28
BLOCK = {"0.0.0.0", "::"}

# ---- pure logic (tested) --------------------------------------------------
def host_of(line):
    line = line.strip().split(",")[0]
    if "://" in line: line = urllib.parse.urlsplit(line).hostname or ""
    return line

def verdict(j):
    """DoH JSON reply -> adult-blocked | allowed | no-address | error."""
    if "Status" not in j: return "error"
    if j["Status"] == 3: return "no-address"          # NXDOMAIN
    if j["Status"] != 0: return "error"               # SERVFAIL, REFUSED, ...
    addrs = [a.get("data") for a in j.get("Answer") or [] if a.get("type") in (A, AAAA)]
    if not addrs: return "no-address"
    return "adult-blocked" if any(a in BLOCK for a in addrs) else "allowed"

# ---- I/O ------------------------------------------------------------------
def lookup(host):
    url = DOH + "?" + urllib.parse.urlencode({"name": host, "type": "A"})
    req = urllib.request.Request(url, headers={"Accept": "application/dns-json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            j = json.load(r)
        return verdict(j), j.get("Status"), [a.get("data") for a in j.get("Answer") or []]
    except Exception as e:
        return "error", None, [type(e).__name__]

def main():
    if len(sys.argv) != 3: sys.exit(__doc__)
    src, out = sys.argv[1], sys.argv[2]
    hosts = [h for h in (host_of(l) for l in open(src)) if h]
    counts = Counter(); t0 = time.time()
    with open(out, "x") as fo:
        for i, h in enumerate(hosts, 1):
            v, status, answers = lookup(h)
            counts[v] += 1
            fo.write(json.dumps({"host": h, "verdict": v, "dns_status": status, "answers": answers,
                                 "resolver": "family.cloudflare-dns.com",
                                 "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}) + "\n")
            fo.flush()
            if i % 100 == 0: print(f"  {i}/{len(hosts)}  {dict(counts)}", flush=True)
            time.sleep(PACE)
    print(f"\nDONE {len(hosts)} in {time.time()-t0:.0f}s  {dict(counts)}")

if __name__ == "__main__": main()
