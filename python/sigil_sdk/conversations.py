"""Read-only access to Sigil conversations and eval collections.

Sigil collections group saved conversations. These helpers list collection
members and fetch conversations with their generations.

Endpoints use the configured Sigil API endpoint and auth headers:

  GET {prefix}/eval/collections/{collection_id}/members  saved conversations in a collection
  GET {prefix}/query/conversations/{conversation_id}     one conversation with generations

The functions here are thin; :class:`sigil_sdk.client.Client` wraps them with a
resolved endpoint, insecure flag, auth headers, and path prefix (see
``Client._api_args``).
"""

from __future__ import annotations

from typing import Any
from urllib import parse as urllib_parse

from ._experiments_transport import RetryPolicy, _base_url, _request_json
from .errors import ExperimentTransportError, ValidationError

_DEFAULT_PATH_PREFIX = "/api/v1"


def list_collection_members(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    collection_id: str,
    path_prefix: str = _DEFAULT_PATH_PREFIX,
    retry: RetryPolicy | None = None,
) -> list[dict[str, Any]]:
    """Lists the saved conversations belonging to a collection.

    Returns the raw member dicts (``saved_id``, ``conversation_id``, ``name``,
    ...) as decoded JSON for this first iteration.
    """

    cid = (collection_id or "").strip()
    if cid == "":
        raise ValidationError("sigil collection validation failed: collection_id is required")
    url = _collection_members_url(api_endpoint, insecure, cid, path_prefix)
    body = _request_json("GET", url, headers, None, retry, ExperimentTransportError, "collection members list")
    return _as_member_list(body)


def get_conversation(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    conversation_id: str,
    path_prefix: str = _DEFAULT_PATH_PREFIX,
    retry: RetryPolicy | None = None,
) -> dict[str, Any]:
    """Fetches a single conversation with all of its generations.

    Returns the raw conversation dict as decoded JSON for this first iteration.
    """

    cid = (conversation_id or "").strip()
    if cid == "":
        raise ValidationError("sigil conversation validation failed: conversation_id is required")
    url = _conversation_url(api_endpoint, insecure, cid, path_prefix)
    body = _request_json("GET", url, headers, None, retry, ExperimentTransportError, "conversation get")
    return body if isinstance(body, dict) else {}


# --------------------------------------------------------------------------- #
# URL + parsing helpers
# --------------------------------------------------------------------------- #


def _as_member_list(body: Any) -> list[dict[str, Any]]:
    """Normalizes the members response (a bare array or an items/members wrapper)."""

    if isinstance(body, list):
        raw = body
    elif isinstance(body, dict):
        raw = body.get("members") or body.get("items") or []
    else:
        raw = []
    return [m for m in raw if isinstance(m, dict)]


def _prefix(path_prefix: str) -> str:
    return "/" + (path_prefix or _DEFAULT_PATH_PREFIX).strip().strip("/")


def _collection_members_url(endpoint: str, insecure: bool, collection_id: str, path_prefix: str) -> str:
    quoted = urllib_parse.quote(collection_id, safe="")
    return f"{_base_url(endpoint, insecure)}{_prefix(path_prefix)}/eval/collections/{quoted}/members"


def _conversation_url(endpoint: str, insecure: bool, conversation_id: str, path_prefix: str) -> str:
    quoted = urllib_parse.quote(conversation_id, safe="")
    return f"{_base_url(endpoint, insecure)}{_prefix(path_prefix)}/query/conversations/{quoted}"
