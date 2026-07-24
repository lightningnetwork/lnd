#!/usr/bin/env python3
"""Pre-flight checks for an optimize_anything run (gepa package) — fail fast before a long job.

    python preflight.py                      # checks gepa + reflection-LM creds
    python preflight.py --engine autoresearch  # also checks the `claude` CLI + jq
    GEPA_REFLECTION_LM=anthropic/claude-sonnet-4-6 python preflight.py --test-lm

Exit code 0 = all good; non-zero = at least one blocker.
"""
from __future__ import annotations

import argparse
import os
import shutil
import sys

OK, BAD = "\033[32mOK\033[0m", "\033[31mFAIL\033[0m"
problems: list[str] = []

# The gepa backend's reflection LM defaults to openai/gpt-5.1; best_of_n's
# sampling model defaults to claude-sonnet-4-6 (see references/api.md).
DEFAULT_LM_BY_ENGINE = {"gepa": "openai/gpt-5.1", "best_of_n": "claude-sonnet-4-6"}


def check(label: str, ok: bool, fix: str = "") -> None:
    print(f"  [{OK if ok else BAD}] {label}")
    if not ok:
        problems.append(f"{label} — {fix}" if fix else label)


def _creds_for(lm: str) -> tuple[bool, str]:
    """Best-effort provider-credential check for a LiteLLM model id."""
    has_aws = bool(
        os.environ.get("AWS_BEARER_TOKEN_BEDROCK")
        or os.environ.get("AWS_ACCESS_KEY_ID")
        or os.environ.get("AWS_PROFILE")
    )
    if "bedrock" in lm:
        return has_aws, "export AWS creds (AWS_BEARER_TOKEN_BEDROCK / AWS_ACCESS_KEY_ID / AWS_PROFILE)"
    if lm.startswith("openai/") or lm.startswith("gpt-") or "gpt-5" in lm:
        return bool(os.environ.get("OPENAI_API_KEY")), "export OPENAI_API_KEY"
    if "claude" in lm or lm.startswith("anthropic/"):
        return bool(os.environ.get("ANTHROPIC_API_KEY")) or has_aws, "export ANTHROPIC_API_KEY (or AWS creds)"
    # Unknown provider: accept any common key being present.
    any_key = bool(
        os.environ.get("OPENAI_API_KEY") or os.environ.get("ANTHROPIC_API_KEY") or has_aws
    )
    return any_key, "export your LiteLLM provider's API key"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--engine", default="gepa",
                    choices=["gepa", "best_of_n", "autoresearch", "meta_harness"])
    ap.add_argument("--test-lm", action="store_true",
                    help="make a 1-call round-trip to the reflection LM (costs a few tokens)")
    a = ap.parse_args()

    print("== optimize_anything preflight ==")

    # 1) import + the correct API surface
    try:
        import gepa  # noqa
        from gepa.optimize_anything import OptimizeAnythingConfig, optimize_anything  # noqa
        check(f"import gepa ({getattr(gepa, '__version__', '?')}) + optimize_anything", True)
    except Exception as e:  # noqa
        check("import gepa + optimize_anything", False, "pip install 'gepa[full]'")
        print(f"      {e}")
        return _report()

    # 2) LM credentials (in-process engines that call an LLM directly)
    lm = os.environ.get("GEPA_REFLECTION_LM", "")
    if a.engine in ("gepa", "best_of_n"):
        effective_lm = lm or DEFAULT_LM_BY_ENGINE[a.engine]
        if not lm:
            print(f"      GEPA_REFLECTION_LM unset -> engine default '{effective_lm}'")
        ok, fix = _creds_for(effective_lm)
        check(f"LLM creds present for '{effective_lm}'", ok, fix)

    # 3) agentic engines need the claude CLI (and autoresearch's eval.sh uses jq)
    if a.engine in ("autoresearch", "meta_harness"):
        cli = shutil.which("claude")
        check(f"`claude` CLI on PATH (required by {a.engine})", bool(cli),
              "install + authenticate the Claude Code CLI headless")
        if cli:
            print(f"      claude -> {cli}")
        if a.engine == "autoresearch":
            check("`jq` on PATH (used by the generated eval.sh)", bool(shutil.which("jq")),
                  "install jq")
        if sys.platform.startswith("linux"):
            check("`bwrap` on PATH (default sandbox=True jails claude with bubblewrap)",
                  bool(shutil.which("bwrap")),
                  "sudo apt/dnf install bubblewrap, or pass sandbox=False (runs unconfined)")

    # 4) optional live LM round-trip
    if a.test_lm and a.engine in ("gepa", "best_of_n"):
        target = lm or DEFAULT_LM_BY_ENGINE[a.engine]
        try:
            from gepa.lm import LM
            out = LM(target)("Reply with the single word: ok")
            check(f"LM 1-call round-trip ({target})", bool(out),
                  "LM returned empty; check model id / creds / region")
        except Exception as e:  # noqa
            check(f"LM 1-call round-trip ({target})", False, str(e)[:160])

    return _report()


def _report() -> int:
    print()
    if problems:
        print(f"\033[31m{len(problems)} blocker(s):\033[0m")
        for p in problems:
            print(f"  - {p}")
        return 1
    print("\033[32mAll preflight checks passed.\033[0m")
    return 0


if __name__ == "__main__":
    sys.exit(main())
