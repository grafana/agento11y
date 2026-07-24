"""Control-plane client for stored Agent Observability test suites.

Experiment ingest writes runs, trials, scores, and generations with the ingest
token. Stored test suites live behind the Grafana plugin control plane in Cloud.
"""

from __future__ import annotations

import os
import re
import urllib.parse
from dataclasses import dataclass
from typing import Any

from .. import _experiments_transport as _transport
from ..config import _warn_legacy_env
from ..errors import ConflictError, ExperimentTransportError, NotFoundError, ValidationError
from .types import TestCase, TestSuite, _first_nonblank

_DEFAULT_CONTROL_PATH = "/api/plugins/grafana-sigil-app/resources/eval"
_GRAFANA_APP_PATH = "/a/grafana-sigil-app"
_PORTABILITY_METADATA_KEY = "agento11y.sdk.portability"
_PORTABILITY_VERSION = 1


@dataclass(slots=True)
class PushedSuite:
    """Summary returned by :meth:`TestSuitesClient.push_suite`."""

    suite_id: str
    suite_version: str
    published: bool
    suite: TestSuite
    remote_suite: dict[str, Any]
    remote_version: dict[str, Any]
    pruned_case_ids: list[str]


class TestSuitesClient:
    """Client for stored test suites through the eval control-plane routes."""

    __test__ = False

    def __init__(
        self,
        *,
        grafana_url: str = "",
        service_account_token: str = "",
        control_endpoint: str = "",
        timeout: float = 30.0,
    ) -> None:
        _warn_legacy_env()
        grafana = (grafana_url or _first_nonblank(os.environ, "AGENTO11Y_GRAFANA_URL")).strip()
        endpoint = (control_endpoint or _first_nonblank(os.environ, "AGENTO11Y_CONTROL_ENDPOINT")).strip()
        if not endpoint:
            if not grafana:
                raise ValueError("control_endpoint is required (or set AGENTO11Y_CONTROL_ENDPOINT)")
            endpoint = grafana
        endpoint = _normalize_control_endpoint(endpoint)
        token = (service_account_token or _first_nonblank(os.environ, "AGENTO11Y_SERVICE_ACCOUNT_TOKEN")).strip()
        if not token:
            raise ValueError("service_account_token is required (or set AGENTO11Y_SERVICE_ACCOUNT_TOKEN)")
        self.endpoint = endpoint.rstrip("/")
        self.control_endpoint = self.endpoint
        parsed_endpoint = urllib.parse.urlsplit(self.endpoint)
        self.grafana_url = (
            grafana.rstrip("/")
            if grafana
            else urllib.parse.urlunsplit((parsed_endpoint.scheme, parsed_endpoint.netloc, "", "", ""))
        )
        self.service_account_token = token
        self.timeout = timeout
        self._retry = _transport.RetryPolicy(timeout=timeout)

    def list_suites(self, *, limit: int = 200, max_pages: int = 100) -> list[dict[str, Any]]:
        """Lists all stored test suites visible to the control-plane token."""

        out: list[dict[str, Any]] = []
        cursor: str | None = None
        for _ in range(max_pages):
            query = {"limit": str(limit)}
            if cursor:
                query["cursor"] = cursor
            body = self._request("GET", "/test-suites", query=query)
            out.extend(_items(body))
            cursor = _normalize_cursor(body.get("next_cursor") if isinstance(body, dict) else None)
            if cursor is None:
                return out
        raise ExperimentTransportError("agento11y test suite list transport failed: pagination did not terminate")

    def get_suite(self, suite_id: str) -> dict[str, Any]:
        """Fetches a stored test suite record, including version metadata."""

        normalized = _required_id(suite_id, "suite_id")
        body = self._request("GET", f"/test-suites/{_quote(normalized)}")
        return dict(body) if isinstance(body, dict) else {}

    def list_cases(
        self,
        suite_id: str,
        version: str,
        *,
        limit: int = 200,
        max_pages: int = 100,
    ) -> list[TestCase]:
        """Lists all test cases in one exact stored suite version."""

        normalized_suite = _required_id(suite_id, "suite_id")
        normalized_version = _required_id(version, "version")
        out: list[TestCase] = []
        cursor: str | None = None
        for _ in range(max_pages):
            query = {"limit": str(limit)}
            if cursor:
                query["cursor"] = cursor
            body = self._request(
                "GET",
                f"/test-suites/{_quote(normalized_suite)}/versions/{_quote(normalized_version)}/test-cases",
                query=query,
            )
            out.extend(_remote_case_to_local(item) for item in _items(body))
            cursor = _normalize_cursor(body.get("next_cursor") if isinstance(body, dict) else None)
            if cursor is None:
                return out
        raise ExperimentTransportError("agento11y test case list transport failed: pagination did not terminate")

    def pull_suite(self, suite_id: str, version: str = "latest_published") -> TestSuite:
        """Pulls a stored suite version into the SDK's portable ``TestSuite`` shape."""

        remote = self.get_suite(suite_id)
        resolved = self.resolve_version(remote, version)
        cases = self.list_cases(suite_id, resolved)
        return TestSuite(
            suite_id=str(remote.get("suite_id") or suite_id),
            name=str(remote.get("name") or ""),
            version=resolved,
            description=str(remote.get("description") or ""),
            tags=list(remote.get("tags") or []),
            changelog=str(_version_record(remote, resolved).get("changelog") or ""),
            test_cases=cases,
        )

    def push_suite(
        self,
        suite: TestSuite,
        *,
        publish: bool = False,
        changelog: str = "",
        empty_draft: bool = False,
        prune: bool = False,
    ) -> PushedSuite:
        """Pushes local suite metadata and cases into a mutable stored draft.

        Local cases are upserted into the draft. Set ``prune=True`` to delete
        remote-only cases and make the draft exactly match the local suite.
        """

        if not isinstance(suite, TestSuite):
            raise TypeError(f"suite must be a TestSuite, got {type(suite).__name__}")
        suite_id = _required_id(suite.suite_id, "suite_id")
        remote = self._ensure_suite(suite)
        self._patch_suite_metadata(suite, remote)
        remote = self.get_suite(suite_id)
        version = self._ensure_draft_version(
            suite_id,
            remote,
            changelog=changelog or suite.changelog,
            empty_draft=empty_draft,
        )
        version_id = str(version.get("version") or "")
        if not version_id:
            raise ExperimentTransportError("agento11y test suite version transport failed: missing version")

        for case in suite.cases:
            self._upsert_case(suite_id, version_id, case)

        pruned_case_ids: list[str] = []
        if prune:
            local_ids = {case.test_case_id for case in suite.cases}
            for remote_case in self.list_cases(suite_id, version_id):
                if remote_case.test_case_id not in local_ids:
                    self._delete_case(suite_id, version_id, remote_case.test_case_id)
                    pruned_case_ids.append(remote_case.test_case_id)

        published = False
        if publish:
            version = self._request("POST", f"/test-suites/{_quote(suite_id)}/versions/{_quote(version_id)}:publish")
            published = bool(version.get("published")) if isinstance(version, dict) else True

        pulled_shape = TestSuite(
            suite_id=suite_id,
            name=suite.name or str(remote.get("name") or ""),
            version=version_id,
            description=suite.description or str(remote.get("description") or ""),
            tags=list(suite.tags or remote.get("tags") or []),
            changelog=changelog or suite.changelog,
            test_cases=list(suite.cases),
        )
        return PushedSuite(
            suite_id=suite_id,
            suite_version=version_id,
            published=published,
            suite=pulled_shape,
            remote_suite=remote,
            remote_version=dict(version) if isinstance(version, dict) else {},
            pruned_case_ids=pruned_case_ids,
        )

    def resolve_version(self, suite: dict[str, Any], version: str) -> str:
        """Resolves exact versions and aliases: latest_published, latest, draft."""

        requested = (version or "").strip()
        if requested == "":
            raise ValidationError("agento11y test suite validation failed: version is required")
        versions = _versions(suite)
        if requested == "latest_published":
            record = _latest_published(versions)
        elif requested == "latest":
            record = _latest_any(versions)
        elif requested == "draft":
            record = _draft(versions)
        else:
            for item in versions:
                if str(item.get("version") or "") == requested:
                    return requested
            raise NotFoundError(f"agento11y test suite version not found: {requested}")
        resolved = str(record.get("version") or "")
        if not resolved:
            raise NotFoundError(f"agento11y test suite version not found: {requested}")
        return resolved

    def _ensure_suite(self, suite: TestSuite) -> dict[str, Any]:
        try:
            return self.get_suite(suite.suite_id)
        except NotFoundError:
            payload = {
                "suite_id": suite.suite_id,
                "name": suite.name or suite.suite_id,
            }
            if suite.description:
                payload["description"] = suite.description
            if suite.tags:
                payload["tags"] = list(suite.tags)
            body = self._request("POST", "/test-suites", payload=payload)
            return dict(body) if isinstance(body, dict) else {}

    def _patch_suite_metadata(self, suite: TestSuite, remote: dict[str, Any]) -> None:
        patch: dict[str, Any] = {}
        if suite.name and suite.name != remote.get("name"):
            patch["name"] = suite.name
        if suite.description:
            patch["description"] = suite.description
        if suite.tags:
            patch["tags"] = list(suite.tags)
        if patch:
            self._request("PATCH", f"/test-suites/{_quote(suite.suite_id)}", payload=patch)

    def _ensure_draft_version(
        self,
        suite_id: str,
        suite: dict[str, Any],
        *,
        changelog: str,
        empty_draft: bool,
    ) -> dict[str, Any]:
        draft = _draft(_versions(suite), required=False)
        if draft:
            _validate_existing_draft_options(draft, changelog=changelog, empty_draft=empty_draft)
            return draft
        payload: dict[str, Any] = {}
        if changelog:
            payload["changelog"] = changelog
        if empty_draft:
            payload["empty_draft"] = True
        try:
            body = self._request("POST", f"/test-suites/{_quote(suite_id)}/versions", payload=payload)
            return dict(body) if isinstance(body, dict) else {}
        except ConflictError:
            refreshed = self.get_suite(suite_id)
            draft = _draft(_versions(refreshed), required=False)
            if draft:
                _validate_existing_draft_options(draft, changelog=changelog, empty_draft=empty_draft)
                return draft
            raise

    def _upsert_case(self, suite_id: str, version: str, case: TestCase) -> dict[str, Any]:
        payload = _local_case_to_remote(case)
        body = self._request(
            "POST",
            f"/test-suites/{_quote(suite_id)}/versions/{_quote(version)}/test-cases",
            payload=payload,
        )
        return dict(body) if isinstance(body, dict) else {}

    def _delete_case(self, suite_id: str, version: str, test_case_id: str) -> None:
        self._request(
            "DELETE",
            f"/test-suites/{_quote(suite_id)}/versions/{_quote(version)}/test-cases/{_quote(test_case_id)}",
        )

    def _request(
        self,
        method: str,
        path: str,
        *,
        payload: dict[str, Any] | None = None,
        query: dict[str, str] | None = None,
    ) -> Any:
        return _transport._request_json(  # noqa: SLF001 - shared internal HTTP/error handling.
            method,
            self._url(path, query),
            self._headers(),
            payload,
            self._retry,
            ExperimentTransportError,
            "test suite",
        )

    def _headers(self) -> dict[str, str]:
        return {"Authorization": _format_bearer(self.service_account_token)}

    def _url(self, path: str, query: dict[str, str] | None = None) -> str:
        url = self.endpoint + "/" + path.lstrip("/")
        if query:
            url += "?" + urllib.parse.urlencode(query)
        return url


def _format_bearer(token: str) -> str:
    raw = (token or "").strip()
    if raw.lower().startswith("bearer "):
        raw = raw[7:].strip()
    return f"Bearer {raw}"


def _normalize_control_endpoint(value: str) -> str:
    raw = value.strip().rstrip("/")
    parsed = urllib.parse.urlsplit(raw)
    if not parsed.scheme or not parsed.netloc:
        raise ValueError("control_endpoint must be an absolute URL")
    path = parsed.path.rstrip("/")
    if path.endswith(_DEFAULT_CONTROL_PATH) or path.endswith("/api/v1/eval"):
        normalized_path = path
    else:
        app_index = path.find(_GRAFANA_APP_PATH)
        if app_index >= 0:
            prefix = path[:app_index]
        else:
            prefix = path
        normalized_path = prefix.rstrip("/") + _DEFAULT_CONTROL_PATH
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, normalized_path, "", ""))


def _quote(value: str) -> str:
    return urllib.parse.quote(value, safe="")


def _required_id(value: str, name: str) -> str:
    normalized = (value or "").strip()
    if not normalized:
        raise ValidationError(f"agento11y test suite validation failed: {name} is required")
    return normalized


def _items(body: Any) -> list[dict[str, Any]]:
    if not isinstance(body, dict):
        return []
    return [dict(item) for item in body.get("items", []) or [] if isinstance(item, dict)]


def _versions(suite: dict[str, Any]) -> list[dict[str, Any]]:
    return [dict(item) for item in suite.get("versions", []) or [] if isinstance(item, dict)]


def _version_record(suite: dict[str, Any], version: str) -> dict[str, Any]:
    for item in _versions(suite):
        if str(item.get("version") or "") == version:
            return item
    return {}


def _draft(versions: list[dict[str, Any]], *, required: bool = True) -> dict[str, Any]:
    for item in versions:
        if not bool(item.get("published")):
            return item
    if required:
        raise NotFoundError("agento11y test suite version not found: draft")
    return {}


def _validate_existing_draft_options(
    draft: dict[str, Any],
    *,
    changelog: str,
    empty_draft: bool,
) -> None:
    draft_changelog = str(draft.get("changelog") or "")
    if changelog and changelog != draft_changelog:
        raise ConflictError("agento11y test suite conflict: an existing draft cannot apply a different changelog")
    if empty_draft:
        raise ConflictError("agento11y test suite conflict: empty_draft only applies when creating a new draft")


def _latest_published(versions: list[dict[str, Any]]) -> dict[str, Any]:
    published = [item for item in versions if bool(item.get("published"))]
    if not published:
        raise NotFoundError("agento11y test suite version not found: latest_published")
    return max(published, key=lambda item: _version_sort_key(str(item.get("version") or "")))


def _latest_any(versions: list[dict[str, Any]]) -> dict[str, Any]:
    if not versions:
        raise NotFoundError("agento11y test suite version not found: latest")
    return max(versions, key=lambda item: _version_sort_key(str(item.get("version") or "")))


def _version_sort_key(value: str) -> tuple[int, int, str]:
    match = re.fullmatch(r"v(\d+)", value.strip())
    if match:
        return (1, int(match.group(1)), value)
    return (0, 0, value)


def _normalize_cursor(value: Any) -> str | None:
    if value is None:
        return None
    text = str(value).strip()
    if text == "" or text == "0":
        return None
    return text


def _local_case_to_remote(case: TestCase) -> dict[str, Any]:
    if not case.test_case_id.strip():
        raise ValidationError("agento11y test suite validation failed: test_case_id is required")
    if case.input is None:
        raise ValidationError("agento11y test suite validation failed: input is required")
    metadata = dict(case.metadata)
    portability: dict[str, Any] = {"version": _PORTABILITY_VERSION}
    if case.weight != 1.0:
        portability["weight"] = case.weight
    wrapped: list[str] = []
    remote_input, input_wrapped = _ensure_object(case.input)
    if input_wrapped:
        wrapped.append("input")
    remote_expected: dict[str, Any] | None = None
    if case.expected is not None:
        remote_expected, expected_wrapped = _ensure_object(case.expected)
        if expected_wrapped:
            wrapped.append("expected")
    if wrapped:
        portability["wrapped_fields"] = wrapped
    if len(portability) > 1:
        metadata[_PORTABILITY_METADATA_KEY] = portability

    out: dict[str, Any] = {
        "test_case_id": case.test_case_id,
        "input": remote_input,
    }
    if case.name:
        out["name"] = case.name
    if case.description:
        out["description"] = case.description
    if case.tags:
        out["tags"] = list(case.tags)
    if case.category:
        out["category"] = case.category
    if remote_expected is not None:
        out["expected"] = remote_expected
    if metadata:
        out["metadata"] = metadata
    if case.artifact_refs:
        out["artifact_refs"] = [dict(ref) for ref in case.artifact_refs]
    return out


def _remote_case_to_local(data: dict[str, Any]) -> TestCase:
    metadata = dict(data.get("metadata") or {})
    portability = metadata.get(_PORTABILITY_METADATA_KEY)
    if isinstance(portability, dict) and portability.get("version") == _PORTABILITY_VERSION:
        metadata.pop(_PORTABILITY_METADATA_KEY)
    else:
        portability = {}
    weight = float(portability.get("weight", 1.0))
    wrapped = set(portability.get("wrapped_fields", []) or [])
    raw_input = data.get("input")
    raw_expected = data.get("expected")
    return TestCase(
        test_case_id=str(data.get("test_case_id") or data.get("id") or ""),
        name=str(data.get("name") or ""),
        description=str(data.get("description") or ""),
        tags=list(data.get("tags") or []),
        category=str(data.get("category") or ""),
        input=_unwrap_value(raw_input) if "input" in wrapped else raw_input,
        expected=_unwrap_value(raw_expected) if "expected" in wrapped else raw_expected,
        weight=weight,
        metadata=metadata,
        artifact_refs=[dict(ref) for ref in data.get("artifact_refs", []) or [] if isinstance(ref, dict)],
    )


def _ensure_object(value: Any) -> tuple[dict[str, Any], bool]:
    if isinstance(value, dict):
        return dict(value), False
    return {"value": value}, True


def _unwrap_value(value: Any) -> Any:
    if isinstance(value, dict) and set(value.keys()) == {"value"}:
        return value.get("value")
    return value
