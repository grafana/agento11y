"""Per-export auth config tests."""

from __future__ import annotations

import base64

import pytest
from agento11y import AuthConfig, ClientConfig, GenerationExportConfig
from agento11y.config import resolve_config


def test_resolve_config_injects_tenant_header_for_generation_export() -> None:
    cfg = resolve_config(
        ClientConfig(
            generation_export=GenerationExportConfig(
                auth=AuthConfig(mode="tenant", tenant_id="tenant-a"),
            ),
        )
    )

    assert cfg.generation_export.headers["X-Scope-OrgID"] == "tenant-a"


def test_resolve_config_keeps_explicit_headers() -> None:
    cfg = resolve_config(
        ClientConfig(
            generation_export=GenerationExportConfig(
                headers={"x-scope-orgid": "override-tenant"},
                auth=AuthConfig(mode="tenant", tenant_id="tenant-a"),
            ),
        )
    )

    assert cfg.generation_export.headers["x-scope-orgid"] == "override-tenant"


def test_resolve_config_basic_auth_with_tenant_id() -> None:
    cfg = resolve_config(
        ClientConfig(
            generation_export=GenerationExportConfig(
                auth=AuthConfig(mode="basic", tenant_id="42", basic_password="secret"),
            ),
        )
    )

    expected = "Basic " + base64.b64encode(b"42:secret").decode()
    assert cfg.generation_export.headers["Authorization"] == expected
    assert cfg.generation_export.headers["X-Scope-OrgID"] == "42"


def test_resolve_config_basic_auth_with_explicit_user() -> None:
    cfg = resolve_config(
        ClientConfig(
            generation_export=GenerationExportConfig(
                auth=AuthConfig(
                    mode="basic",
                    tenant_id="42",
                    basic_user="probe-user",
                    basic_password="secret",
                ),
            ),
        )
    )

    expected = "Basic " + base64.b64encode(b"probe-user:secret").decode()
    assert cfg.generation_export.headers["Authorization"] == expected
    assert cfg.generation_export.headers["X-Scope-OrgID"] == "42"


def test_resolve_config_basic_auth_explicit_header_wins() -> None:
    cfg = resolve_config(
        ClientConfig(
            generation_export=GenerationExportConfig(
                headers={
                    "Authorization": "Basic override",
                    "X-Scope-OrgID": "override-tenant",
                },
                auth=AuthConfig(mode="basic", tenant_id="42", basic_password="secret"),
            ),
        )
    )

    assert cfg.generation_export.headers["Authorization"] == "Basic override"
    assert cfg.generation_export.headers["X-Scope-OrgID"] == "override-tenant"


# Auth configs that resolve_config rejects: only "mode requires X but X
# missing" cases. Mode-irrelevant fields (e.g. tenant_id when mode=bearer) are
# silently ignored — env layering can populate any field independently of
# mode, and rejecting cross-mode mixes only forced extra cleanup upstream.
@pytest.mark.parametrize(
    "auth",
    [
        AuthConfig(mode="tenant"),  # tenant requires tenant_id
        AuthConfig(mode="bearer"),  # bearer requires bearer_token
        AuthConfig(mode="basic"),  # basic requires password
        AuthConfig(mode="basic", basic_password="secret"),  # basic requires user/tenant
        AuthConfig(mode="unknown", tenant_id="tenant-a"),  # unknown mode
    ],
)
def test_resolve_config_rejects_missing_required_field(auth: AuthConfig) -> None:
    with pytest.raises(ValueError):
        resolve_config(ClientConfig(generation_export=GenerationExportConfig(auth=auth)))


# Auth configs that resolve_config tolerates: mode-irrelevant fields are
# ignored, the resulting headers reflect only the mode-relevant ones.
@pytest.mark.parametrize(
    "auth",
    [
        AuthConfig(mode="none", tenant_id="tenant-a"),
        AuthConfig(mode="none", bearer_token="token"),
        AuthConfig(mode="none", basic_password="secret"),
        AuthConfig(mode="tenant", tenant_id="tenant-a", bearer_token="ignored"),
        AuthConfig(mode="bearer", tenant_id="ignored", bearer_token="token"),
    ],
)
def test_resolve_config_tolerates_irrelevant_fields(auth: AuthConfig) -> None:
    cfg = resolve_config(ClientConfig(generation_export=GenerationExportConfig(auth=auth)))
    # No exception raised; auth mode preserved.
    assert cfg.generation_export.auth.mode == auth.mode
