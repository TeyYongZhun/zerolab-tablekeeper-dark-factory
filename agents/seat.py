"""Run one factory seat: a Band agent driven by OpenCode on a Featherless model.

The seat's mandate (mandates/<seat>.md) is both its instructions and the source of
its model id, so the file judges read is exactly what the seat runs.

Needs `opencode serve --hostname=127.0.0.1 --port=4096` running, and in .env:
RESULT_REPO (absolute path) plus BAND_<SEAT>_AGENT_ID / BAND_<SEAT>_API_KEY.
"""
import asyncio
import os
import pathlib
import re

from band import Agent, Emit
from band.adapters import OpencodeAdapter, OpencodeAdapterConfig

ROOT = pathlib.Path(__file__).resolve().parent.parent


def _load_env(path: pathlib.Path) -> None:
    if not path.is_file():
        return
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        line = line.strip()
        if line.startswith("export "):
            line = line[len("export "):]
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key, value = key.strip(), value.strip().strip("\"'")
        # An empty variable left in the shell must not hide the file's value.
        if value and not os.environ.get(key):
            os.environ[key] = value


def _require(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"{name} is not set; add it to .env")
    return value


def run(seat: str) -> None:
    _load_env(ROOT / ".env")

    mandate = (ROOT / "mandates" / f"{seat}.md").read_text(encoding="utf-8")
    model = re.search(r"(?im)^Model:\s*(\S+)", mandate)
    if not model:
        raise SystemExit(f"mandates/{seat}.md has no `Model:` line")

    result_repo = _require("RESULT_REPO")
    if not os.path.isabs(result_repo):
        raise SystemExit("RESULT_REPO must be an absolute path")

    print(f"{seat}: model {model.group(1)}, working in {result_repo}", flush=True)

    key = seat.upper().replace("-", "_")
    adapter = OpencodeAdapter(
        config=OpencodeAdapterConfig(
            # Base URL comes from OPENCODE_BASE_URL, defaulting to 127.0.0.1:4096.
            directory=result_repo,
            provider_id="featherless",
            model_id=model.group(1),
            custom_section=mandate,
            # Teaches the model band_send_message and @mentions, which the
            # room log needs for handoffs between seats.
            include_base_instructions=True,
            # "manual" stalls every tool call waiting for a human click.
            approval_mode="auto_accept",
            question_mode="auto_reject",
            # Long design documents and full-spec handoffs take a slow model
            # well over 15 minutes in a single turn.
            turn_timeout_s=3600,
        ),
        emit={Emit.TOOL_CALLS, Emit.TASK_EVENTS},
    )
    agent = Agent.create(
        adapter=adapter,
        agent_id=_require(f"BAND_{key}_AGENT_ID"),
        api_key=_require(f"BAND_{key}_API_KEY"),
    )
    asyncio.run(agent.run())
