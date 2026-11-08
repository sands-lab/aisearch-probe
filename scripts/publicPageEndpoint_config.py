#!/usr/bin/env python3
"""Config for the monotonic-anchor staleness run (the precision spine).
Each anchor is a public endpoint whose value only climbs, so any value a vendor returns is
datable against our ground-truth poll. `extract` pulls the live value for ground truth;
`question` is the verbatim prompt we give each vendor (fetch the URL, return ONLY the value).
Each was checked live on 2026-10-07.
"""
import json, re

def _num(b):      return b.decode("utf-8", "replace").strip()
def _osm(b):      m = re.search(rb"sequenceNumber=(\d+)", b); return m.group(1).decode() if m else None
def _jpath(*path):
    def f(b):
        j = json.loads(b)
        for k in path: j = j[k]
        return str(j)
    return f

ANCHORS = [
    {"key": "hn_maxitem",   "url": "https://hacker-news.firebaseio.com/v0/maxitem.json",
     "extract": _num, "tick": "seconds",
     "question": "the single number returned (the largest Hacker News item id)"},
    {"key": "nist_beacon",  "url": "https://beacon.nist.gov/beacon/2.0/pulse/last",
     "extract": _jpath("pulse", "pulseIndex"), "tick": "60s",
     "question": "ONLY the value of the \"pulseIndex\" field in the JSON"},
    {"key": "drand",        "url": "https://api.drand.sh/public/latest",
     "extract": _jpath("round"), "tick": "30s",
     "question": "ONLY the value of the \"round\" field in the JSON"},
    {"key": "osm_minute",   "url": "https://planet.openstreetmap.org/replication/minute/state.txt",
     "extract": _osm, "tick": "60s",
     "question": "ONLY the number after \"sequenceNumber=\" in the text"},
    {"key": "wikipedia_rcid","url": "https://en.wikipedia.org/w/api.php?action=query&list=recentchanges&rctype=edit&rclimit=1&rcprop=ids&format=json",
     "extract": _jpath("query", "recentchanges", 0, "rcid"), "tick": "sub-second",
     "question": "ONLY the \"rcid\" value of the first object in query.recentchanges"},
    {"key": "stackoverflow","url": "https://api.stackexchange.com/2.3/questions?order=desc&sort=creation&site=stackoverflow&pagesize=1",
     "extract": _jpath("items", 0, "question_id"), "tick": "minutes",
     "question": "ONLY the \"question_id\" of the first object in items"},
    {"key": "crates_io",    "url": "https://crates.io/api/v1/summary",
     "extract": _jpath("num_crates"), "tick": "minutes",
     "question": "ONLY the value of the \"num_crates\" field in the JSON"},
    {"key": "eth_height",   "url": "https://api.blockcypher.com/v1/eth/main",
     "extract": _jpath("height"), "tick": "~12s",
     "question": "ONLY the value of the \"height\" field in the JSON"},
    {"key": "ltc_height",   "url": "https://api.blockcypher.com/v1/ltc/main",
     "extract": _jpath("height"), "tick": "~2.5min",
     "question": "ONLY the value of the \"height\" field in the JSON"},
    {"key": "btc_tip",      "url": "https://mempool.space/api/blocks/tip/height",
     "extract": _num, "tick": "~10min",
     "question": "the single number returned (the current Bitcoin block height)"},
    {"key": "btc_count",    "url": "https://blockchain.info/q/getblockcount",
     "extract": _num, "tick": "~10min",
     "question": "the single number returned (the current Bitcoin block count)"},
    {"key": "usgs_quakes",  "url": "https://earthquake.usgs.gov/earthquakes/feed/v1.0/summary/all_hour.geojson",
     "extract": _jpath("metadata", "generated"), "tick": "60s",
     "question": "ONLY the value of metadata.generated in the JSON"},
]

def prompt_for(a):
    return (f"Fetch {a['url']} and reply with ONLY {a['question']}, exactly as shown, "
            f"and nothing else — no words, no punctuation, just the value.")

ANCHORS_BY_KEY = {a["key"]: a for a in ANCHORS}
