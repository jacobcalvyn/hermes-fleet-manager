#!/usr/bin/env python3
"""Apply and verify the Fleet-owned Hermes runtime configuration."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time

from hermes_cli import config as hermes_config


SCHEMA_VERSION = 2
STATE_FILENAME = ".fleet-runtime-ready.json"
LOCK_FILENAME = ".fleet-runtime-config.lock"


def configuration_revision(
    provider: str,
    model: str,
    reasoning: str,
    service_tier: str,
) -> str:
    return hashlib.sha256(
        "\0".join((provider, model, reasoning, service_tier)).encode()
    ).hexdigest()


def desired_values(args: argparse.Namespace) -> tuple[str, str, str, str, str]:
    provider = (args.provider or os.getenv("HERMES_INFERENCE_PROVIDER", "")).strip()
    model = (args.model or os.getenv("HERMES_INFERENCE_MODEL", "")).strip()
    reasoning = (args.reasoning or os.getenv("HERMES_REASONING_EFFORT", "")).strip()
    service_tier = (args.service_tier or os.getenv("HERMES_SERVICE_TIER", "")).strip()
    build_id = (args.runtime_build_id or os.getenv("HERMES_FLEET_RUNTIME_BUILD_ID", "unknown")).strip()
    if not provider or not model:
        raise RuntimeError("Fleet provider and model are required")
    if not reasoning or not service_tier:
        raise RuntimeError("Fleet reasoning and service tier are required")
    if len(build_id) != 64 or any(character not in "0123456789abcdef" for character in build_id):
        raise RuntimeError("Fleet runtime wrapper build identity is invalid")
    return provider, model, reasoning, service_tier, build_id


def hermes_home() -> Path:
    return Path(os.getenv("HERMES_HOME", "/data"))


def load_runtime_config() -> tuple[dict[str, object], dict[str, object]]:
    loaded = hermes_config.load_config()
    model = loaded.get("model", {}) if isinstance(loaded, dict) else {}
    agent = loaded.get("agent", {}) if isinstance(loaded, dict) else {}
    return (
        model if isinstance(model, dict) else {},
        agent if isinstance(agent, dict) else {},
    )


def expected_state(
    provider: str,
    model: str,
    reasoning: str,
    service_tier: str,
    build_id: str,
) -> dict[str, object]:
    return {
        "schema_version": SCHEMA_VERSION,
        "configuration_revision": configuration_revision(
            provider, model, reasoning, service_tier
        ),
        "provider": provider,
        "model": model,
        "reasoning": reasoning,
        "service_tier": service_tier,
        "runtime_build_id": build_id,
    }


def write_state(state: dict[str, object]) -> None:
    home = hermes_home()
    home.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".fleet-runtime-ready-", dir=home)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(state, handle, sort_keys=True, separators=(",", ":"))
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, home / STATE_FILENAME)
        directory = os.open(home, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def apply(
    provider: str,
    model: str,
    reasoning: str,
    service_tier: str,
    build_id: str,
) -> None:
    home = hermes_home()
    home.mkdir(parents=True, exist_ok=True)
    with (home / LOCK_FILENAME).open("a+", encoding="utf-8") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        raw = hermes_config.read_raw_config() or {}
        if not isinstance(raw, dict):
            raise RuntimeError("Hermes configuration root must be a mapping")
        model_config = raw.get("model", {})
        if not isinstance(model_config, dict):
            model_config = {}
        model_config["default"] = model
        model_config["provider"] = provider
        raw["model"] = model_config
        agent_config = raw.get("agent", {})
        if not isinstance(agent_config, dict):
            agent_config = {}
        agent_config["reasoning_effort"] = reasoning
        agent_config["service_tier"] = service_tier
        raw["agent"] = agent_config
        hermes_config.save_config(
            raw,
            strip_defaults=False,
            preserve_keys={
                ("model", "default"),
                ("model", "provider"),
                ("agent", "reasoning_effort"),
                ("agent", "service_tier"),
            },
        )
        effective_model, effective_agent = load_runtime_config()
        if (
            effective_model.get("default") != model
            or effective_model.get("provider") != provider
            or effective_agent.get("reasoning_effort") != reasoning
            or effective_agent.get("service_tier") != service_tier
        ):
            raise RuntimeError("Hermes did not persist the Fleet runtime configuration")
        write_state(expected_state(provider, model, reasoning, service_tier, build_id))


def ready(
    provider: str,
    model: str,
    reasoning: str,
    service_tier: str,
    build_id: str,
) -> bool:
    effective_model, effective_agent = load_runtime_config()
    if (
        effective_model.get("default") != model
        or effective_model.get("provider") != provider
        or effective_agent.get("reasoning_effort") != reasoning
        or effective_agent.get("service_tier") != service_tier
    ):
        return False
    try:
        state = json.loads((hermes_home() / STATE_FILENAME).read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return False
    return (
        isinstance(state, dict)
        and state.get("schema_version") == SCHEMA_VERSION
        and state.get("provider") == provider
        and state.get("model") == model
        and state.get("reasoning") == reasoning
        and state.get("service_tier") == service_tier
        and state.get("configuration_revision")
        == configuration_revision(provider, model, reasoning, service_tier)
        and state.get("runtime_build_id") == build_id
    )


def verify(
    provider: str,
    model: str,
    reasoning: str,
    service_tier: str,
    build_id: str,
    timeout: float,
) -> None:
    deadline = time.monotonic() + max(timeout, 0)
    while True:
        if ready(provider, model, reasoning, service_tier, build_id):
            return
        if time.monotonic() >= deadline:
            raise RuntimeError("Fleet runtime configuration is not ready")
        time.sleep(0.25)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("apply", "verify"))
    parser.add_argument("--provider")
    parser.add_argument("--model")
    parser.add_argument("--reasoning")
    parser.add_argument("--service-tier")
    parser.add_argument("--runtime-build-id")
    parser.add_argument("--timeout", type=float, default=float(os.getenv("HERMES_FLEET_READY_TIMEOUT", "30")))
    args = parser.parse_args()
    provider, model, reasoning, service_tier, build_id = desired_values(args)
    if args.action == "apply":
        apply(provider, model, reasoning, service_tier, build_id)
    else:
        verify(provider, model, reasoning, service_tier, build_id, args.timeout)


if __name__ == "__main__":
    main()
