#!/usr/bin/env python3
"""A GEPA LanguageModel implementation backed by the Codex CLI in headless
mode (`codex exec`), so the reflection step runs through the local Codex
agent rather than a raw API call.

The GEPA LM protocol is minimal: callable(prompt: str | list[messages]) ->
str. We render message lists to a single prompt, invoke codex
non-interactively with a read-only sandbox, and return the agent's final
message.
"""

import subprocess
import tempfile
import os


class CodexLM:
    """Callable implementing GEPA's LanguageModel protocol via codex exec."""

    def __init__(self, model: str = "gpt-5.6-sol", timeout: int = 600):
        self.model = model
        self.timeout = timeout

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

    def __call__(self, prompt) -> str:
        rendered = self._render(prompt)

        with tempfile.NamedTemporaryFile(
                mode="r", suffix=".txt", delete=False) as out:
            out_path = out.name

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


if __name__ == "__main__":
    lm = CodexLM()
    print(lm("Reply with exactly the word: pong"))
