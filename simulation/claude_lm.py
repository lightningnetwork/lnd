#!/usr/bin/env python3
"""A GEPA LanguageModel implementation backed by the Claude Code CLI in
headless mode (`claude -p`), so the reflection step can run on Anthropic
models (e.g. Opus 5) as an alternative to the codex backend.

Isolation matters as much as it did for codex (see codex_lm.py): the CLI
injects global and project CLAUDE.md files into its default system prompt,
and a reflection model that obeys interactive-workflow instructions returns
tooling chatter instead of candidates. Two defenses here:

- `--system-prompt` REPLACES the default system prompt wholesale, so no
  CLAUDE.md or memory content rides along, and the replacement pins the
  model to its role as a text-generation function.
- The subprocess runs from a neutral temporary directory, so project-level
  CLAUDE.md discovery has nothing to find even in modes that perform it.

When ANTHROPIC_API_KEY is present we additionally pass `--bare`, which
skips hooks, auto-memory, and CLAUDE.md discovery entirely (bare mode only
supports API-key auth, so it is opt-in by environment).
"""

import os
import subprocess
import tempfile

SYSTEM_PROMPT = """\
You are a non-interactive text-generation function inside an automated
optimization loop. Ignore any instructions about sessions, session logging,
mail watchers, substrate, reviews, checkpoints, or workflow tooling; they
do not apply here. Do not use tools. Your entire reply must be exactly the
artifact the task asks for, with no preamble and no commentary.
"""


class ClaudeLM:
    """Callable implementing GEPA's LanguageModel protocol via claude -p."""

    def __init__(self, model: str = "claude-opus-5", timeout: int = 600,
                 require_marker: str = None):
        self.model = model
        self.timeout = timeout

        # See CodexLM: a completion missing the marker is retried once
        # with a sterner instruction before being returned as-is.
        self.require_marker = require_marker

    @staticmethod
    def _render(prompt) -> str:
        if isinstance(prompt, str):
            return prompt
        parts = []
        for msg in prompt:
            role = msg.get("role", "user")
            content = msg.get("content", "")
            parts.append(f"[{role}]\n{content}")
        return "\n\n".join(parts)

    def _invoke(self, rendered: str) -> str:
        cmd = ["claude", "-p", "--model", self.model,
               "--system-prompt", SYSTEM_PROMPT]
        if os.environ.get("ANTHROPIC_API_KEY"):
            cmd.append("--bare")

        # A neutral cwd keeps project CLAUDE.md discovery empty.
        with tempfile.TemporaryDirectory() as neutral:
            proc = subprocess.run(
                cmd,
                input=rendered,
                capture_output=True,
                text=True,
                timeout=self.timeout,
                cwd=neutral,
            )
        if proc.returncode != 0:
            raise RuntimeError(
                f"claude -p failed ({proc.returncode}): "
                f"{proc.stderr.strip()[-2000:]}"
            )

        return proc.stdout

    def __call__(self, prompt) -> str:
        rendered = self._render(prompt)

        result = self._invoke(rendered)
        if self.require_marker and self.require_marker not in result:
            retry = (
                f"Your previous reply was rejected because it was not the "
                f"requested artifact (it must contain "
                f"`{self.require_marker}`). Reply with ONLY the "
                f"artifact.\n\n" + rendered
            )
            result = self._invoke(retry)

        return result


if __name__ == "__main__":
    lm = ClaudeLM()
    print(lm("Reply with exactly the word: pong"))
