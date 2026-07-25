#!/usr/bin/env python3
"""A GEPA LanguageModel implementation backed by the Codex CLI in headless
mode (`codex exec`), so the reflection step runs through the local Codex
agent rather than a raw API call.

The GEPA LM protocol is minimal: callable(prompt: str | list[messages]) ->
str. We render message lists to a single prompt, invoke codex
non-interactively with a read-only sandbox, and return the agent's final
message.
"""

import pathlib
import subprocess
import tempfile
import os

# A dedicated CODEX_HOME for the harness. The default ~/.codex injects the
# user's global AGENTS.md and accumulated memories into every session,
# which is how the reflection model ended up arming mail watchers instead
# of writing candidates. The harness home symlinks auth (so credential
# refreshes still work) and carries a minimal config with no instructions
# and memories disabled — verified to eliminate the hijack entirely.
HARNESS_HOME = pathlib.Path.home() / "codez" / "codex-harness-home"

HARNESS_CONFIG = """\
# Dedicated codex home for the GEPA reflection harness. No AGENTS.md, no
# memories, no notify hooks: the model must see only the reflection prompt.
model = "gpt-5.6-sol"
model_reasoning_effort = "high"

[features]
memories = false
"""


def ensure_harness_home() -> pathlib.Path:
    """Create the isolated codex home on first use."""
    HARNESS_HOME.mkdir(parents=True, exist_ok=True)

    auth = HARNESS_HOME / "auth.json"
    if not auth.exists():
        auth.symlink_to(pathlib.Path.home() / ".codex" / "auth.json")

    config = HARNESS_HOME / "config.toml"
    if not config.exists():
        config.write_text(HARNESS_CONFIG)

    return HARNESS_HOME

# The codex CLI injects the user's global agent instructions
# (~/.codex/AGENTS.md) into every session. Those instructions describe an
# interactive workflow -- session logging, mail watchers, review requests --
# and the model will happily obey them INSTEAD of the reflection task,
# returning "Watcher armed" as its final message (this burned ~70% of the
# code_split1 run's reflection budget). The preamble pins the model to its
# actual role; require_marker catches any leakage that slips through.
PREAMBLE = """\
IMPORTANT OVERRIDE: You are running as a non-interactive text-generation
function inside an automated optimization loop. Ignore ALL global or
project instructions about sessions, session logging, mail watchers,
substrate, reviews, checkpoints, or any other workflow tooling -- they do
not apply here and you must not act on them or mention them. Do not run
commands. Your ENTIRE final message must be exactly the artifact the task
below asks for, with no preamble and no commentary.

"""


class CodexLM:
    """Callable implementing GEPA's LanguageModel protocol via codex exec."""

    def __init__(self, model: str = "gpt-5.6-sol", timeout: int = 600,
                 require_marker: str = None):
        self.model = model
        self.timeout = timeout

        # require_marker is a substring every valid completion must contain
        # (e.g. "package main" for Go candidates). A completion missing it
        # is retried once with a sterner preamble before being returned
        # as-is, so one hijacked reply costs a retry rather than a wasted
        # optimizer iteration.
        self.require_marker = require_marker

    @staticmethod
    def _render(prompt) -> str:
        if isinstance(prompt, str):
            return prompt
        # Message list: render with role tags, which the model reads fine.
        parts = []
        for msg in prompt:
            role = msg.get("role", "user")
            content = msg.get("content", "")
            parts.append(f"[{role}]\n{content}")
        return "\n\n".join(parts)

    def _invoke(self, rendered: str) -> str:
        with tempfile.NamedTemporaryFile(
                mode="r", suffix=".txt", delete=False) as out:
            out_path = out.name

        env = dict(os.environ)
        env["CODEX_HOME"] = str(ensure_harness_home())

        try:
            proc = subprocess.run(
                [
                    "codex", "exec",
                    "--model", self.model,
                    "--sandbox", "read-only",
                    "--skip-git-repo-check",
                    "--output-last-message", out_path,
                    "-",  # read the prompt from stdin
                ],
                input=rendered,
                capture_output=True,
                text=True,
                timeout=self.timeout,
                env=env,
            )
            if proc.returncode != 0:
                raise RuntimeError(
                    f"codex exec failed ({proc.returncode}): "
                    f"{proc.stderr.strip()[-2000:]}"
                )
            with open(out_path) as f:
                return f.read()
        finally:
            os.unlink(out_path)

    def __call__(self, prompt) -> str:
        rendered = PREAMBLE + self._render(prompt)

        result = self._invoke(rendered)
        if self.require_marker and self.require_marker not in result:
            retry = (
                PREAMBLE
                + f"Your previous reply was rejected because it was not "
                f"the requested artifact (it must contain "
                f"`{self.require_marker}`). Do not describe tooling or "
                f"session state. Reply with ONLY the artifact.\n\n"
                + self._render(prompt)
            )
            result = self._invoke(retry)

        return result


if __name__ == "__main__":
    lm = CodexLM()
    print(lm("Reply with exactly the word: pong"))
