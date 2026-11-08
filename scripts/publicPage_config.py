#!/usr/bin/env python3
"""Shared settings for the three public-page tests: the model each vendor is asked
with, the call time limits and retries, the Anthropic fetch tool, and how a refusal
to read a page is recognised. No actions."""
import re

ANTHROPIC_TOOL = {"type": "web_fetch_20250910", "name": "web_fetch", "max_uses": 3, "max_content_tokens": 20000}
POST_KW = {"timeout": 300, "retries": 4, "retry_on_timeout": False, "waits": [30, 60, 120]}
MODELS = {"openai": "openai/gpt-5-mini", "xai": "x-ai/grok-4.3", "anthropic": "claude-sonnet-5",
          "google": "gemini-3.1-flash-lite"}
# A refusal must be about getting the document, not e.g. "cannot quote 3 lines".
REFUSAL = re.compile(r"\b((cannot|can't|can not|unable to|not able to|couldn't|could not)\s+(read|access|fetch|retrieve|open|load)"
                     r"|don't have access|do not have access|blocked|not accessible|inaccessible|unsupported content"
                     r"|failed to (fetch|retrieve|access|read|load)|fetch(ing)? failed|request failed|no access|not permitted)", re.I)
def _norm(s): return " ".join((s or "").split())
