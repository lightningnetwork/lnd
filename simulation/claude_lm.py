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
- CLAUDE_CONFIG_DIR points at a sterile home (~/codez/claude-harness-home),
  so user-level settings — and, critically, user-level HOOKS — never load.
  Hooks fire even in headless -p mode and their feedback text reaches the
  model: one reflection in the first Opus run came back discussing the
  user's mail-watcher Stop hook instead of emitting a router. Auth is
  unaffected because the OAuth token travels via the environment.

When ANTHROPIC_API_KEY is present we additionally pass `--bare`, which
skips hooks, auto-memory, and CLAUDE.md discovery entirely (bare mode only
supports API-key auth, so it is opt-in by environment).
"""

import os
import pathlib
import subprocess
import tempfile

# Headless auth: a nested `claude -p` (launched from inside another Claude
# Code session) cannot reach the interactive OAuth session, so it needs
# CLAUDE_CODE_OAUTH_TOKEN. If the variable is not already exported, we read
# it from this file (create with: `claude setup-token`, store mode 0600).
TOKEN_FILE = pathlib.Path.home() / "codez" / ".claude-harness-token"

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
                 require_marker: str = None, effort: str = None):
        self.model = model
        self.timeout = timeout

        # Reasoning effort (low/medium/high/xhigh/max). Lower effort
        # trades per-proposal deliberation for iteration throughput —
        # the evolutionary loop supplies the search, so a faster, less
        # deliberate proposer can win on proposals-per-hour.
        self.effort = effort

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
        if self.effort:
            cmd += ["--effort", self.effort]
        if os.environ.get("ANTHROPIC_API_KEY"):
            cmd.append("--bare")

        env = dict(os.environ)
        if not env.get("CLAUDE_CODE_OAUTH_TOKEN") and TOKEN_FILE.exists():
            env["CLAUDE_CODE_OAUTH_TOKEN"] = (
                TOKEN_FILE.read_text().strip()
            )

        # A sterile config home keeps user-level settings and hooks out
        # of the session (see module docstring); created on first use.
        config_home = pathlib.Path.home() / "codez" / "claude-harness-home"
        config_home.mkdir(parents=True, exist_ok=True)
        env["CLAUDE_CONFIG_DIR"] = str(config_home)

        # The claude binary is a wrapper that spawns the real CLI as a
        # grandchild sharing our stdout pipe. subprocess.run's timeout
        # kills only the direct child and then blocks forever reading a
        # pipe the grandchild still holds — this hung a whole evolution
        # run. Run the tree in its own process group and kill the group
        # on timeout.
        #
        # A neutral cwd keeps project CLAUDE.md discovery empty.
        with tempfile.TemporaryDirectory() as neutral:
            proc = subprocess.Popen(
                cmd,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                cwd=neutral,
                env=env,
                start_new_session=True,
            )
            try:
                stdout, stderr = proc.communicate(
                    input=rendered, timeout=self.timeout,
                )
            except subprocess.TimeoutExpired:
                import signal

                try:
                    os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    proc.kill()
                proc.wait()
                raise RuntimeError(
                    f"claude -p timed out after {self.timeout}s "
                    "(process group killed)"
                )
        if proc.returncode != 0:
            # Scrub the credential from anything that could reach a run
            # log or transcript: the CLI must never echo the token, but
            # we do not rely on that.
            detail = stderr.strip()[-2000:]
            token = env.get("CLAUDE_CODE_OAUTH_TOKEN", "")
            if token:
                detail = detail.replace(token, "[REDACTED]")
            raise RuntimeError(
                f"claude -p failed ({proc.returncode}): {detail}"
            )

        return stdout

    def __call__(self, prompt) -> str:
        rendered = self._render(prompt)

        # A failed or timed-out reflection call must degrade to a
        # no-op proposal (which fails compile and costs one iteration),
        # never propagate and kill the run: Opus reflections routinely
        # run minutes, so timeouts are an expected operating condition.
        try:
            result = self._invoke(rendered)
            if self.require_marker and self.require_marker not in result:
                retry = (
                    f"Your previous reply was rejected because it was "
                    f"not the requested artifact (it must contain "
                    f"`{self.require_marker}`). Reply with ONLY the "
                    f"artifact.\n\n" + rendered
                )
                result = self._invoke(retry)
        except RuntimeError as exc:
            return f"// reflection unavailable: {exc}\n"

        return result


if __name__ == "__main__":
    lm = ClaudeLM()
    print(lm("Reply with exactly the word: pong"))
