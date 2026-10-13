"""Calls the real core client so a keyword missing from agento11y fails here."""

from __future__ import annotations

import inspect

from agento11y import _experiments_transport
from agento11y.experiments import Client
from agento11y_harbor.integration import _publish_trajectory
from harbor_fakes import harbor_result, write_trajectory


def test_record_generation_accepts_the_observed_step_time():
    parameters = inspect.signature(Client.record_generation).parameters

    assert {"started_at", "completed_at", "usage", "metadata", "tags", "conversation_id"} <= set(parameters)


def test_trajectory_import_through_real_client(tmp_path, monkeypatch):
    sent = []
    monkeypatch.setattr(_experiments_transport, "export_generation_json", lambda **request: sent.append(request))
    client = Client("https://stack.grafana.net/agento11y", ingest_token="glc_token")
    write_trajectory(tmp_path)

    conversation_id, generation_id = _publish_trajectory(client, tmp_path, harbor_result(), "exp-1", "case-1", 1)

    assert generation_id and conversation_id
    [generation] = [request["generation"] for request in sent]
    assert generation["id"] == generation_id
    assert generation["conversation_id"] == conversation_id
    assert generation["started_at"] == generation["completed_at"]
    assert generation["started_at"].startswith("2026-09-17T00:00:12")
