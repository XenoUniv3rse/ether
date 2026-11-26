"""Ether Plugin SDK for Python 3.9+ (stdlib-only, zero external dependencies).

Implements Ether Plugin Protocol v1 (EPP/1) for sidecar and remote plugins:
- Declarative router (`plugin(manifest=..., routes=...)`) with identical route
  semantics to `@ether/plugin-sdk` and `sdk/go`.
- Settings schema decoding, validation, and copy-rule linting (SPEC §5.5).
- Localization helper (`create_i18n`, `t_ref`).
- Remote host client (`RemoteHostClient`) for `/ether/plugin-host/*` authenticated
  with `X-Ether-Plugin-Credential` (SPEC §5.1.5, WS8b).
- HTTP sidecar server (`serve(..., mode="http")`) and WebSocket tunnel client
  (`serve(..., mode="tunnel")`, `dispatch_over_tunnel_frame`).
- Local test harness, mock host client, fixtures, and capability starter templates.
"""

from __future__ import annotations

import base64
import concurrent.futures
import hashlib
import http.server
import inspect
import json
import logging
import os
import re
import socket
import socketserver
import ssl
import struct
import threading
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Literal, Optional, Tuple, TypedDict, Union

TUNNEL_PROTOCOL = "ether-tunnel-v1"
DEFAULT_MAX_CHUNK_BYTES = 64 * 1024
MAX_KIND_VISIBLE_IDS = 500
HEADER_VIEWER_TOKEN = "X-Ether-Viewer-Token"
HEADER_ORIGIN = "X-Ether-Origin"
HEADER_IDEMPOTENCY_KEY = "X-Ether-Idempotency-Key"

ALLOWED_SETTING_KINDS = frozenset(
    {
        "group",
        "text",
        "secret",
        "duration",
        "readonly",
        "number",
        "bool",
        "select",
        "list",
        "bytes",
        "hidden",
        "multiSelect",
        "orderedList",
        "language",
        "viewer",
        "collection",
        "folder",
        "url",
        "title",
    }
)

_BANNED_COPY_RE = re.compile(r"\b(simply|easily|just)\b", re.IGNORECASE)
_INTERP_RE = re.compile(r"\{([A-Za-z0-9_]+)\}")


class PluginHttpError(Exception):
    """Structured HTTP error raised by a plugin route handler."""

    def __init__(self, status: int, code: str, message: str) -> None:
        super().__init__(message)
        self.status = int(status)
        self.code = str(code)
        self.message = str(message)


PluginRouteError = PluginHttpError


# ---------------------------------------------------------------------------
# Copy rules & i18n helpers (SPEC §2.7, §2.8, §5.1.1, §5.6)
# ---------------------------------------------------------------------------


def t_ref(key: str) -> str:
    """Returns a `$t:<key>` reference for manifest and ViewSpec fields."""
    return f"$t:{key}"


def lint_copy(raw: str, max_len: int = 0) -> str:
    """Validates a user-visible string against Ether's copy rules."""
    s = raw.strip()
    if max_len > 0 and len(s) > max_len:
        return f"is longer than {max_len} characters"
    if "—" in s or "–" in s:
        return "must not contain em or en dashes"
    if "!" in s:
        return "must not contain exclamation marks"
    if _BANNED_COPY_RE.search(s):
        return "must not use the words simply, just or easily"
    for ch in s:
        if ch.isalpha():
            if ch.islower():
                return "must use sentence case"
            break
    words = [w for w in s.split() if w]
    if len(words) >= 3:
        caps = sum(1 for w in words if w[0].isalpha() and w[0].isupper())
        if caps * 3 > len(words) * 2:
            return "must use sentence case"
    return ""


class I18nHelper:
    """Resolves localized strings with fallback to base language and 'en'."""

    def __init__(
        self,
        strings: Optional[Dict[str, Dict[str, str]]] = None,
        default_locale: str = "en",
    ) -> None:
        self._strings: Dict[str, Dict[str, str]] = strings or {}
        self._default_locale = default_locale or "en"

    def t(
        self,
        raw_key: str,
        params: Optional[Dict[str, Any]] = None,
        locale: Optional[str] = None,
    ) -> str:
        key = raw_key[3:] if raw_key.startswith("$t:") else raw_key
        loc = locale or self._default_locale
        base = loc.split("-")[0] if loc else ""
        template: Optional[str] = None
        for candidate in (loc, base, "en"):
            if not candidate:
                continue
            table = self._strings.get(candidate)
            if table and key in table:
                template = table[key]
                break
        if template is None:
            template = key
        if not params:
            return template

        def _replace(match: re.Match[str]) -> str:
            name = match.group(1)
            return str(params[name]) if name in params else match.group(0)

        return _INTERP_RE.sub(_replace, template)

    def ref(self, key: str) -> str:
        return t_ref(key)

    def catalog(self) -> Dict[str, Dict[str, str]]:
        return self._strings


def create_i18n(
    strings: Optional[Dict[str, Dict[str, str]]] = None,
    default_locale: str = "en",
) -> I18nHelper:
    return I18nHelper(strings=strings, default_locale=default_locale)


# ---------------------------------------------------------------------------
# Settings schema decoding and linting (SPEC §5.5)
# ---------------------------------------------------------------------------


def collect_setting_fields(schema: Optional[Dict[str, Any]]) -> List[Dict[str, Any]]:
    if not isinstance(schema, dict):
        return []
    out: List[Dict[str, Any]] = []

    def _walk(node: Any) -> None:
        if isinstance(node, list):
            for item in node:
                _walk(item)
            return
        if not isinstance(node, dict):
            return
        key = (
            node.get("key")
            or node.get("name")
            or node.get("id")
            or node.get("field")
            or ""
        )
        kind = node.get("kind", "")
        if isinstance(key, str) and key and isinstance(kind, str) and kind and kind != "group":
            out.append(
                {
                    "key": key,
                    "kind": kind,
                    "label": node.get("label"),
                    "hint": node.get("hint"),
                    "required": node.get("required") is True,
                    "default": node.get("default"),
                    "min": node.get("min"),
                    "max": node.get("max"),
                }
            )
        for v in node.values():
            if isinstance(v, (dict, list)):
                _walk(v)

    _walk(schema)
    return out


def lint_settings_schema(schema: Optional[Dict[str, Any]]) -> List[Dict[str, str]]:
    if not isinstance(schema, dict):
        return []
    problems: List[Dict[str, str]] = []

    def _walk(node: Any, path: str) -> None:
        if isinstance(node, list):
            for idx, item in enumerate(node):
                _walk(item, f"{path}[{idx}]")
            return
        if not isinstance(node, dict):
            return
        kind = node.get("kind")
        if isinstance(kind, str):
            if kind == "json":
                problems.append(
                    {
                        "field": f"{path}.kind",
                        "message": "the json field kind is not allowed in plugin settings",
                    }
                )
            elif kind not in ALLOWED_SETTING_KINDS:
                problems.append(
                    {
                        "field": f"{path}.kind",
                        "message": f'unknown field kind "{kind}"',
                    }
                )
        label = node.get("label")
        if isinstance(label, str) and not label.startswith("$t:"):
            msg = lint_copy(label, 40)
            if msg:
                problems.append({"field": f"{path}.label", "message": msg})
        hint = node.get("hint")
        if isinstance(hint, str) and not hint.startswith("$t:"):
            msg = lint_copy(hint, 120)
            if msg:
                problems.append({"field": f"{path}.hint", "message": msg})
            trimmed = hint.rstrip(".")
            if "." in trimmed:
                problems.append({"field": f"{path}.hint", "message": "must be one sentence"})
        for k, v in node.items():
            if isinstance(v, (dict, list)):
                _walk(v, f"{path}.{k}")

    _walk(schema, "ether.settings")
    return problems


def decode_settings(
    schema: Optional[Dict[str, Any]],
    incoming: Optional[Dict[str, Any]] = None,
    defaults: Optional[Dict[str, Any]] = None,
) -> Tuple[Dict[str, Any], Dict[str, str]]:
    fields = collect_setting_fields(schema)
    result: Dict[str, Any] = dict(defaults or {})
    errors: Dict[str, str] = {}

    for f in fields:
        if f.get("default") is not None and f["key"] not in result:
            result[f["key"]] = f["default"]

    if isinstance(incoming, dict):
        for k, v in incoming.items():
            result[k] = v

    for f in fields:
        key = f["key"]
        kind = f["kind"]
        val = result.get(key)
        ptr = f"/{key}"
        if f.get("required") and (val is None or val == ""):
            errors[ptr] = "is required"
            continue
        if val is None or val == "":
            continue
        if kind in ("number", "bytes", "viewer"):
            if isinstance(val, bool):
                errors[ptr] = "must be a number"
                continue
            try:
                num = float(val) if isinstance(val, str) and "." in val else int(val) if isinstance(val, (int, str)) else float(val)
            except (ValueError, TypeError):
                errors[ptr] = "must be a number"
                continue
            if f.get("min") is not None and num < f["min"]:
                errors[ptr] = f"must be at least {f['min']}"
            if f.get("max") is not None and num > f["max"]:
                errors[ptr] = f"must be at most {f['max']}"
            result[key] = num
        elif kind == "bool":
            if not isinstance(val, bool):
                errors[ptr] = "must be true or false"
        elif kind in ("list", "multiSelect", "orderedList"):
            if not isinstance(val, list):
                errors[ptr] = "must be a list"
        elif kind == "url":
            if not isinstance(val, str) or not (
                val.startswith("http://") or val.startswith("https://")
            ):
                errors[ptr] = "must be an http or https URL"

    return result, errors


# ---------------------------------------------------------------------------
# Host client for sidecar plugins (`/ether/plugin-host/*`)
# ---------------------------------------------------------------------------


class _KVClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def get(self, key: str) -> Optional[str]:
        encoded = urllib.parse.quote(key, safe="")
        res = self._call("GET", f"/ether/plugin-host/kv/{encoded}", allow_404=True)
        return res.get("value") if isinstance(res, dict) else None

    def put(self, key: str, value: str) -> None:
        encoded = urllib.parse.quote(key, safe="")
        self._call("PUT", f"/ether/plugin-host/kv/{encoded}", {"value": str(value)})

    def delete(self, key: str) -> None:
        encoded = urllib.parse.quote(key, safe="")
        self._call("DELETE", f"/ether/plugin-host/kv/{encoded}")

    def list(self, prefix: str = "") -> List[str]:
        q = f"?prefix={urllib.parse.quote(prefix, safe='')}" if prefix else ""
        res = self._call("GET", f"/ether/plugin-host/kv{q}")
        return list(res.get("keys", [])) if isinstance(res, dict) else []

    def usage(self) -> Dict[str, int]:
        res = self._call("GET", "/ether/plugin-host/kv-usage")
        return dict(res) if isinstance(res, dict) else {}


MAX_POSTER_BADGE_SETS = 4
MAX_BADGE_ENTRIES_PER_CALL = 500
MAX_BADGE_LABELS_PER_TITLE = 3
MAX_BADGE_LABEL_LEN = 16
POSTER_BADGE_QUOTA_TITLES = 200_000

_BADGE_SET_KEY_RE = re.compile(r"^[a-z][a-z0-9_]{0,31}$")
_BADGE_LABEL_RE = re.compile(r"^[A-Z0-9 +&'./-]+$")
_IMDB_BADGE_RE = re.compile(r"^tt[0-9]{1,12}$")


class PosterBadgeSetMeta(TypedDict, total=False):
    key: str
    label: str
    description: str


class BadgeTitleRef(TypedDict, total=False):
    type: str
    tmdb: int
    imdb: str


class BadgeEntry(TypedDict):
    title: BadgeTitleRef
    labels: List[str]


class BadgeUsage(TypedDict):
    titles: int
    quotaTitles: int


def _check_badge_set_key(set_key: str) -> str:
    if not isinstance(set_key, str) or not _BADGE_SET_KEY_RE.match(set_key):
        raise ValueError("badge set key must match ^[a-z][a-z0-9_]{0,31}$")
    return set_key


def _normalize_badge_label(raw: str) -> str:
    if not isinstance(raw, str):
        raise ValueError("badge label must be a string")
    upper = "".join(chr(ord(ch) - 32) if "a" <= ch <= "z" else ch for ch in raw)
    if (
        len(upper) < 1
        or len(upper) > MAX_BADGE_LABEL_LEN
        or not upper.strip()
        or not _BADGE_LABEL_RE.match(upper)
    ):
        raise ValueError(
            f"badge label must be 1..{MAX_BADGE_LABEL_LEN} characters from [A-Z0-9 +&'./-]"
        )
    return upper


def _resolve_badge_title_key(
    title: Dict[str, Any],
    catalog_store: Dict[str, Dict[str, Any]],
) -> str:
    if not isinstance(title, dict):
        raise ValueError("badge title must be an object")
    t_type = title.get("type")
    if t_type not in ("movie", "show"):
        raise ValueError('badge title.type must be "movie" or "show"')
    tmdb_raw = title.get("tmdb")
    tmdb = 0
    if tmdb_raw is not None:
        if isinstance(tmdb_raw, bool) or not isinstance(tmdb_raw, int) or tmdb_raw <= 0:
            raise ValueError("badge title.tmdb must be a positive integer")
        tmdb = tmdb_raw
    imdb_raw = title.get("imdb")
    imdb = ""
    if imdb_raw is not None and imdb_raw != "":
        if not isinstance(imdb_raw, str):
            raise ValueError("badge title.imdb must be a string")
        norm = imdb_raw.strip().lower()
        if not _IMDB_BADGE_RE.match(norm):
            raise ValueError("badge title.imdb must match tt<digits>")
        imdb = norm
    if tmdb == 0 and not imdb:
        raise ValueError("badge title requires tmdb or imdb")
    if tmdb > 0:
        for item in catalog_store.values():
            if item.get("type") == t_type and item.get("tmdbId") == tmdb:
                return str(item.get("id"))
    if imdb:
        for item in catalog_store.values():
            if item.get("type") == t_type and str(item.get("imdbId") or "").lower() == imdb:
                return str(item.get("id"))
    return f"tmdb:{t_type}:{tmdb}" if tmdb > 0 else f"imdb:{t_type}:{imdb}"


def _count_distinct_badge_titles(store: Dict[str, Dict[str, List[str]]]) -> int:
    seen = set()
    for set_map in store.values():
        seen.update(set_map.keys())
    return len(seen)


class _BadgesClient:
    def __init__(self, call_fn: Callable[..., Any]) -> None:
        self._call = call_fn

    def set(self, set_key: str, entries: List[BadgeEntry]) -> None:
        encoded = urllib.parse.quote(str(set_key), safe="")
        self._call("PUT", f"/ether/plugin-host/badges/{encoded}", {"entries": list(entries)})

    def clear(self, set_key: str) -> None:
        encoded = urllib.parse.quote(str(set_key), safe="")
        self._call("DELETE", f"/ether/plugin-host/badges/{encoded}")

    def usage(self) -> BadgeUsage:
        res = self._call("GET", "/ether/plugin-host/badges-usage")
        if not isinstance(res, dict):
            return {"titles": 0, "quotaTitles": 0}
        return {
            "titles": int(res.get("titles", 0)),
            "quotaTitles": int(res.get("quotaTitles", 0)),
        }


MIN_DATA_READ_BYTES = 8 * 1024 * 1024
MAX_DATA_READ_BYTES = 512 * 1024 * 1024
MAX_ARCHIVE_READ_ITEMS = 4096


class RuntimeLimits(TypedDict, total=False):
    fetchBytes: int
    archiveMemberBytes: int
    archiveExpandedBytes: int
    archiveMembers: int
    maxItems: int
    memoryBytes: int
    readBytes: int
    deadline: str
    unitBudget: int
    poolSize: int


class DataRange(TypedDict):
    offset: int
    length: int


class ArchiveReadItem(TypedDict):
    member: int
    offset: int
    length: int


class ArchiveReadResult(TypedDict):
    index: int
    member: int
    size: int
    body: bytes


class ArchiveMember(TypedDict, total=False):
    index: int
    name: str
    size: int
    unsafe: bool
    unsafeReason: str


class ArchiveExtractOptions(TypedDict, total=False):
    libraryKey: str
    name: str


class ArchiveExtractResult(TypedDict):
    ref: str
    size: int
    sha1: str
    md5: str
    crc32: str


class RasterizeOptions(TypedDict, total=False):
    width: int
    height: int
    format: str


class RasterizeResult(TypedDict):
    blob: str
    contentType: str
    width: int
    height: int
    size: int


def _check_rasterize_options(options: Optional[RasterizeOptions]) -> Optional[Dict[str, Any]]:
    if options is None:
        return None
    out: Dict[str, Any] = dict(options)
    for dim in (out.get("width"), out.get("height")):
        if dim is not None and (isinstance(dim, bool) or not isinstance(dim, int) or dim < 0 or dim > 2048):
            raise ValueError("cache.rasterize width and height must be within 0..2048")
    fmt = out.get("format")
    if fmt is not None and fmt not in ("png", "jpeg"):
        raise ValueError("cache.rasterize format must be png or jpeg")
    return out


def _check_data_range(data_range: Dict[str, Any], max_bytes: int = MAX_DATA_READ_BYTES) -> Dict[str, int]:
    offset = data_range.get("offset")
    length = data_range.get("length")
    if (
        isinstance(offset, bool)
        or isinstance(length, bool)
        or not isinstance(offset, int)
        or not isinstance(length, int)
        or offset < 0
        or offset > (1 << 60)
        or length <= 0
        or length > max_bytes
    ):
        raise ValueError(f"Data range needs nonnegative offset and length 1..{max_bytes}")
    return {"offset": offset, "length": length}


def _check_archive_reads(reads: List[ArchiveReadItem], max_bytes: int = MAX_DATA_READ_BYTES) -> List[Dict[str, int]]:
    if not isinstance(reads, list) or len(reads) == 0 or len(reads) > MAX_ARCHIVE_READ_ITEMS:
        raise ValueError(f"archive.read takes 1..{MAX_ARCHIVE_READ_ITEMS} reads")
    normalized: List[Dict[str, int]] = []
    total = 0
    for item in reads:
        member = item.get("member")
        if isinstance(member, bool) or not isinstance(member, int) or member < 0:
            raise ValueError("archive.read member index must be a nonnegative integer")
        rng = _check_data_range(item, max_bytes=max_bytes)
        total += rng["length"]
        if total > max_bytes:
            raise ValueError(f"archive.read requested bytes exceed per-call readBytes limit ({max_bytes})")
        normalized.append({"member": member, "offset": rng["offset"], "length": rng["length"]})
    return normalized


class _RuntimeClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def limits(self) -> RuntimeLimits:
        res = self._call("GET", "/ether/plugin-host/limits")
        return dict(res) if isinstance(res, dict) else {}  # type: ignore[return-value]


class _ArchiveClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def list(self, blob_ref: str) -> List[ArchiveMember]:
        res = self._call("POST", "/ether/plugin-host/data/archive.list", [str(blob_ref)])
        return list(res) if isinstance(res, list) else []

    def open(self, blob_ref: str, member: int, data_range: DataRange) -> bytes:  # noqa: A002
        norm = _check_data_range(data_range)
        res = self._call("POST", "/ether/plugin-host/data/archive.open", [str(blob_ref), int(member), norm])
        return base64.b64decode((res or {}).get("bodyBase64") or "")

    def read(self, blob_ref: str, reads: List[ArchiveReadItem]) -> List[ArchiveReadResult]:
        norm = _check_archive_reads(reads)
        res = self._call("POST", "/ether/plugin-host/data/archive.read", [str(blob_ref), norm])
        rows = res if isinstance(res, list) else []
        return [
            {
                "index": int(row["index"]),
                "member": int(row["member"]),
                "size": int(row["size"]),
                "body": base64.b64decode(row.get("bodyBase64") or ""),
            }
            for row in rows
        ]

    def extract(
        self,
        blob_ref: str,
        member: int,
        options: Optional[ArchiveExtractOptions] = None,
    ) -> ArchiveExtractResult:
        args: List[Any] = [str(blob_ref), int(member)]
        if options is not None:
            args.append(dict(options))
        res = self._call("POST", "/ether/plugin-host/data/archive.extract", args)
        return res if isinstance(res, dict) else {}  # type: ignore[return-value]


class _SecretClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def set(self, name: str, value: str, ttl_sec: Optional[int] = None) -> None:
        body: Dict[str, Any] = {"value": str(value)}
        if ttl_sec is not None:
            body["ttlSec"] = int(ttl_sec)
        self._call("PUT", f"/ether/plugin-host/secrets/{urllib.parse.quote(name, safe='')}", body)

    def delete(self, name: str) -> None:
        self._call("DELETE", f"/ether/plugin-host/secrets/{urllib.parse.quote(name, safe='')}")


DEFAULT_FETCH_MANY_CONCURRENCY = 8
MAX_FETCH_MANY_CONCURRENCY = 8
MAX_FETCH_MANY_REQUESTS = 64
DEFAULT_MAX_FETCH_PER_HOST = 6


def _fetch_origin_key(raw_url: str) -> str:
    try:
        parsed = urllib.parse.urlparse(str(raw_url).strip())
    except Exception:
        return ""
    scheme = (parsed.scheme or "").lower()
    if scheme not in ("http", "https"):
        return ""
    host = (parsed.hostname or "").lower().rstrip(".")
    if not host:
        return ""
    port = parsed.port or (443 if scheme == "https" else 80)
    return f"{scheme}://{host}:{port}"


def _run_fetch_many(
    requests_list: List[Dict[str, Any]],
    opts: Optional[Dict[str, Any]],
    run_one: Callable[[Dict[str, Any]], Dict[str, Any]],
) -> List[Dict[str, Any]]:
    reqs = list(requests_list or [])
    if len(reqs) > MAX_FETCH_MANY_REQUESTS:
        raise ValueError(f"http.fetchMany accepts at most {MAX_FETCH_MANY_REQUESTS} requests per call")
    concurrency = DEFAULT_FETCH_MANY_CONCURRENCY
    if opts is not None and opts.get("concurrency") is not None:
        raw_c = opts["concurrency"]
        if isinstance(raw_c, bool) or not isinstance(raw_c, int) or raw_c < 0:
            raise ValueError("http.fetchMany concurrency must be a non-negative integer")
        if raw_c > 0:
            concurrency = min(raw_c, MAX_FETCH_MANY_CONCURRENCY)
    if not reqs:
        return []

    origin_sems: Dict[str, threading.Semaphore] = {}
    origin_lock = threading.Lock()

    def _exec(req: Dict[str, Any]) -> Dict[str, Any]:
        origin = _fetch_origin_key(str(req.get("url") or ""))
        sem: Optional[threading.Semaphore] = None
        if origin:
            with origin_lock:
                sem = origin_sems.get(origin)
                if sem is None:
                    sem = threading.Semaphore(DEFAULT_MAX_FETCH_PER_HOST)
                    origin_sems[origin] = sem
            sem.acquire()
        try:
            return run_one(req)
        except (TimeoutError, socket.timeout) as exc:
            return {"error": {"code": "timeout", "message": str(exc) or "host call timed out"}}
        except Exception as exc:
            code = getattr(exc, "code", None)
            if not isinstance(code, str) or not code:
                code = "timeout" if isinstance(getattr(exc, "reason", None), (TimeoutError, socket.timeout)) else "host_error"
            return {"error": {"code": code, "message": str(exc) or "request failed"}}
        finally:
            if sem is not None:
                sem.release()

    worker_count = min(concurrency, len(reqs))
    keys: List[str] = []
    buckets: Dict[str, List[int]] = {}
    for idx, req in enumerate(reqs):
        key = _fetch_origin_key(str(req.get("url") or ""))
        if key not in buckets:
            keys.append(key)
            buckets[key] = []
        buckets[key].append(idx)
    order: List[int] = []
    while len(order) < len(reqs):
        for key in keys:
            q = buckets[key]
            if q:
                order.append(q.pop(0))

    results: List[Dict[str, Any]] = [{} for _ in reqs]
    with concurrent.futures.ThreadPoolExecutor(max_workers=worker_count) as pool:
        future_map = {pool.submit(_exec, reqs[idx]): idx for idx in order}
        for fut, idx in future_map.items():
            results[idx] = fut.result()
    return results


class _HttpClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def hash(self, req: Dict[str, Any]) -> Dict[str, Any]:
        """Request a host-side digest (`{url, algo, expectSize?, headers?, keepHeadersOnRedirect?}`)."""
        return self._call("POST", "/ether/plugin-host/data/http.hash", [req])

    def fetch_to_cache(self, req: Dict[str, Any]) -> Dict[str, Any]:
        """Stream a remote GET into the plugin's library cache (`{url, headers?, keepHeadersOnRedirect?, libraryKey, name, timeoutMs?, resume?, connections?}`). Defaults to a 10 min timeout (also the ceiling on viewer/request paths; jobs, schedules, and background contexts may raise `timeoutMs` up to the calling job's remaining deadline). Set `resume: True` to continue a partial download when validators match, preserving progress per range. `connections` defaults to 1. Jobs, schedules, and background calls can request up to 8 concurrent ranges, subject to the host cap."""
        return self._call("POST", "/ether/plugin-host/data/http.fetchToCache", [req])

    fetchToCache = fetch_to_cache

    def fetch(self, req: Dict[str, Any]) -> Dict[str, Any]:
        method = str(req.get("method") or "GET").upper()
        url = str(req["url"])
        headers = dict(req.get("headers") or {})
        body_str = req.get("body")
        data = body_str.encode("utf-8") if isinstance(body_str, str) else None
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=min(15, max(0.001, float(req.get("timeoutMs", 15000)) / 1000))) as resp:
                raw = resp.read().decode("utf-8", errors="replace")
                out_headers = {k.lower(): v for k, v in resp.headers.items()}
                return {"status": resp.status, "headers": out_headers, "body": raw}
        except urllib.error.HTTPError as exc:
            raw = exc.read().decode("utf-8", errors="replace")
            out_headers = {k.lower(): v for k, v in exc.headers.items()} if exc.headers else {}
            return {"status": exc.code, "headers": out_headers, "body": raw}

    def fetch_many(
        self,
        requests: List[Dict[str, Any]],
        opts: Optional[Dict[str, Any]] = None,
    ) -> List[Dict[str, Any]]:
        """Run up to 64 outbound HTTP requests concurrently (bounded to 8 per batch and 6 per origin)."""
        return _run_fetch_many(requests, opts, self.fetch)

    fetchMany = fetch_many


class _CatalogClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def search(self, query: str, limit: int = 50, offset: int = 0) -> Dict[str, Any]:
        qs = urllib.parse.urlencode({"q": query, "limit": str(limit), "offset": str(offset)})
        res = self._call("GET", f"/ether/plugin-host/catalog/search?{qs}")
        return res if isinstance(res, dict) else {"titles": []}

    def get(self, title_id: str) -> Optional[Dict[str, Any]]:
        encoded = urllib.parse.quote(title_id, safe="")
        res = self._call("GET", f"/ether/plugin-host/catalog/{encoded}", allow_404=True)
        return res if isinstance(res, dict) else None


class _ViewersClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def history(self, viewer_id: int, limit: int = 50, offset: int = 0) -> List[Dict[str, Any]]:
        qs = urllib.parse.urlencode({"limit": str(limit), "offset": str(offset)})
        res = self._call("GET", f"/ether/plugin-host/viewers/{int(viewer_id)}/history?{qs}")
        return list(res) if isinstance(res, list) else []

    def set_watch_state(self, viewer_id: int, title_id: str, watched: bool) -> None:
        """Mark a catalogue movie/episode or an owned playable item ref."""
        self._call(
            "POST",
            "/ether/plugin-host/viewers/watch-state",
            {"viewerId": int(viewer_id), "titleId": str(title_id), "watched": bool(watched)},
        )

    setWatchState = set_watch_state


class _CollectionsClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def upsert(self, collection: Dict[str, Any]) -> None:
        self._call("POST", "/ether/plugin-host/collections/upsert", collection)


class _ScreensClient:
    """Pushes to open screens and surfaces (screens capability)."""

    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def invalidate(self) -> Dict[str, int]:
        """Open screens of this plugin read their sources again now."""
        return self._call("POST", "/ether/plugin-host/screens/invalidate", {}) or {"streams": 0}

    def put_records(self, records: List[Dict[str, Any]]) -> Dict[str, int]:
        """New values for records open screens show: ``[{type, id, value}]``, at most 100."""
        return self._call("POST", "/ether/plugin-host/screens/records", {"records": records}) or {"streams": 0}

    putRecords = put_records


def image(url: str) -> Dict[str, str]:
    """A picture on one of your permissions.network hosts, served through Ether's image proxy."""
    return {"$image": url}


def asset(path: str) -> Dict[str, str]:
    """A file in your plugin package."""
    return {"$image": "asset:" + path.lstrip("/")}


def link(url: str) -> Dict[str, str]:
    """An https page viewers may open on phones and the web."""
    return {"$link": url}


def record(type_: str, id_: str) -> Dict[str, Dict[str, str]]:
    """A reference to one of your records; send its value in ``records``."""
    return {"$record": {"type": type_, "id": id_}}


def event(feed: str, id_: str) -> Dict[str, Dict[str, str]]:
    """One of your live feed's events."""
    return {"$event": {"feed": feed, "id": id_}}


def entity(feed: str, kind: str, id_: str) -> Dict[str, Dict[str, str]]:
    """One of your live feed's entities (a team, a driver)."""
    return {"$entity": {"feed": feed, "kind": kind, "id": id_}}


class EventEnumOption(TypedDict, total=False):
    value: str
    label: str


class EventFieldMeta(TypedDict, total=False):
    name: str
    type: str
    label: str
    description: str
    unit: str
    enum: List[EventEnumOption]
    pii: bool
    secret: bool
    filterable: bool
    example: Any


class PublishedEventMeta(TypedDict, total=False):
    key: str
    label: str
    description: str
    version: int
    fields: List[EventFieldMeta]


class FlowActionParamMeta(TypedDict, total=False):
    key: str
    type: str
    label: str
    description: str
    required: bool
    secret: bool
    template: bool
    default: Any
    enum: List[EventEnumOption]
    unit: str
    picker: str


class FlowActionMeta(TypedDict, total=False):
    key: str
    label: str
    description: str
    icon: str
    route: str
    idempotent: bool
    sideEffect: bool
    timeout: str
    params: List[FlowActionParamMeta]
    outputs: List[EventFieldMeta]


class ViewerWatchEventPayload(TypedDict, total=False):
    """Payload of the viewer.watch.* event subscriptions."""

    viewerId: int
    username: str
    titleKey: str
    titleName: str
    mediaType: str
    watched: bool
    state: Literal["watched", "unwatched", "progress"]
    source: Literal["playback", "manual", "plugin", "import"]
    surface: str
    itemRef: str
    tmdbId: int
    tvdbId: int
    imdbId: str
    season: int
    episode: int
    positionMs: int


class EventCorrelation(TypedDict, total=False):
    key: str
    sessionId: str
    playId: str
    titleId: str
    viewerId: int
    channelId: str
    sourceId: str
    pluginId: str
    deviceId: str
    libraryId: str


class FlowActionRequest(TypedDict, total=False):
    key: str
    flowId: str
    runId: str
    nodeId: str
    idempotencyKey: str
    dryRun: bool
    params: Dict[str, Any]


class FlowActionResponse(TypedDict, total=False):
    output: Dict[str, Any]
    branch: str


class AIMessage(TypedDict):
    role: str
    content: str


class AICompleteRequest(TypedDict, total=False):
    messages: List[AIMessage]
    schema: Dict[str, Any]
    maxTokens: int
    temperature: float
    timeoutMs: int
    searchQuery: str


class AICompleteResponse(TypedDict, total=False):
    text: str
    json: Any


class MediaProbeResult(TypedDict):
    """Container metadata returned by Ether's `media.probe` host service."""

    durationSec: float
    container: str
    width: int
    height: int
    sizeBytes: int


class HostError(RuntimeError):
    """Structured error returned by Ether's plugin-host API, carrying `.code`, `.status`, and optional retry metadata."""

    def __init__(
        self,
        message: str,
        code: str = "",
        status: int = 0,
        retry_after_sec: Optional[int] = None,
        retry_at: Optional[str] = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.status = status
        self.retry_after_sec = retry_after_sec
        self.retry_at = retry_at


class _AIClient:
    """Client for Ether's owner-configured language model host service (`ai.complete`)."""

    def __init__(
        self,
        call_json: Callable[..., Any],
        default_viewer_token: Optional[Callable[[], str]] = None,
    ) -> None:
        self._call = call_json
        self._default_viewer_token = default_viewer_token

    def complete(
        self,
        req: AICompleteRequest,
        viewer_token: Optional[str] = None,
    ) -> AICompleteResponse:
        tok = (
            viewer_token
            if viewer_token is not None
            else (self._default_viewer_token() if self._default_viewer_token else "")
        ).strip()
        extra_headers = {HEADER_VIEWER_TOKEN: tok} if tok else None
        res = self._call(
            "POST",
            "/ether/plugin-host/ai/complete",
            dict(req),
            extra_headers=extra_headers,
        )
        return res if isinstance(res, dict) else {"text": ""}  # type: ignore[return-value]


class _MediaClient:
    """Client for Ether's container metadata probing host service (`media.probe`)."""

    def __init__(
        self,
        call_json: Callable[..., Any],
        default_viewer_token: Optional[Callable[[], str]] = None,
    ) -> None:
        self._call = call_json
        self._default_viewer_token = default_viewer_token

    def probe(
        self,
        source: Union[PlayableSource, Dict[str, Any]],
        viewer_token: Optional[str] = None,
    ) -> MediaProbeResult:
        tok = (
            viewer_token
            if viewer_token is not None
            else (self._default_viewer_token() if self._default_viewer_token else "")
        ).strip()
        extra_headers = {HEADER_VIEWER_TOKEN: tok} if tok else None
        res = self._call(
            "POST",
            "/ether/plugin-host/media/probe",
            dict(source),
            extra_headers=extra_headers,
        )
        return (
            res
            if isinstance(res, dict)
            else {"durationSec": 0.0, "container": "", "width": 0, "height": 0, "sizeBytes": 0}
        )  # type: ignore[return-value]


class _EventsClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def publish(
        self,
        key: str,
        payload: Optional[Dict[str, Any]] = None,
        correlation: Optional[EventCorrelation] = None,
    ) -> None:
        body: Dict[str, Any] = {
            "key": str(key),
            "payload": dict(payload) if payload is not None else {},
        }
        if correlation is not None:
            body["correlation"] = dict(correlation)
        self._call("POST", "/ether/plugin-host/events/publish", body)


class _NotifyClient:
    def __init__(self, call_json: Callable[..., Any]) -> None:
        self._call = call_json

    def owner(self, text: str) -> None:
        self._call("POST", "/ether/plugin-host/notify", {"text": str(text)})


def _check_part_index(part_index: Optional[int]) -> Optional[int]:
    if part_index is None:
        return None
    if isinstance(part_index, bool) or not isinstance(part_index, int) or part_index < 0:
        raise ValueError("stream url: part_index must be a non-negative integer")
    return part_index


def _check_stream_source(source: Optional[str]) -> Optional[str]:
    if source is None or source == "":
        return None
    if not isinstance(source, str):
        raise ValueError("stream url: source must be a string")
    if source.strip() == "":
        raise ValueError("stream url: source must not be blank")
    return source


class _FrontendClient:
    def __init__(
        self,
        call_json: Callable[..., Any],
        default_viewer_token: Optional[Callable[[], str]] = None,
    ) -> None:
        self._call = call_json
        self._default_viewer_token = default_viewer_token

    def stream_url(
        self,
        viewer_id: int,
        item_ref: str,
        part_index: Optional[int] = None,
        source: Optional[str] = None,
        absolute: bool = False,
        viewer_token: Optional[str] = None,
    ) -> str:
        checked = _check_part_index(part_index)
        checked_source = _check_stream_source(source)
        if not isinstance(absolute, bool):
            raise ValueError("stream url: absolute must be a bool")
        payload: Dict[str, Any] = {"viewerId": int(viewer_id), "itemRef": str(item_ref)}
        if checked is not None:
            payload["partIndex"] = checked
        if checked_source is not None:
            payload["source"] = checked_source
        if absolute:
            payload["absolute"] = True
        tok = (
            viewer_token
            if viewer_token is not None
            else (self._default_viewer_token() if self._default_viewer_token else "")
        ).strip()
        if tok:
            result = self._call(
                "POST",
                "/ether/plugin-host/frontend/stream-url",
                payload,
                extra_headers={HEADER_VIEWER_TOKEN: tok},
            )
        else:
            result = self._call(
                "POST",
                "/ether/plugin-host/frontend/stream-url",
                payload,
            )
        return str((result or {}).get("url") or "")


class RemoteHostClient:
    """Host client for remote/sidecar plugins talking to `/ether/plugin-host/*`."""

    def __init__(
        self,
        base_url: Optional[str] = None,
        credential: Optional[str] = None,
        viewer_token: Optional[str] = None,
    ) -> None:
        raw_base = base_url or os.environ.get("ETHER_HOST_URL") or "http://127.0.0.1:32450"
        self.base_url = raw_base.rstrip("/")
        self.credential = (
            credential if credential is not None else os.environ.get("ETHER_PLUGIN_CREDENTIAL", "")
        )
        self.viewer_token = (viewer_token or "").strip()
        self.kv = _KVClient(self._call_json)
        self.badges = _BadgesClient(self._call_json)
        self.secret = _SecretClient(self._call_json)
        self.http = _HttpClient(self._call_json)
        self.catalog = _CatalogClient(self._call_json)
        self.viewers = _ViewersClient(self._call_json)
        self.collections = _CollectionsClient(self._call_json)
        self.screens = _ScreensClient(self._call_json)
        self.events = _EventsClient(self._call_json)
        self.ai = _AIClient(self._call_json, lambda: self.viewer_token)
        self.media = _MediaClient(self._call_json, lambda: self.viewer_token)
        self.notify = _NotifyClient(self._call_json)
        self.kinds = _KindsClient(self._call_json, lambda: self.viewer_token)
        self.runtime = _RuntimeClient(self._call_json)
        self.cache = self._Cache(self)
        self.archive = _ArchiveClient(self._call_json)
        self.frontend = _FrontendClient(self._call_json, lambda: self.viewer_token)

    class _Cache:
        def __init__(self, parent: "RemoteHostClient") -> None:
            self._p = parent

        def _request(self, method: str, path: str, data: Optional[bytes] = None) -> bytes:
            req = urllib.request.Request(
                f"{self._p.base_url}{path}", data=data, method=method,
                headers={"X-Ether-Plugin-Credential": self._p.credential},
            )
            with urllib.request.urlopen(req, timeout=30) as resp:
                return resp.read()

        def put(self, library_key: str, name: str, data: bytes) -> str:
            q = urllib.parse.urlencode({"name": name})
            raw = self._request("POST", f"/ether/plugin-host/cache/{urllib.parse.quote(library_key)}?{q}", data)
            return str(json.loads(raw or b"{}").get("blob") or "")

        def open(self, blob_ref: str) -> bytes:  # noqa: A002 - mirrors host naming
            return self._request("GET", "/ether/plugin-host/cache/" + str(blob_ref).lstrip("/"))

        def read(self, blob_ref: str, data_range: DataRange) -> bytes:
            norm = _check_data_range(data_range)
            res = self._p._call_json("POST", "/ether/plugin-host/data/cache.read", [str(blob_ref), norm])
            return base64.b64decode((res or {}).get("bodyBase64") or "")

        def rasterize(
            self,
            library_key: str,
            name: str,
            blob_ref: str,
            options: Optional[RasterizeOptions] = None,
        ) -> RasterizeResult:
            args: List[Any] = [str(library_key), str(name), str(blob_ref)]
            norm = _check_rasterize_options(options)
            if norm is not None:
                args.append(norm)
            res = self._p._call_json("POST", "/ether/plugin-host/data/cache.rasterize", args)
            return res if isinstance(res, dict) else {}  # type: ignore[return-value]

        def delete(self, blob_ref: str) -> None:
            encoded = urllib.parse.quote(str(blob_ref), safe=":")
            self._p._call_json("DELETE", f"/ether/plugin-host/cache/{encoded}")

    def _call_json(
        self,
        method: str,
        path: str,
        body: Optional[Any] = None,
        allow_404: bool = False,
        extra_headers: Optional[Dict[str, str]] = None,
    ) -> Any:
        headers: Dict[str, str] = {
            "X-Ether-Plugin-Credential": self.credential,
            "Accept": "application/json",
        }
        if extra_headers:
            for k, v in extra_headers.items():
                if v:
                    headers[str(k)] = str(v)
        data: Optional[bytes] = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            f"{self.base_url}{path}",
            data=data,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                raw = resp.read().decode("utf-8")
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as exc:
            if allow_404 and exc.code == 404:
                return None
            raw = exc.read().decode("utf-8", errors="replace")
            code = ""
            retry_after_sec: Optional[int] = None
            retry_at: Optional[str] = None
            hdr_ra = exc.headers.get("Retry-After") if exc.headers is not None else None
            if hdr_ra and hdr_ra.strip().isdigit():
                retry_after_sec = int(hdr_ra.strip())
            try:
                parsed = json.loads(raw)
                err_obj = parsed.get("error", {}) if isinstance(parsed, dict) else {}
                msg = err_obj.get("message") if isinstance(err_obj, dict) else None
                code = str(err_obj.get("code") or "") if isinstance(err_obj, dict) else ""
                if isinstance(err_obj, dict):
                    if isinstance(err_obj.get("retryAfterSec"), (int, float)) and err_obj["retryAfterSec"] > 0:
                        retry_after_sec = int(err_obj["retryAfterSec"])
                    if isinstance(err_obj.get("retryAt"), str) and err_obj["retryAt"]:
                        retry_at = err_obj["retryAt"]
            except Exception:
                msg = None
            raise HostError(
                msg or f"Host call {method} {path} failed with HTTP {exc.code}",
                code=code,
                status=int(exc.code),
                retry_after_sec=retry_after_sec,
                retry_at=retry_at,
            ) from exc

    def settings_get(self) -> Dict[str, Any]:
        result = self._call_json("GET", "/ether/plugin-host/settings")
        return result.get("settings", {})

    def log(
        self,
        level: str,
        message: str,
        attrs: Optional[Dict[str, Any]] = None,
    ) -> None:
        payload: Dict[str, Any] = {"level": str(level), "message": str(message)}
        if attrs is not None:
            payload["attrs"] = attrs
        self._call_json("POST", "/ether/plugin-host/log", payload)

    def abort(self, message: str = "aborted") -> None:
        raise RuntimeError(message)


def create_remote_host_client(
    base_url: Optional[str] = None,
    credential: Optional[str] = None,
    viewer_token: Optional[str] = None,
) -> RemoteHostClient:
    return RemoteHostClient(base_url=base_url, credential=credential, viewer_token=viewer_token)


# ---------------------------------------------------------------------------
# Plugin context & router
# ---------------------------------------------------------------------------


@dataclass
class PluginContext:
    manifest: Dict[str, Any]
    settings: Dict[str, Any]
    locale: str
    pick_token: Optional[str]
    request_id: Optional[str]
    headers: Dict[str, Any]
    query: Dict[str, str]
    ether: Any
    i18n: I18nHelper

    @property
    def pickToken(self) -> Optional[str]:
        return self.pick_token

    @property
    def requestId(self) -> Optional[str]:
        return self.request_id

    def t(self, key: str, params: Optional[Dict[str, Any]] = None) -> str:
        return self.i18n.t(key, params=params, locale=self.locale)

    def stream_url(
        self,
        item_ref: str,
        part_index: Optional[int] = None,
        source: Optional[str] = None,
        absolute: bool = False,
    ) -> str:
        """Mint a scoped Ether stream URL for one of this plugin's own kind
        items (spec §6.5), optionally scoped to a zero-based part (#576), a
        resolver PlayableSource.id (#661), and/or resolved to an absolute URL
        using the active request origin or the adapter's first published
        address (#772). The viewer is the one Ether authenticated on this
        request (X-Ether-Viewer); bytes are served by Ether, not the plugin."""
        if self.ether is None:
            raise RuntimeError("stream url: no host service available")
        checked = _check_part_index(part_index)
        checked_source = _check_stream_source(source)
        if not isinstance(absolute, bool):
            raise ValueError("stream url: absolute must be a bool")
        raw = _get_header(self.headers, "X-Ether-Viewer")
        try:
            viewer_id = int(str(raw).strip()) if raw is not None else 0
        except (TypeError, ValueError):
            viewer_id = 0
        if viewer_id <= 0:
            raise RuntimeError("stream url: no authenticated viewer on this request")
        kwargs: Dict[str, Any] = {}
        if checked_source is not None:
            kwargs["source"] = checked_source
        if absolute:
            kwargs["absolute"] = True
        if kwargs:
            return self.ether.frontend.stream_url(
                viewer_id, str(item_ref), checked, **kwargs
            )
        if checked is None:
            return self.ether.frontend.stream_url(viewer_id, str(item_ref))
        return self.ether.frontend.stream_url(viewer_id, str(item_ref), checked)

    def _viewer_token(self) -> str:
        raw = _get_header(self.headers, HEADER_VIEWER_TOKEN)
        return str(raw).strip() if raw is not None else ""

    def kinds_visible(self, kind: str, ids: List[str]) -> List[str]:
        """Evaluate the library's Rules and viewer access policy for a bounded
        batch (at most 500) of external IDs in `kind` (#615). Only frontend
        adapter requests carrying Ether's authenticated viewer token may call it."""
        if self.ether is None or getattr(self.ether, "kinds", None) is None:
            raise RuntimeError("kinds.visible: no host service available")
        tok = self._viewer_token()
        if not tok:
            raise RuntimeError("kinds.visible: no authenticated frontend adapter viewer on this request")
        return self.ether.kinds.visible(str(kind), list(ids), viewer_token=tok)

    def kinds_browse(
        self,
        kind: str,
        query: Optional[Union[KindBrowseQuery, Dict[str, Any]]] = None,
    ) -> Dict[str, Any]:
        """Browse or search `kind` with the library's Rules and viewer access
        policy applied for the requesting frontend-adapter viewer (#615). Only
        frontend adapter requests carrying Ether's authenticated viewer token
        may call it."""
        if self.ether is None or getattr(self.ether, "kinds", None) is None:
            raise RuntimeError("kinds.browse: no host service available")
        tok = self._viewer_token()
        if not tok:
            raise RuntimeError("kinds.browse: no authenticated frontend adapter viewer on this request")
        return self.ether.kinds.browse(str(kind), dict(query or {}), viewer_token=tok)

    kindsVisible = kinds_visible
    kindsBrowse = kinds_browse


def _get_header(headers: Optional[Dict[str, Any]], name: str) -> Optional[str]:
    if not headers:
        return None
    target = name.lower()
    for k, v in headers.items():
        if k.lower() == target:
            if isinstance(v, list):
                return str(v[0]) if v else None
            if v is not None:
                return str(v)
    return None


def _parse_url_path_and_query(
    raw_path: str,
    raw_query: Optional[Dict[str, Any]] = None,
) -> Tuple[str, Dict[str, str]]:
    path_only = raw_path or "/"
    query: Dict[str, str] = {}
    if isinstance(raw_query, dict):
        for k, v in raw_query.items():
            if isinstance(v, list):
                if v:
                    query[str(k)] = str(v[0])
            elif v is not None:
                query[str(k)] = str(v)
    if "?" in path_only:
        path_only, qs = path_only.split("?", 1)
        path_only = path_only or "/"
        for k, vals in urllib.parse.parse_qs(qs, keep_blank_values=True).items():
            if k not in query and vals:
                query[k] = vals[0]
    if not path_only.startswith("/"):
        path_only = "/" + path_only
    return path_only, query


def _parse_json_body(body: Any) -> Dict[str, Any]:
    if body is None or body == "" or body == b"":
        return {}
    if isinstance(body, (bytes, bytearray)):
        body = body.decode("utf-8")
    if isinstance(body, str):
        trimmed = body.strip()
        if not trimmed:
            return {}
        parsed = json.loads(trimmed)
        return parsed if isinstance(parsed, dict) else {}
    if isinstance(body, dict):
        return body
    return {}


def _ok_response(
    body: Any,
    headers: Optional[Dict[str, str]] = None,
    status: int = 200,
) -> Dict[str, Any]:
    merged = {"Content-Type": "application/json"}
    if headers:
        merged.update(headers)
    return {"status": status, "headers": merged, "body": body}


def _error_response(status: int, code: str, message: str) -> Dict[str, Any]:
    return {
        "status": status,
        "headers": {"Content-Type": "application/json"},
        "body": {"error": {"code": code, "message": message}},
    }


def _source_search_request(query: Dict[str, str]) -> Dict[str, Any]:
    """Decode GET /ether/search: query, category (comma-separated Newznab ids),
    purpose, lookup and lookup.<field> (stream_source category searches)."""
    cats = []
    for raw in (query.get("category") or "").split(","):
        raw = raw.strip()
        if raw.isdigit():
            cats.append(int(raw))
    req: Dict[str, Any] = {"query": query.get("query", ""), "categories": cats}
    if query.get("purpose"):
        req["purpose"] = query["purpose"]
    if query.get("lookup"):
        fields = {k[len("lookup.") :]: v for k, v in query.items() if k.startswith("lookup.") and len(k) > len("lookup.")}
        req["lookup"] = {"type": query["lookup"], "fields": fields}
    return req


def _match_stremio_route(path: str, query: Dict[str, str]) -> Optional[Dict[str, Any]]:
    if not path.endswith(".json"):
        return None
    trimmed = path[1:-5]
    parts = trimmed.split("/")
    if len(parts) < 3 or len(parts) > 4:
        return None
    resource = parts[0]
    if resource not in ("catalog", "meta", "stream", "subtitles"):
        return None
    media_type = urllib.parse.unquote(parts[1])
    item_id = urllib.parse.unquote(parts[2])
    extra: Dict[str, str] = {}
    if len(parts) == 4 and parts[3]:
        raw_extra = parts[3]
        for k, vals in urllib.parse.parse_qs(raw_extra, keep_blank_values=True).items():
            if vals:
                extra[k] = vals[0]
    for k, v in query.items():
        if k not in extra:
            extra[k] = v
    return {
        "resource": resource,
        "type": media_type,
        "id": item_id,
        "extra": extra,
    }


class PluginInstance:
    """Runnable Ether plugin instance returned by `plugin(...)`."""

    def __init__(
        self,
        manifest: Dict[str, Any],
        routes: Optional[Dict[str, Callable[..., Any]]] = None,
        default_settings: Optional[Dict[str, Any]] = None,
        strings: Optional[Dict[str, Dict[str, str]]] = None,
        default_locale: str = "en",
        host: Optional[Any] = None,
    ) -> None:
        if not isinstance(manifest, dict):
            raise ValueError("Plugin manifest is required.")
        if not manifest.get("id") or not manifest.get("name") or not manifest.get("version"):
            raise ValueError("Plugin manifest requires id, name, and version.")

        self.manifest = manifest
        self.routes: Dict[str, Callable[..., Any]] = dict(routes or {})
        self._default_settings = dict(default_settings or {})
        self._default_locale = default_locale or "en"
        self._host = host
        self._settings_failure_logged = False

        inline_strings: Dict[str, Dict[str, str]] = {}
        ether_block = manifest.get("ether")
        if isinstance(ether_block, dict) and isinstance(ether_block.get("strings"), dict):
            for loc, val in ether_block["strings"].items():
                if isinstance(val, dict):
                    inline_strings[loc] = dict(val)
        if strings:
            for loc, table in strings.items():
                inline_strings.setdefault(loc, {}).update(table)
        self.i18n = create_i18n(inline_strings, self._default_locale)

        self._settings_schema = (
            ether_block.get("settings") if isinstance(ether_block, dict) else None
        )
        self._current_settings, _ = decode_settings(
            self._settings_schema,
            None,
            self._default_settings,
        )

        self._page_routes: Dict[str, str] = {}
        self._flow_action_routes: Dict[str, str] = {}
        if isinstance(ether_block, dict):
            contributes = ether_block.get("contributes")
            if isinstance(contributes, dict):
                for p in contributes.get("pages") or []:
                    if isinstance(p, dict) and p.get("route") and p.get("key"):
                        self._page_routes[str(p["route"])] = str(p["key"])
                for s in contributes.get("slots") or []:
                    if isinstance(s, dict) and s.get("route") and s.get("key"):
                        self._page_routes[str(s["route"])] = str(s["key"])
                for fa in contributes.get("flowActions") or []:
                    if isinstance(fa, dict) and fa.get("route") and fa.get("key"):
                        self._flow_action_routes[str(fa["route"])] = str(fa["key"])

    def get_settings(self) -> Dict[str, Any]:
        return dict(self._current_settings)

    def set_settings(
        self, incoming: Dict[str, Any]
    ) -> Tuple[Dict[str, Any], Dict[str, str]]:
        decoded, errors = decode_settings(
            self._settings_schema,
            incoming,
            self._default_settings,
        )
        if not errors:
            self._current_settings = decoded
        return decoded, errors

    def _build_context(self, req: Dict[str, Any]) -> PluginContext:
        _, query = _parse_url_path_and_query(
            str(req.get("path") or "/"),
            req.get("query"),
        )
        headers = req.get("headers") or {}
        locale_raw = (
            req.get("locale")
            or query.get("locale")
            or _get_header(headers, "X-Ether-Locale")
            or _get_header(headers, "Accept-Language")
            or self._default_locale
        )
        locale = str(locale_raw).split(",")[0].strip() or "en"
        pick_token = _get_header(headers, "X-Ether-Pick")
        request_id = _get_header(headers, "X-Ether-Request-ID")
        viewer_token = _get_header(headers, HEADER_VIEWER_TOKEN)
        host_client = (
            self._host
            if self._host is not None
            else create_remote_host_client(viewer_token=viewer_token)
        )
        settings = dict(self._current_settings)
        getter = getattr(host_client, "settings_get", None)
        if callable(getter) and str(req.get("path", "")) != "/ether/configure" and (not isinstance(host_client, RemoteHostClient) or host_client.credential):
            try:
                fresh = getter()
                for field in collect_setting_fields(self._settings_schema):
                    key = field.get("key")
                    if field.get("kind") == "secret" and key in self._current_settings:
                        fresh[key] = self._current_settings[key]
                settings, _ = decode_settings(self._settings_schema, fresh, self._default_settings)
            except Exception:
                if not self._settings_failure_logged:
                    logging.getLogger(__name__).warning("plugin settings lookup failed")
                    self._settings_failure_logged = True
                settings = {}
        return PluginContext(
            manifest=self.manifest,
            settings=settings,
            locale=locale,
            pick_token=pick_token,
            request_id=request_id,
            headers=headers,
            query=query,
            ether=host_client,
            i18n=self.i18n,
        )

    def handle(self, req: Dict[str, Any]) -> Dict[str, Any]:
        try:
            ctx = self._build_context(req)
            return self._dispatch_core(req, ctx)
        except PluginHttpError as exc:
            return _error_response(exc.status, exc.code, exc.message)
        except Exception as exc:
            return _error_response(500, "internal_error", str(exc) or "Internal plugin error.")

    def dispatch(self, req: Dict[str, Any]) -> Dict[str, Any]:
        return self.handle(req)

    def rank_key(
        self,
        candidate: Dict[str, Any],
        config: Optional[Dict[str, Any]] = None,
    ) -> float:
        fn = self.routes.get("rank_key") or self.routes.get("rankKey")
        if not fn:
            return 0
        cfg = config if config is not None else self._current_settings
        return float(fn(candidate, cfg) or 0)

    def cond_match(
        self,
        candidate: Dict[str, Any],
        operator: str = "eq",
        value: Any = None,
    ) -> bool:
        fn = self.routes.get("cond_match") or self.routes.get("condMatch")
        if not fn:
            return False
        return bool(fn(candidate, operator, value))

    def _dispatch_core(self, req: Dict[str, Any], ctx: PluginContext) -> Dict[str, Any]:
        method = str(req.get("method") or "GET").upper()
        path, query = _parse_url_path_and_query(
            str(req.get("path") or "/"),
            req.get("query"),
        )

        if method == "GET" and path == "/manifest.json":
            return _ok_response(self.manifest)

        if method == "GET" and path == "/ether/health":
            fn = self.routes.get("health")
            if fn:
                return _ok_response(fn(ctx))
            return _ok_response({"status": "ok"})

        if method == "POST" and path == "/ether/subtitles/download":
            if not ctx.pick_token or not ctx.pick_token.strip():
                return _error_response(403, "pick_required", "Subtitle downloads require an X-Ether-Pick token.")
            fn = self.routes.get("subtitle_download") or self.routes.get("subtitleDownload")
            if not fn:
                return _error_response(404, "not_found", "Subtitle download handler is not registered.")
            try:
                body_obj = _parse_json_body(req.get("body"))
            except Exception:
                return _error_response(400, "invalid_json", "Request body must be valid JSON.")
            return _ok_response(fn(body_obj, ctx))

        if method == "POST" and path == "/ether/configure":
            try:
                body_obj = _parse_json_body(req.get("body"))
            except Exception:
                return _error_response(400, "invalid_json", "Request body must be valid JSON.")
            incoming = (
                body_obj["settings"]
                if isinstance(body_obj.get("settings"), dict)
                else body_obj
            )
            decoded, errors = decode_settings(
                self._settings_schema,
                incoming,
                self._default_settings,
            )
            if errors:
                return _ok_response({"errors": errors})
            fn = self.routes.get("configure")
            if fn:
                ctx.settings = dict(decoded)
                res = fn(decoded, ctx) or {}
                custom_errors = res.get("errors") if isinstance(res, dict) else {}
                if not custom_errors:
                    self._current_settings = decoded
                return _ok_response({"errors": custom_errors or {}})
            self._current_settings = decoded
            return _ok_response({"errors": {}})

        if method == "POST" and path == "/ether/test":
            fn = self.routes.get("test")
            if fn:
                return _ok_response(fn(ctx))
            return _ok_response({"ok": True})

        if method == "GET" and path.startswith("/ether/options/"):
            field_name = urllib.parse.unquote(path[len("/ether/options/") :])
            fn = self.routes.get("options")
            if not fn:
                return _ok_response([])
            return _ok_response(fn(field_name, ctx))

        if method == "POST" and path == "/ether/stream/open":
            fn = self.routes.get("open_stream") or self.routes.get("openStream")
            if not fn:
                return _error_response(404, "not_found", "Stream open handler is not registered.")
            body_obj = _parse_json_body(req.get("body"))
            return _ok_response(fn({"streamId": str(body_obj.get("streamId") or "")}, ctx))

        if method == "GET" and path.startswith("/ether/open/"):
            fn = self.routes.get("open_stream") or self.routes.get("openStream")
            if not fn:
                return _error_response(404, "not_found", "Stream open handler is not registered.")
            open_req: Dict[str, Any] = {"streamId": urllib.parse.unquote(path[len("/ether/open/") :])}
            if query.get("file"):
                open_req["file"] = query["file"]
            raw_index = query.get("fileIndex", "")
            if raw_index.isdigit():
                open_req["fileIndex"] = int(raw_index)
            return _ok_response(fn(open_req, ctx))

        if method == "GET" and path == "/ether/search":
            fn = self.routes.get("source_search") or self.routes.get("sourceSearch")
            if not fn:
                return _error_response(404, "not_found", "Source search handler is not registered.")
            res = fn(_source_search_request(query), ctx) or {}
            if isinstance(res, dict) and res.get("streams") is None:
                res = {**res, "streams": []}
            return _ok_response(res)

        if method == "GET" and path.startswith("/ether/files/"):
            fn = self.routes.get("release_files") or self.routes.get("releaseFiles")
            if not fn:
                return _error_response(404, "not_found", "Release files handler is not registered.")
            res = fn({"id": urllib.parse.unquote(path[len("/ether/files/") :])}, ctx) or {}
            if isinstance(res, dict) and res.get("files") is None:
                res = {**res, "files": []}
            return _ok_response(res)

        if method == "GET" and path.startswith("/ether/markers/") and path.endswith(".json"):
            fn = self.routes.get("markers")
            if not fn:
                return _error_response(404, "not_found", "Markers handler is not registered.")
            trimmed = path[len("/ether/markers/") : -5]
            parts = trimmed.split("/")
            if len(parts) < 2:
                return _error_response(400, "invalid_path", "Markers route requires type and id.")
            return _ok_response(
                fn(
                    {
                        "type": urllib.parse.unquote(parts[0]),
                        "id": urllib.parse.unquote("/".join(parts[1:])),
                    },
                    ctx,
                )
            )

        if method == "GET" and path == "/ether/channels.json":
            fn = self.routes.get("channels")
            if not fn:
                return _error_response(404, "not_found", "Channels handler is not registered.")
            return _ok_response(fn(ctx))

        if method == "GET" and path.startswith("/ether/guide/") and path.endswith(".json"):
            fn = self.routes.get("guide")
            if not fn:
                return _error_response(404, "not_found", "Guide handler is not registered.")
            channel_id = urllib.parse.unquote(path[len("/ether/guide/") : -5])
            return _ok_response(
                fn(
                    {
                        "channelId": channel_id,
                        "from": query.get("from"),
                        "to": query.get("to"),
                    },
                    ctx,
                )
            )

        if method == "POST" and path == "/ether/notify":
            fn = self.routes.get("notify")
            if not fn:
                return _error_response(404, "not_found", "Notify handler is not registered.")
            body_obj = _parse_json_body(req.get("body"))
            return _ok_response(
                fn(
                    {
                        "title": body_obj.get("title"),
                        "message": str(body_obj.get("message") or body_obj.get("text") or ""),
                        "event": body_obj.get("event"),
                        "settings": body_obj.get("settings"),
                    },
                    ctx,
                )
            )

        if method == "POST" and path == "/ether/event":
            fn = self.routes.get("event")
            if not fn:
                return _error_response(404, "not_found", "Event handler is not registered.")
            body_obj = _parse_json_body(req.get("body"))
            events = body_obj.get("events") if isinstance(body_obj.get("events"), list) else []
            res = fn(events, ctx)
            if isinstance(res, dict) and isinstance(res.get("ack"), list):
                return _ok_response({"ack": res["ack"]})
            return _ok_response({"ack": [e.get("id") for e in events if isinstance(e, dict)]})

        if method == "GET" and (
            path == "/ether/view"
            or path.startswith("/ether/view/")
            or path in self._page_routes
        ):
            fn = self.routes.get("view")
            if not fn:
                return _error_response(404, "not_found", "View handler is not registered.")
            page = self._page_routes.get(path)
            if not page:
                page = (
                    "home"
                    if path == "/ether/view"
                    else urllib.parse.unquote(path[len("/ether/view/") :])
                )
            return _ok_response(fn({"page": page, "query": query}, ctx))

        if method == "POST" and path.startswith("/ether/action/"):
            fn = self.routes.get("action")
            if not fn:
                return _error_response(404, "not_found", "Action handler is not registered.")
            action_name = urllib.parse.unquote(path[len("/ether/action/") :])
            input_obj = _parse_json_body(req.get("body"))
            return _ok_response(fn({"action": action_name, "input": input_obj}, ctx))

        if method == "POST":
            flow_key = self._flow_action_routes.get(path)
            if not flow_key and path.startswith("/ether/v1/flow-actions/"):
                flow_key = urllib.parse.unquote(path[len("/ether/v1/flow-actions/") :])
            if flow_key:
                fn = self.routes.get("flow_action") or self.routes.get("flowAction")
                if not fn:
                    return _error_response(
                        404, "not_found", "Flow action handler is not registered."
                    )
                body_obj = _parse_json_body(req.get("body"))
                header_idem = _get_header(req.get("headers"), HEADER_IDEMPOTENCY_KEY) or ""
                params_obj = body_obj.get("params")
                if not isinstance(params_obj, dict):
                    params_obj = {}
                fa_req: FlowActionRequest = {
                    "key": flow_key,
                    "flowId": str(body_obj.get("flowId") or ""),
                    "runId": str(body_obj.get("runId") or ""),
                    "nodeId": str(body_obj.get("nodeId") or ""),
                    "idempotencyKey": str(header_idem or body_obj.get("idempotencyKey") or ""),
                    "dryRun": bool(body_obj.get("dryRun", False)),
                    "params": params_obj,
                }
                return _ok_response(fn(fa_req, ctx))

        if method == "POST" and path.startswith("/ether/tool/"):
            fn = self.routes.get("tool")
            if not fn:
                return _error_response(404, "not_found", "Tool handler is not registered.")
            tool_name = urllib.parse.unquote(path[len("/ether/tool/") :])
            input_obj = _parse_json_body(req.get("body"))
            return _ok_response(fn({"name": tool_name, "input": input_obj}, ctx))

        if method == "POST" and path.startswith("/ether/job/"):
            fn = self.routes.get("job")
            if not fn:
                return _error_response(404, "not_found", "Job handler is not registered.")
            job_name = urllib.parse.unquote(path[len("/ether/job/") :])
            return _ok_response(fn({
                "name": job_name,
                "continuation": _parse_json_body(req.get("body")).get("continuation"),
            }, ctx))

        if method == "POST" and path == "/ether/rank-key":
            fn = self.routes.get("rank_key") or self.routes.get("rankKey")
            if not fn:
                return _error_response(404, "not_found", "Rank key handler is not registered.")
            body_obj = _parse_json_body(req.get("body"))
            candidate = (
                body_obj["candidate"]
                if isinstance(body_obj.get("candidate"), dict)
                else body_obj
            )
            cfg = (
                body_obj["config"]
                if isinstance(body_obj.get("config"), dict)
                else self._current_settings
            )
            score = float(fn(candidate, cfg) or 0)
            return _ok_response({"score": int(score) if score.is_integer() else score})

        if method == "POST" and path == "/ether/cond-match":
            fn = self.routes.get("cond_match") or self.routes.get("condMatch")
            if not fn:
                return _error_response(
                    404, "not_found", "Condition match handler is not registered."
                )
            body_obj = _parse_json_body(req.get("body"))
            candidate = (
                body_obj["candidate"]
                if isinstance(body_obj.get("candidate"), dict)
                else body_obj
            )
            operator = str(body_obj.get("operator") or "eq")
            value = body_obj.get("value")
            return _ok_response({"match": bool(fn(candidate, operator, value))})

        if method in ("GET", "HEAD") and path.startswith("/ether/file/"):
            fn = self.routes.get("file")
            if not fn:
                return _error_response(404, "not_found", "File handler is not registered.")
            rel_path = urllib.parse.unquote(path[len("/ether/file/") :])
            if not rel_path or rel_path.startswith("/") or ".." in rel_path.split("/"):
                return _error_response(
                    400, "invalid_path", "File path must be a clean relative path."
                )
            range_header = _get_header(req.get("headers"), "Range")
            file_res = fn(
                {"path": rel_path, "range": range_header, "headOnly": method == "HEAD"},
                ctx,
            )
            headers: Dict[str, str] = {
                "Content-Type": file_res.get("contentType") or "application/octet-stream",
                "Accept-Ranges": "bytes",
            }
            if isinstance(file_res.get("headers"), dict):
                headers.update(file_res["headers"])
            content_range = file_res.get("contentRange")
            if content_range:
                headers["Content-Range"] = str(content_range)
            elif file_res.get("totalSize") is not None:
                headers["Content-Length"] = str(file_res["totalSize"])
            status = int(file_res.get("status") or (206 if content_range else 200))
            raw_body = file_res.get("body", b"")
            if isinstance(raw_body, str):
                raw_body = raw_body.encode("utf-8")
            return {
                "status": status,
                "headers": headers,
                "body": b"" if method == "HEAD" else bytes(raw_body),
            }

        if method == "GET":
            stremio = _match_stremio_route(path, query)
            if stremio:
                resource = stremio["resource"]
                if resource == "subtitles":
                    fn = self.routes.get("subtitles")
                    if not fn:
                        return _error_response(
                            404, "not_found", "Subtitles handler is not registered."
                        )
                    extra = stremio["extra"]
                    video_size = (
                        int(extra["videoSize"])
                        if "videoSize" in extra and str(extra["videoSize"]).isdigit()
                        else None
                    )
                    return _ok_response(
                        fn(
                            {
                                "type": stremio["type"],
                                "id": stremio["id"],
                                "extra": extra,
                                "pickToken": ctx.pick_token,
                                "videoHash": extra.get("videoHash"),
                                "videoSize": video_size,
                                "filename": extra.get("filename"),
                            },
                            ctx,
                        )
                    )

                if resource == "catalog":
                    fn = self.routes.get("catalog")
                    if not fn:
                        return _error_response(
                            404, "not_found", "Catalog handler is not registered."
                        )
                    extra = stremio["extra"]
                    skip = (
                        int(extra["skip"])
                        if "skip" in extra and str(extra["skip"]).isdigit()
                        else 0
                    )
                    return _ok_response(
                        fn(
                            {
                                "type": stremio["type"],
                                "id": stremio["id"],
                                "extra": extra,
                                "skip": skip,
                                "search": extra.get("search"),
                                "genre": extra.get("genre"),
                            },
                            ctx,
                        )
                    )

                if resource == "meta":
                    fn = self.routes.get("meta")
                    if not fn:
                        return _error_response(
                            404, "not_found", "Meta handler is not registered."
                        )
                    return _ok_response(
                        fn({"type": stremio["type"], "id": stremio["id"]}, ctx)
                    )

                if resource == "stream":
                    fn = self.routes.get("stream")
                    if not fn:
                        return _error_response(
                            404, "not_found", "Stream handler is not registered."
                        )
                    return _ok_response(
                        fn(
                            {
                                "type": stremio["type"],
                                "id": stremio["id"],
                                "extra": stremio["extra"],
                            },
                            ctx,
                        )
                    )

        kinds_resp = self._dispatch_kinds(method, path, req, ctx)
        if kinds_resp is not None:
            return kinds_resp

        return _error_response(404, "not_found", f"No route matched {method} {path}.")

    # -- Content kinds (Ether v0.3.8) -------------------------------------

    def on_catalog(self, kind: str, provider: Any) -> None:
        self._kind_catalog()[kind] = provider

    def on_resolve(self, kind: str, resolver: Any) -> None:
        self._kind_resolve()[kind] = resolver

    def on_match(self, kind: str, matcher: Any) -> None:
        self._kind_match()[kind] = matcher

    def on_live(self, kind: str, provider: Any) -> None:
        self._kind_live()[kind] = provider

    def on_frontend(self, key: str, adapter: Any) -> None:
        self._kind_frontend()[key] = adapter

    def on_setup_test(self, tester: Any) -> None:
        self._setup_tester = tester

    def on_screen_source(self, screen: str, route: str, handler: Callable[..., Any]) -> None:
        """Answer a document's ``plugin`` source; ``screen`` may be ``"*"``.

        ``handler(ctx, req)`` gets ``{screen, route, page, args, locale, tz}``
        and returns ``{value, records?, maxAgeSec?, nextRefreshSec?}``. Wrap
        images, links, records, events and entities with :func:`image`,
        :func:`asset`, :func:`link`, :func:`record`, :func:`event` and
        :func:`entity`; Ether drops bare URL strings.
        """
        self._screen_sources()[f"{screen}/{route}"] = handler

    def on_screen_action(self, screen: str, action: str, handler: Callable[..., Any]) -> None:
        """Run a manifest-declared screen action.

        ``handler(ctx, req)`` gets ``{screen, action, page, args, params,
        locale, tz}`` and returns ``{records?, sources?, refresh?, toast?,
        navigate?}``.
        """
        self._screen_actions()[f"{screen}/{action}"] = handler

    def _screen_sources(self) -> Dict[str, Any]:
        return self.__dict__.setdefault("_screen_source_handlers", {})

    def _screen_actions(self) -> Dict[str, Any]:
        return self.__dict__.setdefault("_screen_action_handlers", {})

    onCatalog = on_catalog
    onResolve = on_resolve
    onMatch = on_match
    onLive = on_live
    onFrontend = on_frontend
    onSetupTest = on_setup_test
    onScreenSource = on_screen_source
    onScreenAction = on_screen_action

    def _kind_catalog(self) -> Dict[str, Any]:
        return self.__dict__.setdefault("_catalog_providers", {})

    def _kind_resolve(self) -> Dict[str, Any]:
        return self.__dict__.setdefault("_kind_resolvers", {})

    def _kind_match(self) -> Dict[str, Any]:
        return self.__dict__.setdefault("_kind_matchers", {})

    def _kind_live(self) -> Dict[str, Any]:
        return self.__dict__.setdefault("_kind_live_providers", {})

    def _kind_frontend(self) -> Dict[str, Any]:
        return self.__dict__.setdefault("_frontend_adapters", {})

    def _dispatch_kinds(
        self, method: str, path: str, req: Dict[str, Any], ctx: PluginContext
    ) -> Optional[Dict[str, Any]]:
        if not path.startswith("/ether/v1/"):
            return None
        parts = [urllib.parse.unquote(p) for p in path[len("/ether/v1/"):].split("/")]
        # Frontend adapters are protocol surfaces (WebDAV, Subsonic, OPDS, ...)
        # and see every method token. The kind and setup routes stay JSON
        # GET/POST.
        if len(parts) >= 2 and parts[0] == "frontend":
            adapter = self._kind_frontend().get(parts[1])
            if adapter is None:
                return _error_response(404, "not_found", f'No frontend adapter is registered for "{parts[1]}".')
            sub_path = "/" + "/".join(parts[2:])
            # Rebuild the adapter URL with the original query. The server
            # flattens repeated keys to the first value, so a query map holds
            # one value per key.
            _, query = _parse_url_path_and_query(
                str(req.get("path") or "/"), req.get("query")
            )
            if query:
                sub_path += "?" + urllib.parse.urlencode(query)
            handle_fn = adapter.handle
            try:
                params = list(inspect.signature(handle_fn).parameters.values())
                accepts_ctx = any(p.kind == inspect.Parameter.VAR_POSITIONAL for p in params) or len(params) >= 5
            except (TypeError, ValueError):
                accepts_ctx = False
            # Exact request bytes ride in bodyBase64 on adapter routes; a
            # non-JSON body (multipart, form-encoded, binary) is only there.
            req_body = req.get("body")
            if req_body is None and isinstance(req.get("bodyBase64"), str):
                req_body = base64.b64decode(req["bodyBase64"])
            if accepts_ctx:
                status, headers, resp_body = handle_fn(method, sub_path, dict(req.get("headers") or {}), req_body or b"", ctx)
            else:
                status, headers, resp_body = handle_fn(method, sub_path, dict(req.get("headers") or {}), req_body or b"")
            # A HEAD reply carries headers only, exactly as a real server would.
            if method == "HEAD":
                resp_body = b""
            return {"status": int(status), "headers": dict(headers or {}), "body": resp_body}

        if method not in ("GET", "POST"):
            return _error_response(405, "method_not_allowed", "Kind routes use GET or POST.")

        try:
            body = _parse_json_body(req.get("body"))
        except Exception:
            return _error_response(400, "invalid_json", "Request body must be valid JSON.")

        if len(parts) == 4 and parts[0] in ("screens", "surfaces") and parts[2] in ("sources", "actions"):
            if method != "POST":
                return _error_response(405, "method_not_allowed", "Screen routes use POST.")
            screen, name = parts[1], parts[3]
            table = self._screen_sources() if parts[2] == "sources" else self._screen_actions()
            handler = table.get(f"{screen}/{name}") or table.get(f"*/{name}")
            if handler is None:
                return _error_response(404, "not_found", f'No screen {parts[2][:-1]} "{name}" is registered for "{screen}".')
            body["screen"] = screen
            body["route" if parts[2] == "sources" else "action"] = name
            return _ok_response(handler(ctx, body) or {})

        if len(parts) == 3 and parts[0] == "setup" and parts[2] == "test":
            tester = getattr(self, "_setup_tester", None)
            if tester is None:
                return _error_response(404, "not_found", "Setup test handler is not registered.")
            body.setdefault("step", parts[1])
            return _ok_response(tester.test_setup(ctx, body))

        if len(parts) < 3 or parts[0] != "kinds":
            return None
        kind, tail = parts[1], parts[2:]
        body = dict(body)
        body["kind"] = kind

        def page(result: Any) -> Dict[str, Any]:
            result = dict(result or {})
            limit = int(body.get("limit") or 200)
            items = list(result.get("items") or [])
            if len(items) > limit:
                items = items[:limit]
            result["items"] = items
            if not result.get("nextCursor"):
                result.pop("nextCursor", None)
            return _ok_response(result)

        if tail == ["browse"] or tail == ["search"]:
            provider = self._kind_catalog().get(kind)
            if provider is None:
                return _error_response(404, "not_found", f'No catalogue provider is registered for kind "{kind}".')
            if tail == ["search"]:
                search = getattr(provider, "search", None)
                if not callable(search):
                    return _error_response(501, "not_implemented", "This provider does not search.")
                return page(search(ctx, body))
            return page(provider.browse(ctx, body))
        if tail == ["expand"]:
            provider = self._kind_catalog().get(kind)
            expand = getattr(provider, "expand", None) if provider else None
            if not callable(expand):
                return _error_response(501, "not_implemented", "This provider does not expand facets.")
            result = dict(expand(ctx, body) or {})
            if not result.get("job"):
                result.setdefault("items", [])
            return _ok_response(result)
        if tail == ["related"]:
            provider = self._kind_catalog().get(kind)
            related = getattr(provider, "related", None) if provider else None
            if not callable(related):
                return _error_response(501, "not_implemented", "This provider has no related items.")
            result = dict(related(ctx, body) or {})
            result.setdefault("items", [])
            return _ok_response(result)
        if tail == ["lyrics"]:
            provider = self._kind_catalog().get(kind)
            lyrics = getattr(provider, "lyrics", None) if provider else None
            if not callable(lyrics):
                return _error_response(501, "not_implemented", "This provider has no lyrics.")
            result = lyrics(ctx, body)
            if not result:
                return _error_response(404, "not_found", "No lyrics for this item.")
            return _ok_response(dict(result))
        if len(tail) == 2 and tail[0] == "items":
            provider = self._kind_catalog().get(kind)
            if provider is None:
                return _error_response(404, "not_found", f'No catalogue provider is registered for kind "{kind}".')
            return _ok_response(provider.detail(ctx, kind, tail[1]))
        if tail == ["changes"]:
            provider = self._kind_catalog().get(kind)
            changes = getattr(provider, "changes", None) if provider else None
            if not callable(changes):
                return _error_response(501, "not_implemented", "This provider has no changes feed.")
            result = dict(changes(ctx, body) or {})
            result.setdefault("upserts", [])
            return _ok_response(result)
        if tail == ["resolve"]:
            resolver = self._kind_resolve().get(kind)
            if resolver is None:
                return _error_response(404, "not_found", f'No resolver is registered for kind "{kind}".')
            result = dict(resolver.resolve(ctx, body) or {})
            result.setdefault("sources", [])
            return _ok_response(result)
        if tail == ["live"]:
            provider = self._kind_live().get(kind)
            if provider is None:
                return _error_response(404, "not_found", f'No live provider is registered for kind "{kind}".')
            result = dict(provider.live(ctx, body) or {})
            limit = int(body.get("limit") or 50)
            items = list(result.get("items") or [])
            if len(items) > limit:
                items = items[:limit]
            result["items"] = items
            if not result.get("nextCursor"):
                result.pop("nextCursor", None)
            return _ok_response(result)
        if tail == ["live", "resolve"]:
            provider = self._kind_live().get(kind)
            if provider is None:
                return _error_response(404, "not_found", f'No live provider is registered for kind "{kind}".')
            resolve_fn = getattr(provider, "resolve_live", None) or getattr(provider, "resolveLive", None)
            if not callable(resolve_fn):
                return _error_response(501, "not_implemented", "This live provider does not resolve live streams.")
            return _ok_response(dict(resolve_fn(ctx, body) or {}))
        if tail == ["match"]:
            matcher = self._kind_match().get(kind)
            if matcher is None:
                return _error_response(404, "not_found", f'No matcher is registered for kind "{kind}".')
            result = dict(matcher.match(ctx, body) or {})
            result.setdefault("matches", [])
            return _ok_response(result)
        return _error_response(404, "not_found", "No kind route matched.")


def plugin(
    manifest: Dict[str, Any],
    routes: Optional[Dict[str, Callable[..., Any]]] = None,
    default_settings: Optional[Dict[str, Any]] = None,
    strings: Optional[Dict[str, Dict[str, str]]] = None,
    default_locale: str = "en",
    host: Optional[Any] = None,
) -> PluginInstance:
    return PluginInstance(
        manifest=manifest,
        routes=routes,
        default_settings=default_settings,
        strings=strings,
        default_locale=default_locale,
        host=host,
    )


# ---------------------------------------------------------------------------
# Sidecar HTTP server & WebSocket tunnel (`ether-tunnel-v1`)
# ---------------------------------------------------------------------------


def _serialize_response_body(body: Any) -> Tuple[bytes, bool]:
    if isinstance(body, (bytes, bytearray)):
        return bytes(body), True
    if isinstance(body, str):
        return body.encode("utf-8"), False
    return json.dumps(body if body is not None else {}, separators=(",", ":")).encode("utf-8"), False


def dispatch_over_tunnel_frame(
    instance: PluginInstance,
    frame: Dict[str, Any],
    max_chunk_bytes: int = DEFAULT_MAX_CHUNK_BYTES,
) -> List[Dict[str, Any]]:
    if frame.get("type") != "request" or not frame.get("id"):
        return []
    frame_id = str(frame["id"])
    raw_b64 = frame.get("body") or ""
    decoded_bytes = base64.b64decode(raw_b64) if raw_b64 else b""
    body_text = decoded_bytes.decode("utf-8", errors="replace") if decoded_bytes else None

    headers: Dict[str, Any] = {}
    if isinstance(frame.get("headers"), dict):
        headers.update(frame["headers"])
    if frame.get("requestId") and "X-Ether-Request-ID" not in headers:
        headers["X-Ether-Request-ID"] = frame["requestId"]

    req = {
        "method": frame.get("method") or "GET",
        "path": frame.get("path") or "/",
        "query": frame.get("query"),
        "headers": headers,
        "body": body_text,
    }
    res = instance.dispatch(req)
    payload_bytes, is_binary = _serialize_response_body(res.get("body"))
    multi_headers = {k: [str(v)] for k, v in (res.get("headers") or {}).items()}

    if is_binary and str(req["path"]).startswith("/ether/file/"):
        out: List[Dict[str, Any]] = [
            {
                "type": "response_start",
                "id": frame_id,
                "status": res["status"],
                "headers": multi_headers,
            }
        ]
        chunk_limit = max(1024, int(max_chunk_bytes))
        for offset in range(0, len(payload_bytes), chunk_limit):
            chunk = payload_bytes[offset : offset + chunk_limit]
            out.append(
                {
                    "type": "response_chunk",
                    "id": frame_id,
                    "data": base64.b64encode(chunk).decode("ascii"),
                }
            )
        out.append({"type": "response_end", "id": frame_id})
        return out

    reply: Dict[str, Any] = {
        "type": "response",
        "id": frame_id,
        "status": res["status"],
        "headers": multi_headers,
    }
    if payload_bytes:
        reply["body"] = base64.b64encode(payload_bytes).decode("ascii")
    return [reply]


class _ThreadingHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


@dataclass
class ServeHandle:
    mode: str
    url: Optional[str] = None
    port: Optional[int] = None
    _close_fn: Callable[[], None] = field(default=lambda: None, repr=False)

    def close(self) -> None:
        self._close_fn()


def _serve_http(
    instance: PluginInstance,
    host: str,
    port: int,
    background: bool,
) -> ServeHandle:
    class _Handler(http.server.BaseHTTPRequestHandler):
        def _handle_all(self) -> None:
            length = int(self.headers.get("Content-Length", "0") or "0")
            raw_body = self.rfile.read(length).decode("utf-8", errors="replace") if length > 0 else None
            hdrs = {k: v for k, v in self.headers.items()}
            res = instance.dispatch(
                {
                    "method": self.command,
                    "path": self.path,
                    "headers": hdrs,
                    "body": raw_body,
                }
            )
            payload_bytes, _ = _serialize_response_body(res.get("body"))
            self.send_response(int(res.get("status", 200)))
            out_headers = {"Content-Type": "application/json"}
            out_headers.update(res.get("headers") or {})
            out_headers["Content-Length"] = str(len(payload_bytes))
            for k, v in out_headers.items():
                self.send_header(k, str(v))
            self.end_headers()
            if self.command.upper() != "HEAD":
                self.wfile.write(payload_bytes)

        def do_GET(self) -> None:  # noqa: N802
            self._handle_all()

        def do_POST(self) -> None:  # noqa: N802
            self._handle_all()

        def do_PUT(self) -> None:  # noqa: N802
            self._handle_all()

        def do_DELETE(self) -> None:  # noqa: N802
            self._handle_all()

        def do_HEAD(self) -> None:  # noqa: N802
            self._handle_all()

        def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
            return

    server = _ThreadingHTTPServer((host, port), _Handler)
    actual_port = int(server.server_address[1])
    url = f"http://{host}:{actual_port}"

    if background:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        def _close() -> None:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2.0)

        return ServeHandle(mode="http", url=url, port=actual_port, _close_fn=_close)

    try:
        server.serve_forever()
    finally:
        server.server_close()
    return ServeHandle(mode="http", url=url, port=actual_port)


def _ws_send_frame(sock: socket.socket, opcode: int, payload: bytes) -> None:
    mask_key = os.urandom(4)
    header = bytearray([0x80 | opcode])
    length = len(payload)
    if length < 126:
        header.append(0x80 | length)
    elif length < 65536:
        header.append(0x80 | 126)
        header.extend(struct.pack("!H", length))
    else:
        header.append(0x80 | 127)
        header.extend(struct.pack("!Q", length))
    header.extend(mask_key)
    masked = bytearray(length)
    for i, b in enumerate(payload):
        masked[i] = b ^ mask_key[i & 3]
    sock.sendall(bytes(header) + bytes(masked))


def _ws_send_text(sock: socket.socket, text: str) -> None:
    _ws_send_frame(sock, 0x01, text.encode("utf-8"))


def _ws_recv_exact(sock: socket.socket, n: int) -> bytes:
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("WebSocket closed unexpectedly")
        buf.extend(chunk)
    return bytes(buf)


def _ws_recv_frame(sock: socket.socket) -> Tuple[int, bytes]:
    first_two = _ws_recv_exact(sock, 2)
    opcode = first_two[0] & 0x0F
    masked = (first_two[1] & 0x80) != 0
    length = first_two[1] & 0x7F
    if length == 126:
        length = struct.unpack("!H", _ws_recv_exact(sock, 2))[0]
    elif length == 127:
        length = struct.unpack("!Q", _ws_recv_exact(sock, 8))[0]
    mask_key = _ws_recv_exact(sock, 4) if masked else b""
    payload = bytearray(_ws_recv_exact(sock, length))
    if masked:
        for i in range(length):
            payload[i] ^= mask_key[i & 3]
    return opcode, bytes(payload)


# ---------------------------------------------------------------------------
# Sidecar pairing (SPEC: sidecars that install like an app)
# ---------------------------------------------------------------------------

PAIR_EXCHANGE_PATH = "/ether/plugins/pair"
TUNNEL_PATH = "/ether/plugins/tunnel"


class CredentialError(RuntimeError):
    """Raised when no usable credential can be resolved for a sidecar."""


def no_credential_message() -> str:
    return (
        "No credential: create a pairing code in Ether (Plugins > this plugin > "
        "Connect a sidecar) and set ETHER_PAIRING_CODE."
    )


def _server_base(base: str) -> str:
    base = (base or "").strip()
    if not base:
        raise CredentialError("ETHER_URL is not set")
    parsed = urllib.parse.urlparse(base)
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        raise CredentialError("ETHER_URL must use http or https and include a host")
    return parsed.scheme + "://" + parsed.netloc


def tunnel_url_from_base(base: str) -> str:
    parsed = urllib.parse.urlparse(_server_base(base))
    scheme = "wss" if parsed.scheme == "https" else "ws"
    return f"{scheme}://{parsed.netloc}{TUNNEL_PATH}"


def pair_url_from_base(base: str) -> str:
    return _server_base(base) + PAIR_EXCHANGE_PATH


def resolve_state_dir(explicit: str, plugin_id: str) -> str:
    if (explicit or "").strip():
        return explicit.strip()
    data = "/data"
    if os.path.isdir(data) and os.access(data, os.W_OK):
        return data
    config_home = os.environ.get("XDG_CONFIG_HOME") or os.path.join(
        os.path.expanduser("~"), ".config"
    )
    return os.path.join(config_home, "ether-plugins", plugin_id or "plugin")


def _credential_file(state_dir: str) -> str:
    return os.path.join(state_dir, "credential")


def read_saved_credential(state_dir: str) -> Optional[str]:
    try:
        with open(_credential_file(state_dir), "r", encoding="utf-8") as fh:
            saved = json.load(fh)
    except (OSError, ValueError):
        return None
    cred = saved.get("credential") if isinstance(saved, dict) else None
    return str(cred) if cred else None


def write_saved_credential(state_dir: str, credential: str, url: str) -> None:
    os.makedirs(state_dir, mode=0o700, exist_ok=True)
    try:
        os.chmod(state_dir, 0o700)
    except OSError:
        pass
    path = _credential_file(state_dir)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump({"credential": credential, "url": url}, fh)
    finally:
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass


def clear_saved_credential(state_dir: str) -> None:
    try:
        os.remove(_credential_file(state_dir))
    except OSError:
        pass


def exchange_code(base: str, code: str) -> str:
    body = json.dumps({"code": code}).encode("utf-8")
    request = urllib.request.Request(
        pair_url_from_base(base),
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            payload = json.load(response)
    except urllib.error.HTTPError as exc:
        raise CredentialError(
            f"Pairing code was rejected (HTTP {exc.code})."
        ) from None
    except OSError as exc:
        raise CredentialError(f"Pairing request failed: {exc}") from None
    credential = payload.get("credential") if isinstance(payload, dict) else None
    if not credential:
        raise CredentialError("Pairing response had no credential.")
    return str(credential)


def resolve_credential(
    plugin_id: str,
    *,
    credential: Optional[str] = None,
    url: Optional[str] = None,
    code: Optional[str] = None,
    state_dir: Optional[str] = None,
) -> Tuple[str, bool, str]:
    """Applies the startup order: explicit credential, then the saved file, then
    a one-time pairing code (exchanged and saved). Returns (credential,
    from_file, state_dir). Never logs the credential or the code."""
    resolved_dir = resolve_state_dir(state_dir or "", plugin_id)
    explicit = credential if credential is not None else os.environ.get(
        "ETHER_PLUGIN_CREDENTIAL", ""
    )
    if (explicit or "").strip():
        return explicit.strip(), False, resolved_dir
    saved = read_saved_credential(resolved_dir)
    if saved:
        return saved, True, resolved_dir
    resolved_url = url or os.environ.get("ETHER_URL", "")
    resolved_code = code if code is not None else os.environ.get("ETHER_PAIRING_CODE", "")
    if (resolved_url or "").strip() and (resolved_code or "").strip():
        cred = exchange_code(resolved_url, resolved_code.strip())
        write_saved_credential(resolved_dir, cred, resolved_url)
        return cred, True, resolved_dir
    raise CredentialError(no_credential_message())


def _serve_tunnel(
    instance: PluginInstance,
    tunnel_url: str,
    credential: str,
    background: bool,
    from_file: bool = False,
    saved_dir: Optional[str] = None,
) -> ServeHandle:
    parsed = urllib.parse.urlparse(tunnel_url)
    if parsed.scheme not in ("ws", "wss") or not parsed.hostname:
        raise ValueError("Tunnel URL must use ws:// or wss:// and include a host")
    host = parsed.hostname
    port = parsed.port or (443 if parsed.scheme == "wss" else 80)
    path = parsed.path or "/ether/plugins/tunnel"
    if parsed.query:
        path = f"{path}?{parsed.query}"

    sock = socket.create_connection((host, port), timeout=10)
    if parsed.scheme == "wss":
        try:
            sock = ssl.create_default_context().wrap_socket(sock, server_hostname=host)
        except Exception:
            sock.close()
            raise
    ws_key = base64.b64encode(os.urandom(16)).decode("ascii")
    lines = [
        f"GET {path} HTTP/1.1",
        f"Host: {host}:{port}",
        "Upgrade: websocket",
        "Connection: Upgrade",
        f"Sec-WebSocket-Key: {ws_key}",
        "Sec-WebSocket-Version: 13",
        f"Sec-WebSocket-Protocol: {TUNNEL_PROTOCOL}",
    ]
    if credential:
        lines.append(f"X-Ether-Plugin-Credential: {credential}")
    lines.append("\r\n")
    sock.sendall("\r\n".join(lines).encode("ascii"))

    resp_buf = b""
    while b"\r\n\r\n" not in resp_buf:
        chunk = sock.recv(1)
        if not chunk or len(resp_buf) >= 16384:
            sock.close()
            raise ConnectionError("Failed WebSocket tunnel handshake")
        resp_buf += chunk
    status_line = resp_buf.split(b"\r\n", 1)[0].decode("ascii", errors="replace")
    if " 101 " not in status_line:
        sock.close()
        if " 401 " in status_line and from_file:
            clear_saved_credential(saved_dir)
            raise CredentialError(no_credential_message())
        raise ConnectionError(f"Tunnel handshake rejected: {status_line}")

    expected_accept = base64.b64encode(
        hashlib.sha1((ws_key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode("ascii")).digest()
    ).decode("ascii")
    if expected_accept.encode("ascii") not in resp_buf:
        sock.close()
        raise ConnectionError("Invalid Sec-WebSocket-Accept in tunnel handshake")

    sock.settimeout(None)
    hello = {
        "type": "hello",
        "protocol": TUNNEL_PROTOCOL,
        "plugin": instance.manifest["id"],
        "features": ["range", "stream"],
    }
    _ws_send_text(sock, json.dumps(hello))

    opcode, welcome_bytes = _ws_recv_frame(sock)
    if opcode != 0x01:
        sock.close()
        raise ConnectionError("Expected welcome text frame on tunnel")
    welcome = json.loads(welcome_bytes.decode("utf-8"))
    if welcome.get("type") != "welcome":
        sock.close()
        raise ConnectionError(f"Unexpected tunnel frame: {welcome.get('type')}")
    max_chunk_bytes = int(welcome.get("maxChunkBytes") or DEFAULT_MAX_CHUNK_BYTES)

    stop_event = threading.Event()
    write_lock = threading.Lock()
    active_lock = threading.Lock()
    active: Dict[str, threading.Event] = {}

    def _reply(frame: Dict[str, Any], cancelled: threading.Event) -> None:
        frame_id = str(frame.get("id", ""))
        try:
            replies = dispatch_over_tunnel_frame(instance, frame, max_chunk_bytes)
            for reply in replies:
                if cancelled.is_set() or stop_event.is_set():
                    return
                with write_lock:
                    _ws_send_text(sock, json.dumps(reply))
        except Exception:
            if not cancelled.is_set() and not stop_event.is_set():
                with write_lock:
                    _ws_send_text(sock, json.dumps({"type": "error", "id": frame_id,
                        "error": {"code": "internal_error", "message": "Plugin request failed"}}))
        finally:
            with active_lock:
                active.pop(frame_id, None)

    def _loop() -> None:
        try:
            while not stop_event.is_set():
                op, raw = _ws_recv_frame(sock)
                if op == 0x08:
                    break
                if op == 0x09:
                    with write_lock:
                        _ws_send_frame(sock, 0x0A, raw)
                    continue
                if op != 0x01:
                    continue
                frame = json.loads(raw.decode("utf-8"))
                frame_id = str(frame.get("id", ""))
                if frame.get("type") == "cancel":
                    with active_lock:
                        cancelled = active.get(frame_id)
                    if cancelled:
                        cancelled.set()
                elif frame.get("type") == "request":
                    cancelled = threading.Event()
                    with active_lock:
                        active[frame_id] = cancelled
                    threading.Thread(target=_reply, args=(frame, cancelled), daemon=True).start()
        except (OSError, ConnectionError, ValueError):
            pass
        finally:
            stop_event.set()
            with active_lock:
                for cancelled in active.values():
                    cancelled.set()
            sock.close()

    def _close() -> None:
        stop_event.set()
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        sock.close()

    if background:
        thread = threading.Thread(target=_loop, daemon=True)
        thread.start()
        return ServeHandle(mode="tunnel", url=tunnel_url, _close_fn=_close)

    try:
        _loop()
    finally:
        _close()
    return ServeHandle(mode="tunnel", url=tunnel_url)


def serve(
    instance: PluginInstance,
    *,
    mode: Optional[str] = None,
    host: Optional[str] = None,
    port: Optional[int] = None,
    tunnel_url: Optional[str] = None,
    credential: Optional[str] = None,
    background: bool = True,
    url: Optional[str] = None,
    pairing_code: Optional[str] = None,
    state_dir: Optional[str] = None,
) -> ServeHandle:
    resolved_tunnel = tunnel_url or os.environ.get("ETHER_TUNNEL_URL")
    base = url or os.environ.get("ETHER_URL", "")
    if not resolved_tunnel and base:
        resolved_tunnel = tunnel_url_from_base(base)
    resolved_mode = mode or ("tunnel" if resolved_tunnel else "http")
    if resolved_mode == "tunnel":
        if not resolved_tunnel:
            raise ValueError("Tunnel mode requires tunnel_url, ETHER_TUNNEL_URL or ETHER_URL.")
        try:
            cred, from_file, saved_dir = resolve_credential(
                instance.manifest.get("id", ""),
                credential=credential,
                url=base,
                code=pairing_code or os.environ.get("ETHER_PAIRING_CODE", ""),
                state_dir=state_dir or os.environ.get("ETHER_PLUGIN_STATE_DIR", ""),
            )
        except CredentialError:
            # A low-level caller may pass only a tunnel URL for testing; the
            # pairing flow always has ETHER_URL, so the clear line still applies
            # to real sidecars.
            if base:
                raise
            cred, from_file, saved_dir = "", False, ""
        return _serve_tunnel(instance, resolved_tunnel, cred, background, from_file, saved_dir)

    resolved_host = host or os.environ.get("ETHER_PLUGIN_HOST") or "127.0.0.1"
    resolved_port = (
        port
        if port is not None
        else int(os.environ["ETHER_PLUGIN_PORT"])
        if os.environ.get("ETHER_PLUGIN_PORT")
        else 0
    )
    return _serve_http(instance, resolved_host, resolved_port, background)


# ---------------------------------------------------------------------------
# Testing harness, mock host client, and fixtures
# ---------------------------------------------------------------------------


class MockHostClient:
    """In-memory mock implementation of Ether host services for unit tests."""

    def __init__(self) -> None:
        self.kv_store: Dict[str, str] = {}
        self.badges_store: Dict[str, Dict[str, List[str]]] = {}
        self.badge_quota_titles: int = POSTER_BADGE_QUOTA_TITLES
        self.secrets_store: Dict[str, str] = {}
        self.catalog_store: Dict[str, Dict[str, Any]] = {}
        self.history_store: Dict[int, List[Dict[str, Any]]] = {}
        self.watch_state: Dict[str, bool] = {}
        self.collections_list: List[Dict[str, Any]] = []
        self.published_events: List[Dict[str, Any]] = []
        self.notifications: List[str] = []
        self.logs: List[Dict[str, Any]] = []
        self.last_stream_url: Optional[Tuple[int, str]] = None
        self.last_stream_url_part: Optional[int] = None
        self.last_stream_url_source: Optional[str] = None
        self.last_stream_url_absolute: bool = False
        self.stream_origin: str = "http://ether.test"
        self.http_handler: Callable[[Dict[str, Any]], Dict[str, Any]] = lambda req: {
            "status": 200,
            "headers": {"Content-Type": "application/json"},
            "body": "{}",
        }
        self.ai_handler: Optional[Callable[[AICompleteRequest], AICompleteResponse]] = None
        self.media_probe_handler: Optional[Callable[[Dict[str, Any]], MediaProbeResult]] = None

        self.kv = self._KV(self)
        self.badges = self._Badges(self)
        self.secret = self._Secret(self)
        self.http = self._Http(self)
        self.catalog = self._Catalog(self)
        self.viewers = self._Viewers(self)
        self.kind_store: Dict[str, List[Dict[str, Any]]] = {}
        self.kind_hidden_ids: Dict[str, set] = {}
        self.kind_policy_filter: Optional[Callable[[str, Dict[str, Any]], bool]] = None
        self.sources: List[Dict[str, Any]] = []
        self.source_indexers: Optional[List[Dict[str, Any]]] = None
        self.source_handler: Optional[Callable[[Dict[str, Any]], Dict[str, Any]]] = None
        self.blobs: Dict[str, bytes] = {}
        self.limits: RuntimeLimits = {
            "memoryBytes": 32 * 1024 * 1024,
            "readBytes": MIN_DATA_READ_BYTES,
            "fetchBytes": 8 * 1024 * 1024 * 1024,
            "archiveMemberBytes": 8 * 1024 * 1024 * 1024,
            "archiveExpandedBytes": 32 * 1024 * 1024 * 1024,
            "archiveMembers": 65535,
            "maxItems": 2_000_000,
            "deadline": "12h",
            "poolSize": 2,
        }
        self.runtime = self._Runtime(self)
        self.collections = self._Collections(self)
        self.events = self._Events(self)
        self.ai = self._AI(self)
        self.media = self._Media(self)
        self.notify = self._Notify(self)
        self.kinds = self._Kinds(self)
        self.cache = self._Cache(self)
        self.archive = self._Archive(self)
        self.frontend = self._Frontend(self)

    class _Runtime:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def limits(self) -> RuntimeLimits:
            return dict(self._p.limits)  # type: ignore[return-value]

    class _Frontend:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def stream_url(
            self,
            viewer_id: int,
            item_ref: str,
            part_index: Optional[int] = None,
            source: Optional[str] = None,
            absolute: bool = False,
        ) -> str:
            checked = _check_part_index(part_index)
            checked_source = _check_stream_source(source)
            if not isinstance(absolute, bool):
                raise ValueError("stream url: absolute must be a bool")
            self._p.last_stream_url = (int(viewer_id), str(item_ref))
            self._p.last_stream_url_part = checked
            self._p.last_stream_url_source = checked_source
            self._p.last_stream_url_absolute = absolute
            base = f"/ether/v1/kind-media/mock-{int(viewer_id)}"
            rel = base if checked is None else f"{base}?part={checked}"
            if absolute:
                origin = (self._p.stream_origin or "http://ether.test").rstrip("/")
                return f"{origin}{rel}"
            return rel

    class _Kinds:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def upsert(self, kind: str, items: List[Dict[str, Any]]) -> int:
            kept = {it.get("id"): it for it in self._p.kind_store.get(kind, [])}
            for it in items:
                kept[it.get("id")] = it
            self._p.kind_store[kind] = list(kept.values())
            return len(items)

        def delete(self, kind: str, external_ids: List[str]) -> None:
            drop = set(external_ids)
            self._p.kind_store[kind] = [it for it in self._p.kind_store.get(kind, []) if it.get("id") not in drop]

        def items(self, kind: str, cursor: str = "", limit: int = 200) -> Dict[str, Any]:
            all_items = self._p.kind_store.get(kind, [])
            start = 0
            if cursor:
                start = next((i + 1 for i, it in enumerate(all_items) if it.get("id") == cursor), len(all_items))
            page = all_items[start:start + limit]
            out: Dict[str, Any] = {"items": page}
            if page and start + limit < len(all_items):
                out["nextCursor"] = page[-1].get("id")
            return out

        def _allowed(self, kind: str, item: Dict[str, Any]) -> bool:
            hidden = self._p.kind_hidden_ids.get(kind)
            if hidden and item.get("id") in hidden:
                return False
            filt = self._p.kind_policy_filter
            if filt is not None and not filt(kind, item):
                return False
            return True

        def visible(
            self,
            kind: str,
            ids: List[str],
            viewer_token: Optional[str] = None,
        ) -> List[str]:
            if not (viewer_token or "").strip():
                raise RuntimeError("kinds.visible: no authenticated frontend adapter viewer token on this request")
            if len(ids) > MAX_KIND_VISIBLE_IDS:
                raise ValueError(f"kinds.visible: batch larger than {MAX_KIND_VISIBLE_IDS} ids")
            by_id = {str(it.get("id")): it for it in self._p.kind_store.get(kind, [])}
            seen: set = set()
            out: List[str] = []
            for raw_id in ids:
                ext_id = str(raw_id)
                if not ext_id or ext_id in seen:
                    continue
                seen.add(ext_id)
                it = by_id.get(ext_id)
                if it is None or not self._allowed(kind, it):
                    continue
                out.append(ext_id)
            return out

        def browse(
            self,
            kind: str,
            query: Optional[Dict[str, Any]] = None,
            viewer_token: Optional[str] = None,
        ) -> Dict[str, Any]:
            if not (viewer_token or "").strip():
                raise RuntimeError("kinds.browse: no authenticated frontend adapter viewer token on this request")
            q = dict(query or {})
            total_mode = q.get("total")
            if total_mode not in (None, "", "cheap", "none", "exact"):
                raise ValueError(f"kinds.browse: invalid total mode {total_mode!r}")
            limit = int(q.get("limit") or 200)
            if limit <= 0:
                limit = 200
            parent_id = str(q.get("parentId") or "")
            level = str(q.get("level") or "")
            needle = str(q.get("query") or "").strip().lower()
            exclude_needle = str(q.get("excludeQuery") or "").strip().lower()
            facets = q.get("facets") if isinstance(q.get("facets"), dict) else {}
            all_facets = q.get("allFacets") if isinstance(q.get("allFacets"), dict) else {}
            exclude_facets = q.get("excludeFacets") if isinstance(q.get("excludeFacets"), dict) else {}
            any_groups = q.get("anyGroups") if isinstance(q.get("anyGroups"), list) else []
            empty_fields = q.get("empty") if isinstance(q.get("empty"), dict) else {}
            matched: List[Dict[str, Any]] = []
            for it in self._p.kind_store.get(kind, []):
                if not self._allowed(kind, it):
                    continue
                if parent_id and str(it.get("parentId") or "") != parent_id:
                    continue
                if level and str(it.get("level") or "") != level:
                    continue
                if needle:
                    hay = f"{it.get('title', '')} {it.get('summary', '')}".lower()
                    if needle not in hay:
                        continue
                if exclude_needle:
                    hay = f"{it.get('title', '')} {it.get('summary', '')}".lower()
                    if exclude_needle in hay:
                        continue
                if facets or all_facets or exclude_facets or any_groups or empty_fields:
                    item_fields = it.get("fields") if isinstance(it.get("fields"), dict) else {}

                    def _have(fk: str) -> List[str]:
                        raw = item_fields.get(fk)
                        if isinstance(raw, list):
                            return [str(v).lower() for v in raw]
                        if raw is not None:
                            return [str(raw).lower()]
                        return []

                    ok = True
                    for fk, want in facets.items():
                        if want and not any(str(w).lower() in _have(fk) for w in want):
                            ok = False
                            break
                    if ok:
                        for fk, want in all_facets.items():
                            if want and not all(str(w).lower() in _have(fk) for w in want):
                                ok = False
                                break
                    if ok:
                        for fk, want in exclude_facets.items():
                            if want and any(str(w).lower() in _have(fk) for w in want):
                                ok = False
                                break
                    if ok:
                        for grp in any_groups:
                            if not isinstance(grp, dict):
                                continue
                            alts: List[Tuple[str, List[str]]] = []
                            fk = str(grp.get("field") or "").strip()
                            raw_vals = grp.get("values") if isinstance(grp.get("values"), list) else []
                            want = [str(w).strip() for w in raw_vals if str(w).strip()]
                            if fk and want:
                                alts.append((fk, want))
                            raw_any = grp.get("any") if isinstance(grp.get("any"), list) else []
                            for alt in raw_any:
                                if not isinstance(alt, dict):
                                    continue
                                afk = str(alt.get("field") or "").strip()
                                avals = alt.get("values") if isinstance(alt.get("values"), list) else []
                                awant = [str(w).strip() for w in avals if str(w).strip()]
                                if afk and awant:
                                    alts.append((afk, awant))
                            if alts and not any(
                                any(w.lower() in _have(afk) for w in awant)
                                for afk, awant in alts
                            ):
                                ok = False
                                break
                    if ok and empty_fields:
                        for fk, want_empty in empty_fields.items():
                            has_val = False
                            if fk in item_fields and item_fields[fk] is not None:
                                raw = item_fields[fk]
                                if isinstance(raw, list):
                                    has_val = any(v is not None and v != "" for v in raw)
                                elif isinstance(raw, str):
                                    has_val = raw != ""
                                else:
                                    has_val = True
                            elif fk == "released":
                                has_val = bool(it.get("released"))
                            elif fk in ("year", "decade"):
                                has_val = int(it.get("year") or 0) > 0
                            elif fk == "rating":
                                has_val = float(it.get("rating") or 0) > 0
                            elif fk == "summary":
                                has_val = bool(it.get("summary"))
                            elif fk == "recording":
                                has_val = bool(it.get("recording"))
                            elif fk == "parentId":
                                has_val = bool(it.get("parentId"))
                            elif fk == "index":
                                has_val = int(it.get("index") or 0) > 0
                            if want_empty and has_val:
                                ok = False
                                break
                            if not want_empty and not has_val:
                                ok = False
                                break
                    if not ok:
                        continue
                matched.append(it)
            if q.get("desc"):
                matched = list(reversed(matched))
            cursor = str(q.get("cursor") or "")
            start = 0
            if cursor:
                start = next((i + 1 for i, it in enumerate(matched) if it.get("id") == cursor), len(matched))
            page = matched[start:start + limit]
            out: Dict[str, Any] = {"items": page}
            if page and start + limit < len(matched):
                out["nextCursor"] = page[-1].get("id")
            if total_mode != "none":
                out["total"] = len(matched)
            return out

        def resolve_sources(self, query: Union[SourceQuery, Dict[str, Any]]) -> List[SourceCandidate]:
            res = self.resolve_sources_with_result(query)
            return list(res.get("candidates") or [])

        def resolve_sources_with_result(self, query: Union[SourceQuery, Dict[str, Any]]) -> SourceResolveResult:
            if self._p.source_handler is not None:
                res = self._p.source_handler(dict(query))
                out: Dict[str, Any] = {"candidates": list((res or {}).get("candidates") or [])}
                if res and "indexers" in res and res["indexers"] is not None:
                    out["indexers"] = list(res["indexers"])
                if res and "pending" in res and res["pending"] is not None:
                    out["pending"] = list(res["pending"])
                if res and "searchId" in res and res["searchId"] is not None:
                    out["searchId"] = str(res["searchId"])
                if res and "done" in res and res["done"] is not None:
                    out["done"] = bool(res["done"])
                return out  # type: ignore[return-value]
            out = {"candidates": list(self._p.sources)}
            if self._p.source_indexers is not None:
                out["indexers"] = list(self._p.source_indexers)
            return out  # type: ignore[return-value]

    class _Cache:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def put(self, library_key: str, name: str, data: bytes) -> str:
            ref = "blob:" + hashlib.sha256(data).hexdigest()
            self._p.blobs[ref] = bytes(data)
            return ref

        def open(self, blob_ref: str) -> bytes:  # noqa: A002 - mirrors host naming
            return self._p.blobs[blob_ref]

        def delete(self, blob_ref: str) -> None:
            self._p.blobs.pop(blob_ref, None)

        def read(self, blob_ref: str, data_range: DataRange) -> bytes:
            cap = int(self._p.limits.get("readBytes") or MIN_DATA_READ_BYTES)
            norm = _check_data_range(data_range, max_bytes=cap)
            raw = self._p.blobs[blob_ref]
            return raw[norm["offset"] : norm["offset"] + norm["length"]]

        def rasterize(
            self,
            library_key: str,
            name: str,
            blob_ref: str,
            options: Optional[RasterizeOptions] = None,
        ) -> RasterizeResult:
            _check_rasterize_options(options)
            _ = self._p.blobs[blob_ref]
            raise RuntimeError("cache.rasterize is not available in MockHostClient; use a real Ether host to rasterise SVG")

    class _Archive:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        @staticmethod
        def _unsafe_reason(name: str) -> str:
            trimmed = name[:-1] if name.endswith("/") else name
            if not trimmed or len(name.encode("utf-8")) > 1024:
                return "empty or oversized member name"
            if "\x00" in trimmed or "\\" in trimmed or trimmed.startswith("/"):
                return "non-relative or invalid path separator"
            parts = trimmed.split("/")
            if any(p in ("", ".", "..") for p in parts) or (len(parts[0]) >= 2 and parts[0][1] == ":"):
                return "unsafe archive member path"
            return ""

        def list(self, blob_ref: str) -> List[ArchiveMember]:
            import io
            import zipfile

            out: List[ArchiveMember] = []
            with zipfile.ZipFile(io.BytesIO(self._p.blobs[blob_ref])) as zf:
                for idx, info in enumerate(zf.infolist()):
                    entry: ArchiveMember = {"index": idx, "name": info.filename, "size": info.file_size}
                    reason = self._unsafe_reason(info.filename)
                    if reason:
                        entry["unsafe"] = True
                        entry["unsafeReason"] = reason
                    out.append(entry)
            return out

        def open(self, blob_ref: str, member: int, data_range: DataRange) -> bytes:  # noqa: A002
            import io
            import zipfile

            cap = int(self._p.limits.get("readBytes") or MIN_DATA_READ_BYTES)
            norm = _check_data_range(data_range, max_bytes=cap)
            with zipfile.ZipFile(io.BytesIO(self._p.blobs[blob_ref])) as zf:
                infos = zf.infolist()
                if member < 0 or member >= len(infos):
                    raise RuntimeError("plugins: archive member index outside range")
                info = infos[member]
                if info.is_dir():
                    raise RuntimeError("plugins: archive member is a directory")
                data = zf.read(info)
                if norm["offset"] > len(data):
                    raise RuntimeError("plugins: archive member offset outside range")
                return data[norm["offset"] : norm["offset"] + norm["length"]]

        def read(self, blob_ref: str, reads: List[ArchiveReadItem]) -> List[ArchiveReadResult]:
            import io
            import zipfile

            cap = int(self._p.limits.get("readBytes") or MIN_DATA_READ_BYTES)
            norm = _check_archive_reads(reads, max_bytes=cap)
            out: List[ArchiveReadResult] = []
            with zipfile.ZipFile(io.BytesIO(self._p.blobs[blob_ref])) as zf:
                infos = zf.infolist()
                for idx, item in enumerate(norm):
                    member = item["member"]
                    if member < 0 or member >= len(infos):
                        raise RuntimeError("plugins: archive member index outside range")
                    info = infos[member]
                    if info.is_dir():
                        raise RuntimeError("plugins: archive member is a directory")
                    data = zf.read(info)
                    if item["offset"] > len(data):
                        raise RuntimeError("plugins: archive member offset outside range")
                    chunk = data[item["offset"] : item["offset"] + item["length"]]
                    out.append({"index": idx, "member": member, "size": len(chunk), "body": chunk})
            return out

        def extract(
            self,
            blob_ref: str,
            member: int,
            options: Optional[ArchiveExtractOptions] = None,
        ) -> ArchiveExtractResult:
            import io
            import zipfile
            import zlib

            with zipfile.ZipFile(io.BytesIO(self._p.blobs[blob_ref])) as zf:
                infos = zf.infolist()
                if member < 0 or member >= len(infos):
                    raise RuntimeError("plugins: archive member index outside range")
                info = infos[member]
                if info.is_dir():
                    raise RuntimeError("plugins: archive member is a directory")
                data = zf.read(info)
            ref = "blob:" + hashlib.sha256(data).hexdigest()
            self._p.blobs[ref] = data
            return {
                "ref": ref,
                "size": len(data),
                "sha1": hashlib.sha1(data).hexdigest(),
                "md5": hashlib.md5(data).hexdigest(),
                "crc32": f"{zlib.crc32(data) & 0xFFFFFFFF:08x}",
            }

    class _KV:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def get(self, key: str) -> Optional[str]:
            return self._p.kv_store.get(key)

        def put(self, key: str, value: str) -> None:
            self._p.kv_store[key] = str(value)

        def delete(self, key: str) -> None:
            self._p.kv_store.pop(key, None)

        def list(self, prefix: str = "") -> List[str]:
            return sorted(k for k in self._p.kv_store if k.startswith(prefix))

        def usage(self) -> Dict[str, int]:
            used = sum(len(v.encode("utf-8")) for v in self._p.kv_store.values())
            keys = len(self._p.kv_store)
            return {
                "bytes": used + keys * 256,
                "keys": keys,
                "quotaBytes": 50 * 1024 * 1024,
                "quotaKeys": 10000,
            }

    class _Badges:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def set(self, set_key: str, entries: List[BadgeEntry]) -> None:
            _check_badge_set_key(set_key)
            if not isinstance(entries, list):
                raise ValueError("badges.set entries must be a list")
            if len(entries) > MAX_BADGE_ENTRIES_PER_CALL:
                raise ValueError(
                    f"badges.set accepts at most {MAX_BADGE_ENTRIES_PER_CALL} entries per call"
                )
            next_set = dict(self._p.badges_store.get(set_key, {}))
            for entry in entries:
                if not isinstance(entry, dict) or not isinstance(entry.get("labels"), list):
                    raise ValueError("badge entry labels must be a list")
                raw_labels = entry["labels"]
                if len(raw_labels) > MAX_BADGE_LABELS_PER_TITLE:
                    raise ValueError(
                        f"badge entry may have at most {MAX_BADGE_LABELS_PER_TITLE} labels per title"
                    )
                title_key = _resolve_badge_title_key(entry.get("title"), self._p.catalog_store)
                if len(raw_labels) == 0:
                    next_set.pop(title_key, None)
                    continue
                deduped: List[str] = []
                for lbl in raw_labels:
                    norm = _normalize_badge_label(lbl)
                    if norm not in deduped:
                        deduped.append(norm)
                next_set[title_key] = deduped
            preview = dict(self._p.badges_store)
            if next_set:
                preview[set_key] = next_set
            else:
                preview.pop(set_key, None)
            total_titles = _count_distinct_badge_titles(preview)
            if total_titles > self._p.badge_quota_titles:
                raise RuntimeError(
                    f"poster badge quota exceeded ({total_titles} > {self._p.badge_quota_titles} titles)"
                )
            if next_set:
                self._p.badges_store[set_key] = next_set
            else:
                self._p.badges_store.pop(set_key, None)

        def clear(self, set_key: str) -> None:
            _check_badge_set_key(set_key)
            self._p.badges_store.pop(set_key, None)

        def usage(self) -> BadgeUsage:
            return {
                "titles": _count_distinct_badge_titles(self._p.badges_store),
                "quotaTitles": self._p.badge_quota_titles,
            }

    class _Secret:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def get(self, name: str) -> str:
            return self._p.secrets_store.get(name, "")

        def set(self, name: str, value: str, ttl_sec: Optional[int] = None) -> None:
            if value == "":
                self._p.secrets_store.pop(name, None)
            else:
                self._p.secrets_store[name] = str(value)

        def delete(self, name: str) -> None:
            self._p.secrets_store.pop(name, None)

    class _Http:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def fetch(self, req: Dict[str, Any]) -> Dict[str, Any]:
            return self._p.http_handler(req)

        def fetch_many(
            self,
            requests: List[Dict[str, Any]],
            opts: Optional[Dict[str, Any]] = None,
        ) -> List[Dict[str, Any]]:
            return _run_fetch_many(requests, opts, self.fetch)

        fetchMany = fetch_many

    class _Catalog:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def search(self, query: str, limit: int = 50, offset: int = 0) -> Dict[str, Any]:
            needle = query.lower()
            matches = [
                v
                for v in self._p.catalog_store.values()
                if needle in str(v.get("title", "")).lower()
            ]
            return {"titles": matches[offset : offset + limit]}

        def get(self, title_id: str) -> Optional[Dict[str, Any]]:
            return self._p.catalog_store.get(title_id)

    class _Viewers:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def history(self, viewer_id: int, limit: int = 50, offset: int = 0) -> List[Dict[str, Any]]:
            items = self._p.history_store.get(int(viewer_id), [])
            return items[offset : offset + limit]

        def set_watch_state(self, viewer_id: int, title_id: str, watched: bool) -> None:
            self._p.watch_state[f"{viewer_id}:{title_id}"] = bool(watched)

        setWatchState = set_watch_state

    class _Collections:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def upsert(self, collection: Dict[str, Any]) -> None:
            self._p.collections_list.append(collection)

    class _Events:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def publish(
            self,
            key: str,
            payload: Optional[Dict[str, Any]] = None,
            correlation: Optional[EventCorrelation] = None,
        ) -> None:
            entry: Dict[str, Any] = {
                "key": str(key),
                "payload": dict(payload) if payload is not None else {},
            }
            if correlation is not None:
                entry["correlation"] = dict(correlation)
            self._p.published_events.append(entry)

    class _AI:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def complete(self, req: AICompleteRequest) -> AICompleteResponse:
            if self._p.ai_handler is None:
                raise HostError("Language model is not configured", code="not_configured", status=501)
            return self._p.ai_handler(req)

    class _Media:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def probe(self, source: Union[PlayableSource, Dict[str, Any]]) -> MediaProbeResult:
            if self._p.media_probe_handler is None:
                raise HostError("media.probe is not configured in mock host", code="unavailable", status=503)
            return self._p.media_probe_handler(source)

    class _Notify:
        def __init__(self, parent: "MockHostClient") -> None:
            self._p = parent

        def owner(self, text: str) -> None:
            self._p.notifications.append(str(text))

    def log(
        self,
        level: str,
        message: str,
        attrs: Optional[Dict[str, Any]] = None,
    ) -> None:
        self.logs.append({"level": level, "message": message, "attrs": attrs})

    def abort(self, message: str = "aborted") -> None:
        raise RuntimeError(f'__ETHER_FAULT__{{"code":"aborted","message":{json.dumps(message)}}}')


def create_mock_host_client() -> MockHostClient:
    return MockHostClient()


def assert_valid_view_spec(doc: Dict[str, Any]) -> None:
    if not isinstance(doc, dict):
        raise AssertionError("ViewSpecDocument must be a dict.")
    if doc.get("specVersion") != 1:
        raise AssertionError(f"ViewSpecDocument specVersion must be 1 (got {doc.get('specVersion')}).")
    if doc.get("layout") not in ("list", "detail", "bare"):
        raise AssertionError(f"ViewSpecDocument layout must be list, detail, or bare (got {doc.get('layout')}).")
    if not isinstance(doc.get("header"), list):
        raise AssertionError("ViewSpecDocument header must be a list.")
    if not isinstance(doc.get("body"), list):
        raise AssertionError("ViewSpecDocument body must be a list.")

    def _check_no_classname(val: Any, path: str) -> None:
        if isinstance(val, list):
            for idx, entry in enumerate(val):
                _check_no_classname(entry, f"{path}[{idx}]")
            return
        if isinstance(val, dict):
            for k, v in val.items():
                if k == "className" or k.endswith("ClassName"):
                    raise AssertionError(f"ViewSpec forbids {k} at {path}.{k}.")
                _check_no_classname(v, f"{path}.{k}")

    _check_no_classname(doc, "spec")


class fixtures:  # noqa: N801
    @staticmethod
    def pick_token(item_id: str = "ether:movie:1") -> str:
        return f"pick-v1:{item_id}:sig-test"

    @staticmethod
    def movie(**overrides: Any) -> Dict[str, Any]:
        base: Dict[str, Any] = {
            "id": "tt0133093",
            "type": "movie",
            "name": "The Matrix",
            "releaseInfo": "1999",
            "description": "A computer hacker learns about the true nature of reality.",
            "genres": ["Action", "Sci-Fi"],
            "imdbRating": "8.7",
        }
        base.update(overrides)
        return base

    @staticmethod
    def catalog_item(**overrides: Any) -> Dict[str, Any]:
        base: Dict[str, Any] = {
            "id": "tt0133093",
            "type": "movie",
            "name": "The Matrix",
            "releaseInfo": "1999",
        }
        base.update(overrides)
        return base

    @staticmethod
    def playback_event(
        name: str = "playback.completed", **overrides: Any
    ) -> Dict[str, Any]:
        base: Dict[str, Any] = {
            "id": "evt_01",
            "name": name,
            "at": "2026-11-02T12:00:00Z",
            "viewerId": 1,
            "payload": {
                "titleId": "tt0133093",
                "title": "The Matrix",
                "progress": 0.95,
            },
        }
        base.update(overrides)
        return base

    @staticmethod
    def candidate(**overrides: Any) -> Dict[str, Any]:
        base: Dict[str, Any] = {
            "releaseId": "rel_01",
            "name": "The.Matrix.1999.2160p.UHD.BluRay.x265",
            "resolution": 2160,
            "hdr": ["DV", "HDR10"],
            "videoCodec": "hevc",
            "cached": True,
            "verified": True,
        }
        base.update(overrides)
        return base


def _encode_extra_path(extra: Optional[Dict[str, str]]) -> str:
    if not extra:
        return ""
    return "/" + urllib.parse.urlencode(extra)


class TestHarness:
    """Synchronous test harness for exercising an Ether `PluginInstance`."""

    def __init__(
        self,
        instance: PluginInstance,
        host: Optional[MockHostClient] = None,
    ) -> None:
        self.host = host or (
            instance._host if isinstance(instance._host, MockHostClient) else create_mock_host_client()
        )
        instance._host = self.host
        self.instance = instance

    def request(self, req: Dict[str, Any]) -> Dict[str, Any]:
        return self.instance.dispatch(req)

    def manifest(self) -> Dict[str, Any]:
        return self.instance.dispatch({"method": "GET", "path": "/manifest.json"})["body"]

    def health(self) -> Dict[str, Any]:
        return self.instance.dispatch({"method": "GET", "path": "/ether/health"})

    def configure(self, settings: Dict[str, Any]) -> Dict[str, Any]:
        return self.instance.dispatch(
            {"method": "POST", "path": "/ether/configure", "body": json.dumps(settings)}
        )

    def test(self) -> Dict[str, Any]:
        return self.instance.dispatch({"method": "POST", "path": "/ether/test"})

    def options(self, field_name: str) -> Dict[str, Any]:
        encoded = urllib.parse.quote(field_name, safe="")
        return self.instance.dispatch({"method": "GET", "path": f"/ether/options/{encoded}"})

    def catalog(
        self,
        media_type: str,
        catalog_id: str,
        extra: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        suffix = _encode_extra_path(extra)
        return self.instance.dispatch(
            {
                "method": "GET",
                "path": f"/catalog/{urllib.parse.quote(media_type)}/{urllib.parse.quote(catalog_id)}{suffix}.json",
            }
        )

    def meta(self, media_type: str, item_id: str) -> Dict[str, Any]:
        return self.instance.dispatch(
            {
                "method": "GET",
                "path": f"/meta/{urllib.parse.quote(media_type)}/{urllib.parse.quote(item_id)}.json",
            }
        )

    def stream(
        self,
        media_type: str,
        item_id: str,
        extra: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        suffix = _encode_extra_path(extra)
        return self.instance.dispatch(
            {
                "method": "GET",
                "path": f"/stream/{urllib.parse.quote(media_type)}/{urllib.parse.quote(item_id)}{suffix}.json",
            }
        )

    def subtitles(
        self,
        media_type: str,
        item_id: str,
        pick_token: Optional[str] = None,
        extra: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        suffix = _encode_extra_path(extra)
        headers: Dict[str, str] = {}
        if pick_token is not None:
            headers["X-Ether-Pick"] = pick_token
        return self.instance.dispatch(
            {
                "method": "GET",
                "path": f"/subtitles/{urllib.parse.quote(media_type)}/{urllib.parse.quote(item_id)}{suffix}.json",
                "headers": headers,
            }
        )

    def markers(self, media_type: str, item_id: str) -> Dict[str, Any]:
        return self.instance.dispatch(
            {
                "method": "GET",
                "path": f"/ether/markers/{urllib.parse.quote(media_type)}/{urllib.parse.quote(item_id)}.json",
            }
        )

    def event(self, events: List[Dict[str, Any]]) -> Dict[str, Any]:
        return self.instance.dispatch(
            {
                "method": "POST",
                "path": "/ether/event",
                "body": json.dumps({"events": events}),
            }
        )

    def flow_action(
        self,
        key: str,
        req: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        body = dict(req or {})
        route = f"/ether/v1/flow-actions/{urllib.parse.quote(key)}"
        ether_block = self.instance.manifest.get("ether")
        if isinstance(ether_block, dict):
            contributes = ether_block.get("contributes")
            if isinstance(contributes, dict):
                for fa in contributes.get("flowActions") or []:
                    if isinstance(fa, dict) and fa.get("key") == key and fa.get("route"):
                        route = str(fa["route"])
                        break
        headers: Dict[str, str] = {}
        if body.get("idempotencyKey"):
            headers[HEADER_IDEMPOTENCY_KEY] = str(body["idempotencyKey"])
        return self.instance.dispatch(
            {
                "method": "POST",
                "path": route,
                "headers": headers,
                "body": json.dumps(body),
            }
        )

    def view(
        self,
        page: str = "home",
        query: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        clean = page.lstrip("/") or "home"
        res = self.instance.dispatch(
            {
                "method": "GET",
                "path": f"/ether/view/{urllib.parse.quote(clean)}",
                "query": query,
            }
        )
        if res["status"] == 200:
            assert_valid_view_spec(res["body"]["spec"])
        return res

    def action(
        self,
        action_name: str,
        input_payload: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        return self.instance.dispatch(
            {
                "method": "POST",
                "path": f"/ether/action/{urllib.parse.quote(action_name)}",
                "body": json.dumps(input_payload or {}),
            }
        )

    def tool(
        self,
        name: str,
        input_payload: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        return self.instance.dispatch(
            {
                "method": "POST",
                "path": f"/ether/tool/{urllib.parse.quote(name)}",
                "body": json.dumps(input_payload or {}),
            }
        )

    def rank_key(
        self,
        candidate: Dict[str, Any],
        config: Optional[Dict[str, Any]] = None,
    ) -> float:
        return self.instance.rank_key(candidate, config)

    def cond_match(
        self,
        candidate: Dict[str, Any],
        operator: str = "eq",
        value: Any = None,
    ) -> bool:
        return self.instance.cond_match(candidate, operator, value)

    def file(self, path: str, range_header: Optional[str] = None) -> Dict[str, Any]:
        headers: Dict[str, str] = {}
        if range_header:
            headers["Range"] = range_header
        clean = path.lstrip("/")
        return self.instance.dispatch(
            {"method": "GET", "path": f"/ether/file/{clean}", "headers": headers}
        )


def create_test_harness(
    instance: PluginInstance,
    host: Optional[MockHostClient] = None,
) -> TestHarness:
    return TestHarness(instance=instance, host=host)


# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# Content kinds: base classes, host services, kit helpers (Ether v0.3.8)
# ---------------------------------------------------------------------------

MAX_KIND_LIBRARY_SHELVES = 40


class KindLevel(TypedDict, total=False):
    """One hierarchy level of a contributed media kind."""

    key: str
    label: str
    parent: str
    media: Literal["video", "music", "audiobook", "book", "podcast", "photo"]
    role: str
    playable: bool
    container: bool
    members: bool


class KindLibraryShelf(TypedDict, total=False):
    """One default Recommended shelf declared under ``library.shelves`` or a recommended tab's ``shelves`` (max 40).

    Each shelf gets the stable row ID ``plugin:<viewID>:shelf:<key>`` (or
    ``plugin:<viewID>:tab:<tabId>:shelf:<key>`` on per-tab shelves).
    """

    key: str
    label: str
    level: str
    sort: Literal["title", "added", "released", "year", "rating", "index", "lastViewed"]
    direction: Literal["asc", "desc"]
    facet: str
    value: str
    values: List[str]
    filters: Any
    purpose: Literal["recentContexts", "liked"]
    layout: Literal["standard", "hero", "wide", "tiles", "tracks"]


class KindLibraryTab(TypedDict, total=False):
    """One sub-tab declared on a kind library (up to 50)."""

    key: str
    label: str
    kind: Literal["recommended", "library", "collections", "people", "view", "live"]
    level: str
    facet: str
    order: Literal["count", "name"]
    card: Literal["poster", "landscape", "square"]
    layout: Literal["grid", "list", "timeline"]
    search: bool
    searchPlaceholder: str
    sort: Literal["title", "added", "released", "year", "rating", "index", "lastViewed"]
    direction: Literal["asc", "desc"]
    filters: Dict[str, Any]
    shelves: List[KindLibraryShelf]


class KindBrowseExpand(TypedDict, total=False):
    """On-demand expansion settings for declared browse facets and container levels."""

    facets: List[str]
    levels: List[str]
    ttlHours: int


class KindBrowseDecl(TypedDict, total=False):
    """Browse configuration on a contributed content kind."""

    sorts: List[str]
    facets: List[str]
    search: bool
    searchFields: List[str]
    liveSearch: bool
    expand: KindBrowseExpand


class KindBrowseFacetAlt(TypedDict):
    """One {field, values} alternative inside a cross-field KindBrowseFacetGroup (#807)."""

    field: str
    values: List[str]


class KindBrowseFacetGroup(TypedDict, total=False):
    """One OR-group for KindBrowseRequest.anyGroups (#767, #807).

    Supply either ``field`` + ``values`` for one field or ``any`` with a list
    of ``{field, values}`` alternatives ORed across fields.
    """

    field: str
    values: List[str]
    any: List[KindBrowseFacetAlt]


class KindBrowseQuery(TypedDict, total=False):
    """Query passed to kinds.browse from a frontend adapter (#615, #767, #807)."""

    parentId: str
    level: str
    cursor: str
    limit: int
    sort: str
    desc: bool
    facets: Dict[str, List[str]]
    allFacets: Dict[str, List[str]]
    excludeFacets: Dict[str, List[str]]
    anyGroups: List[KindBrowseFacetGroup]
    empty: Dict[str, bool]
    query: str
    excludeQuery: str
    total: Literal["cheap", "none", "exact"]


class KindBrowseRequest(KindBrowseQuery, total=False):
    """Request body for POST /ether/v1/kinds/{kind}/browse or /search."""

    kind: str


class KindExpandRequest(TypedDict, total=False):
    """Request body for POST /ether/v1/kinds/{kind}/expand.

    For facet/person expansion: facet, value, optional valueId, optional cursor.
    For container expansion (browse.expand.levels): parentId (container externalId),
    level (the container's own level key), optional cursor.
    """

    kind: str
    facet: str
    value: str
    valueId: int
    parentId: str
    level: str
    cursor: str


class KindExpandResponse(TypedDict, total=False):
    """Response body for POST /ether/v1/kinds/{kind}/expand."""

    items: List[Dict[str, Any]]
    cursor: str
    nextCursor: str
    job: str


class KindLibraryDecl(TypedDict, total=False):
    """Library presentation declaration on a contributed content kind."""

    name: str
    icon: str
    card: Literal["poster", "landscape", "square"]
    dateDisplay: Literal["year", "date"]
    experience: str  # watch | listen | read | gallery | live, or another id
    tabs: List[KindLibraryTab]
    shelves: List[KindLibraryShelf]


class ScreenEventQuery(TypedDict, total=False):
    """Live event filter query for a screen section."""

    feed: str
    state: List[Literal["pre", "in", "post", "delayed", "postponed", "canceled"]]
    league: str
    favorites: bool
    window: Literal["today", "week", "recent"]
    category: str


class ScreenLeague(TypedDict, total=False):
    """Optional static league filter chip on a leagues section."""

    id: str
    name: str
    abbr: str
    logo: str


class ScreenSection(TypedDict, total=False):
    """One section declared inside a plugin screen."""

    id: str
    type: Literal["hero", "events", "table", "schedule", "leagues", "entities"]
    title: str
    feed: str
    league: str
    entityKind: str
    limit: int
    query: ScreenEventQuery
    leagues: List[ScreenLeague]


class ScreenMeta(TypedDict, total=False):
    """One generic top-level screen contributed under contributes.screens."""

    id: str
    title: str
    icon: str
    feed: str
    sections: List[ScreenSection]
    frontends: List[str]


class UsenetCheckResult(TypedDict, total=False):
    """Sampled Usenet article availability verdict for one NZB (usenet_check)."""

    available: bool
    checked: int
    missing: int
    cached: bool
    playbackFailed: bool
    reason: str
    checkedAt: int


class ReleaseFile(TypedDict, total=False):
    """One file inside a candidate release returned by release_files."""

    index: int
    path: str
    size: int
    sizeApproximate: bool


class DebridServiceRef(TypedDict):
    """One owner-configured debrid service holding a cached torrent or serving debrid_acquire."""

    id: str
    name: str


class SourceCandidate(TypedDict, total=False):
    """One Ether-found release candidate returned by sources.resolve.

    Search returns opaque ``nzbRef`` and ``torrentRef`` values. The deprecated
    ``nzbUrl`` and ``torrentUrl`` fields mirror them with an ``ether-nzb:``
    or ``ether-torrent:`` prefix.
    """

    infoHash: str
    magnet: str
    torrentUrl: str
    nzbUrl: str
    nzbRef: str
    torrentRef: str
    fileIndex: int
    title: str
    size: int
    seeders: int
    cached: bool
    cachedBy: List[DebridServiceRef]
    indexer: str
    indexerId: str
    score: int
    resolution: int
    source: str
    codec: str
    files: List[ReleaseFile]
    fileCount: int
    usenetCheck: UsenetCheckResult


class IndexerRef(TypedDict, total=False):
    """Enabled indexer identity in the top-level pending list."""

    indexerId: str
    indexer: str


class IndexerSearchStatus(TypedDict, total=False):
    """Per-indexer outcome entry (indexer_search / streaming) or enabled indexer summary (kind='indexers')."""

    indexerId: str
    indexer: str
    status: Literal["answered", "pending", "timed_out", "skipped", "error"]
    results: int
    ms: int
    reason: str
    error: str
    kind: Literal["newznab", "torznab", "builtin", "stremio", "zilean", "addon"]
    medianMs: int
    lastMs: int
    lastError: str


class SourceResolveResult(TypedDict, total=False):
    """Full response envelope from sources.resolve."""

    candidates: List[SourceCandidate]
    indexers: List[IndexerSearchStatus]
    pending: List[IndexerRef]
    searchId: str
    done: bool


class NZBIdentity(TypedDict):
    indexerId: str
    guid: str


class SourceQuery(TypedDict, total=False):
    """Host sources.resolve query payload."""

    kind: Literal[
        "indexer_search",
        "indexer_search_results",
        "indexer_search_cancel",
        "indexers",
        "debrid_check",
        "debrid_acquire",
        "release_files",
        "usenet_check",
        "nzb_request",
    ]
    query: str
    category: str
    profile: str
    stream: bool
    searchId: str
    waitMs: int
    firstResults: bool
    settleMs: int
    minResults: int
    skipIndexers: List[str]
    onlyIndexers: List[str]
    timeoutMs: int
    infoHashes: List[str]
    infoHash: str
    magnet: str
    torrentUrl: str
    nzbUrl: str
    nzbRef: str
    torrentRef: str
    nzbIdentity: NZBIdentity
    candidate: SourceCandidate
    fileCount: int


class TrackHint(TypedDict, total=False):
    """Structured track hint inside a multi-track audio release or archive."""

    disc: int
    number: int
    title: str


class VolumeRef(TypedDict, total=False):
    """One archive volume located by URL or blobcache reference."""

    url: str
    headers: Dict[str, str]
    keepHeadersOnRedirect: List[str]
    blobRef: str
    size: int


class ContainerRef(TypedDict, total=False):
    """Archive member selection inside one or more ordered volumes."""

    format: str
    volumes: List[VolumeRef]
    member: str
    memberIndex: int
    passwordSecret: str


class ReleaseHint(TypedDict, total=False):
    """Release handed to Ether's Usenet / debrid / torrent acquisition pipeline."""

    fileIndex: int
    nzbUrl: str
    nzbRef: str
    torrentRef: str
    magnet: str
    infoHash: str
    torrentUrl: str
    candidate: SourceCandidate
    file: str
    fileHint: str
    track: TrackHint
    passwordSecret: str
    cachedOnly: bool
    fileCount: int


class PlayableSource(TypedDict, total=False):
    """One candidate way to acquire bytes for a content-kind item.

    Set ``tier`` to the same integer on interchangeable sources of equal
    quality so Ether can apply its source routing (auto/usenet/debrid,
    measured startup, and stall demotion) within that tier while preserving
    the plugin's order across tiers. Omit ``tier`` to keep exact plugin order.
    """

    path: str
    id: str
    label: str
    pattern: Literal["range", "download", "direct", "container", "release", "file"]
    mime: str
    tier: int
    size: int
    sizeApproximate: bool
    durationSec: float
    durationToleranceSec: float
    url: str
    headers: Dict[str, str]
    keepHeadersOnRedirect: List[str]
    blobRef: str
    sha256: str
    sha1: str
    crc32: str
    maxBytes: int
    expiresAt: int
    container: ContainerRef
    release: ReleaseHint
    parts: List[Dict[str, Any]]


class CatalogProvider:
    """Serve browse/detail for one declared kind. Items are plain dicts in
    the KindItem wire shape; requests are dicts with an extra ``kind`` key."""

    def browse(self, ctx: PluginContext, req: Dict[str, Any]) -> Dict[str, Any]:
        raise NotImplementedError

    def detail(self, ctx: PluginContext, kind: str, item_id: str) -> Dict[str, Any]:
        raise NotImplementedError

    # Optional: define `changes(ctx, req)` for delta pull sync,
    # `search(ctx, req)` when the kind declares browse.liveSearch, and
    # `expand(ctx, req)` when the kind declares browse.expand.


class KindResolver:
    """Turn a playable item into ordered playable sources."""

    def resolve(self, ctx: PluginContext, req: Dict[str, Any]) -> Dict[str, Any]:
        raise NotImplementedError


class KindMatcher:
    """Score source candidates for one item (spec: matches, best first)."""

    def match(self, ctx: PluginContext, req: Dict[str, Any]) -> Dict[str, Any]:
        raise NotImplementedError


class KindLiveProvider:
    """Serve a kind's declared "live" tab: list currently live streams and
    resolve a live item ID into a playable stream URL."""

    def live(self, ctx: PluginContext, req: Dict[str, Any]) -> Dict[str, Any]:
        raise NotImplementedError

    def resolve_live(self, ctx: PluginContext, req: Dict[str, Any]) -> Dict[str, Any]:
        raise NotImplementedError


class FrontendAdapterMeta(TypedDict, total=False):
    """One ``contributes.frontendAdapters[]`` manifest entry."""

    key: str
    label: str
    protocolHint: Literal["opds", "subsonic", "custom"]
    pathPrefix: str
    defaultPort: int
    clients: List[str]
    auth: Literal["ether_token", "basic", "none_lan", "query_credentials"]
    libraries: List[str]
    searchPaths: List[str]
    # Path to an SVG or PNG inside the package, shown as the adapter's mark
    # on the Apps page. Without one the plugin's icon stands in.
    icon: str


class FrontendAdapter:
    """A plugin-provided protocol surface. Ether authenticates first;
    `handle` receives X-Ether-Viewer headers and never a credential."""

    def handle(
        self, method: str, path: str, headers: Dict[str, str], body: bytes
    ) -> Tuple[int, Dict[str, str], Union[bytes, str]]:
        raise NotImplementedError


class SetupTester:
    """Answer /ether/v1/setup/{step}/test with {ok, detail, fields?}."""

    def test_setup(self, ctx: PluginContext, req: Dict[str, Any]) -> Dict[str, Any]:
        raise NotImplementedError


# Spec-name aliases.
Matcher = KindMatcher


def candidate_ref(candidate: Dict[str, Any]) -> str:
    """The stable ref matchers report for a source candidate."""
    if candidate.get("infoHash"):
        return "infohash:" + str(candidate["infoHash"]).lower()
    if candidate.get("magnet"):
        return "magnet:" + str(candidate["magnet"])
    if candidate.get("torrentUrl"):
        return "torrent:" + str(candidate["torrentUrl"])
    if candidate.get("nzbUrl"):
        return "nzb:" + str(candidate["nzbUrl"])
    return str(candidate.get("title") or "")


def stream_url(
    ctx: PluginContext,
    item_ref: str,
    part_index: Optional[int] = None,
    source: Optional[str] = None,
    absolute: bool = False,
) -> str:
    """Mint an Ether stream URL for one of this plugin's own kind items
    (spec §6.5), optionally pinned to a zero-based part (#576), a resolver
    PlayableSource.id (#661), and/or resolved to an absolute URL (#772). Kept
    as a thin wrapper over PluginContext.stream_url."""
    return ctx.stream_url(item_ref, part_index=part_index, source=source, absolute=absolute)


def kinds_visible(
    ctx: PluginContext,
    kind: str,
    ids: List[str],
) -> List[str]:
    """Evaluate the library's Rules and viewer access policy for a bounded
    batch (at most 500) of external IDs in `kind` (#615)."""
    return ctx.kinds_visible(kind, ids)


def kinds_browse(
    ctx: PluginContext,
    kind: str,
    query: Optional[Dict[str, Any]] = None,
) -> Dict[str, Any]:
    """Browse or search `kind` with the library's Rules and viewer access
    policy applied for the requesting frontend-adapter viewer (#615)."""
    return ctx.kinds_browse(kind, query=query)


class _KindsClient:
    def __init__(
        self,
        call_json: Callable[..., Any],
        default_viewer_token: Optional[Callable[[], str]] = None,
    ) -> None:
        self._call = call_json
        self._default_viewer_token = default_viewer_token

    def _resolve_viewer_token(self, viewer_token: Optional[str]) -> str:
        tok = viewer_token if viewer_token is not None else (
            self._default_viewer_token() if self._default_viewer_token else ""
        )
        return str(tok or "").strip()

    def upsert(self, kind: str, items: List[Dict[str, Any]]) -> int:
        result = self._call("POST", f"/ether/plugin-host/kinds/{urllib.parse.quote(kind)}/upsert", {"items": items})
        return int((result or {}).get("accepted") or 0)

    def delete(self, kind: str, external_ids: List[str]) -> None:
        self._call("POST", f"/ether/plugin-host/kinds/{urllib.parse.quote(kind)}/delete", {"externalIds": external_ids})

    def items(self, kind: str, cursor: str = "", limit: int = 200) -> Dict[str, Any]:
        q = urllib.parse.urlencode({"cursor": cursor, "limit": limit})
        return self._call("GET", f"/ether/plugin-host/kinds/{urllib.parse.quote(kind)}/items?{q}") or {"items": []}

    def visible(
        self,
        kind: str,
        ids: List[str],
        viewer_token: Optional[str] = None,
    ) -> List[str]:
        tok = self._resolve_viewer_token(viewer_token)
        if not tok:
            raise RuntimeError("kinds.visible: no authenticated frontend adapter viewer token on this request")
        if len(ids) > MAX_KIND_VISIBLE_IDS:
            raise ValueError(f"kinds.visible: batch larger than {MAX_KIND_VISIBLE_IDS} ids")
        result = self._call(
            "POST",
            f"/ether/plugin-host/kinds/{urllib.parse.quote(kind, safe='')}/visible",
            {"ids": list(ids)},
            extra_headers={HEADER_VIEWER_TOKEN: tok},
        )
        return list((result or {}).get("ids") or [])

    def browse(
        self,
        kind: str,
        query: Optional[Union[KindBrowseQuery, Dict[str, Any]]] = None,
        viewer_token: Optional[str] = None,
    ) -> Dict[str, Any]:
        tok = self._resolve_viewer_token(viewer_token)
        if not tok:
            raise RuntimeError("kinds.browse: no authenticated frontend adapter viewer token on this request")
        result = self._call(
            "POST",
            f"/ether/plugin-host/kinds/{urllib.parse.quote(kind, safe='')}/browse",
            dict(query or {}),
            extra_headers={HEADER_VIEWER_TOKEN: tok},
        )
        return result or {"items": []}

    def resolve_sources(self, query: Union[SourceQuery, Dict[str, Any]]) -> List[SourceCandidate]:
        """Find or inspect owner sources via ``sources.resolve``.

        Supported ``kind`` values:
        - ``indexer_search``: accepts ``category`` (comma-separated Newznab IDs
          or family names, e.g. music/books/comics; with empty ``query``,
          fetches up to 100 recent releases) and ``profile`` (quality profile
          name for video release scoring; default uses owner default). Video
          candidates include ``score``, ``resolution``, ``source`` and ``codec``
          alongside ``indexer``/``indexerId``; non-video candidates omit those
          video fields. Use the result's opaque ``nzbRef`` or ``torrentRef``.
        - ``nzb_request``: return a file list and a vault reference using
          ``nzbRef`` or ``nzbIdentity``.
        - ``debrid_check`` / ``debrid_acquire``: check or acquire torrents.
        - ``release_files``: list files inside a torrent or NZB candidate.
        - ``usenet_check``: sampled NNTP article check for a Usenet
          ``candidate`` (or ``nzbUrl``). Returns one candidate with
          ``usenetCheck`` (``{available, checked, missing, cached?,
          playbackFailed?, reason?, checkedAt?}``), sharing Ether's bounded
          per-NZB verdict cache (good 15 min, bad 30 min) and playback-failure
          memory at background NNTP priority. On-demand viewer calls require
          ``permissions.sources: "search"`` (or ``"acquire"``) and never count
          against the daily background allowance; background job/schedule calls
          require ``"search+check"`` or ``"acquire+check"`` and count against
          the per-plugin daily allowance (default 20/day), answering
          ``rate_limited`` with ``retry_after_sec`` / ``retry_at`` once spent.
        """
        result = self._call("POST", "/ether/plugin-host/sources/resolve", dict(query))
        return list((result or {}).get("candidates") or [])

    def request_nzb(self, release: Union[str, NZBIdentity]) -> SourceCandidate:
        """Return a file list and permanent vault reference for an indexer NZB."""
        query: SourceQuery = {"kind": "nzb_request"}
        if isinstance(release, str):
            query["nzbRef"] = release
        else:
            query["nzbIdentity"] = release
        result = self.resolve_sources(query)
        if not result or not result[0].get("nzbRef"):
            raise ValueError("nzb_request returned no vault reference")
        return result[0]

    def resolve_sources_with_result(self, query: Union[SourceQuery, Dict[str, Any]]) -> SourceResolveResult:
        """Run ``sources.resolve`` and return the full envelope (``candidates``, ``indexers``, ``pending``, ``searchId``, ``done``) (#733)."""
        result = self._call("POST", "/ether/plugin-host/sources/resolve", dict(query)) or {}
        out: Dict[str, Any] = {"candidates": list(result.get("candidates") or [])}
        if "indexers" in result and result["indexers"] is not None:
            out["indexers"] = list(result["indexers"])
        if "pending" in result and result["pending"] is not None:
            out["pending"] = list(result["pending"])
        if "searchId" in result and result["searchId"] is not None:
            out["searchId"] = str(result["searchId"])
        if "done" in result and result["done"] is not None:
            out["done"] = bool(result["done"])
        return out  # type: ignore[return-value]


def archive_list(data: bytes) -> List[Dict[str, Any]]:
    """List non-directory members of a zip/cbz/epub in archive order."""
    import io
    import zipfile

    out: List[Dict[str, Any]] = []
    with zipfile.ZipFile(io.BytesIO(data)) as zf:
        for info in zf.infolist():
            if not info.is_dir():
                out.append({"index": len(out), "name": info.filename, "size": info.file_size})
    return out


def archive_read_member(data: bytes, name: str) -> bytes:
    import io
    import zipfile

    with zipfile.ZipFile(io.BytesIO(data)) as zf:
        return zf.read(name)


def parse_feed(data: Union[bytes, str]) -> Dict[str, Any]:
    """Parse RSS/Atom/JSON Feed into {title, summary, link, entries:[...]}."""
    if isinstance(data, bytes):
        data = data.decode("utf-8", errors="replace")
    if data.lstrip().startswith("{"):
        doc = json.loads(data)
        return {
            "title": doc.get("title", ""), "summary": doc.get("description", ""),
            "link": doc.get("home_page_url", ""),
            "entries": [
                {
                    "id": it.get("id") or it.get("url", ""), "title": it.get("title", ""),
                    "summary": it.get("summary", ""), "link": it.get("url", ""),
                    "audio": (it.get("attachments") or [{}])[0].get("url", ""),
                    "date": it.get("date_published", ""),
                }
                for it in doc.get("items") or []
            ],
        }
    import xml.etree.ElementTree as ET

    root = ET.fromstring(data)

    def local(tag: str) -> str:
        return tag.rsplit("}", 1)[-1]

    def child_text(el: Any, name: str) -> str:
        for c in el:
            if local(c.tag) == name:
                return (c.text or "").strip()
        return ""

    if local(root.tag) == "feed":
        entries = []
        for e in root.iter():
            if local(e.tag) != "entry":
                continue
            page = media = ""
            for c in e:
                if local(c.tag) == "link":
                    if c.get("rel") == "enclosure":
                        media = c.get("href", "")
                    elif not page:
                        page = c.get("href", "")
            entries.append({"id": child_text(e, "id") or page, "title": child_text(e, "title"),
                            "summary": child_text(e, "summary"), "link": page, "audio": media,
                            "date": child_text(e, "updated")})
        return {"title": child_text(root, "title"), "summary": child_text(root, "subtitle"), "link": "", "entries": entries}
    channel = root
    for e in root.iter():
        if local(e.tag) == "channel":
            channel = e
            break
    entries = []
    for e in channel:
        if local(e.tag) != "item":
            continue
        enc = ""
        for c in e:
            if local(c.tag) == "enclosure":
                enc = c.get("url", "")
        entries.append({"id": child_text(e, "guid") or child_text(e, "link"), "title": child_text(e, "title"),
                        "summary": child_text(e, "description"), "link": child_text(e, "link"),
                        "audio": enc, "date": child_text(e, "pubDate")})
    return {"title": child_text(channel, "title"), "summary": child_text(channel, "description"),
            "link": child_text(channel, "link"), "entries": entries}


# Capability starter templates
# ---------------------------------------------------------------------------


def _base_manifest(
    plugin_id: str,
    name: str,
    kind: str,
    capabilities: List[str],
    version: str = "0.1.0",
    summary: Optional[str] = None,
) -> Dict[str, Any]:
    resolved_summary = summary or f"{name} plugin for Ether."
    return {
        "id": plugin_id,
        "name": name,
        "version": version,
        "description": resolved_summary,
        "types": ["movie", "series"],
        "idPrefixes": ["tt"],
        "ether": {
            "protocol": 1,
            "kind": kind,
            "capabilities": capabilities,
            "summary": resolved_summary,
            "runtime": {"kind": "remote"},
            "permissions": {"catalog": "none"},
        },
    }


def create_catalog_template(
    plugin_id: str,
    name: str,
    version: str = "0.1.0",
) -> PluginInstance:
    man = _base_manifest(plugin_id, name, "extension", ["list_source"], version=version)
    man["catalogs"] = [
        {
            "type": "movie",
            "id": "featured",
            "name": "Featured movies",
            "extra": [{"name": "search", "isRequired": False}],
        }
    ]
    man["resources"] = ["catalog", "meta"]
    man["ether"]["settings"] = {
        "fields": [
            {
                "key": "apiKey",
                "kind": "secret",
                "label": "Access token",
                "hint": "Token used to query the upstream catalog",
            }
        ]
    }
    man["ether"]["contributes"] = {
        "listSources": [
            {
                "key": "featured",
                "label": "Featured movies",
                "hint": "Curated movies from the upstream list",
                "catalog": "featured",
                "refresh": "1h",
            }
        ]
    }
    return plugin(
        manifest=man,
        routes={
            "catalog": lambda req, _ctx: {
                "metas": [
                    {
                        "id": "tt0133093",
                        "type": req["type"],
                        "name": (
                            f"Result for {req['search']}"
                            if req.get("search")
                            else f"Featured in {req['id']}"
                        ),
                        "releaseInfo": "1999",
                    }
                ]
            },
            "meta": lambda req, _ctx: {
                "meta": {
                    "id": req["id"],
                    "type": req["type"],
                    "name": f"Title {req['id']}",
                    "releaseInfo": "1999",
                    "description": "Catalog metadata entry.",
                }
            },
        },
    )


def create_subtitles_template(
    plugin_id: str,
    name: str,
    version: str = "0.1.0",
) -> PluginInstance:
    man = _base_manifest(plugin_id, name, "extension", ["subtitles"], version=version)
    man["resources"] = ["subtitles"]
    man["ether"]["settings"] = {
        "fields": [
            {
                "key": "preferredLanguage",
                "kind": "select",
                "label": "Preferred language",
                "default": "en",
                "options": [
                    {"value": "en", "label": "English"},
                    {"value": "es", "label": "Spanish"},
                    {"value": "fr", "label": "French"},
                ],
            }
        ]
    }
    man["ether"]["contributes"] = {
        "subtitles": {"languages": ["en", "es", "fr"], "formats": ["vtt", "srt"]}
    }

    def _subtitles(req: Dict[str, Any], ctx: PluginContext) -> Dict[str, Any]:
        lang = str(ctx.settings.get("preferredLanguage") or "en")
        encoded = urllib.parse.quote(req["id"], safe="")
        return {
            "subtitles": [
                {
                    "id": f"{req['id']}:{lang}",
                    "url": f"https://subtitles.example.invalid/{encoded}/{lang}.vtt",
                    "lang": lang,
                    "label": lang.upper(),
                    "format": "vtt",
                }
            ]
        }

    return plugin(
        manifest=man,
        default_settings={"preferredLanguage": "en"},
        routes={"subtitles": _subtitles},
    )


def create_scrobbler_template(
    plugin_id: str,
    name: str,
    version: str = "0.1.0",
) -> PluginInstance:
    man = _base_manifest(plugin_id, name, "integration", ["events"], version=version)
    man["ether"]["contributes"] = {
        "integration": {"category": "scrobbler", "adds": ["events"], "connect": "none"},
        "events": {
            "subscribe": [
                "playback.started",
                "playback.paused",
                "playback.stopped",
                "playback.completed",
            ]
        },
    }

    def _event(events: List[Dict[str, Any]], ctx: PluginContext) -> Dict[str, Any]:
        ack: List[str] = []
        for ev in events:
            viewer_id = ev.get("viewerId") or 0
            ctx.ether.kv.put(f"last_event:{viewer_id}", json.dumps(ev))
            ack.append(str(ev["id"]))
        return {"ack": ack}

    return plugin(manifest=man, routes={"event": _event})


def create_view_template(
    plugin_id: str,
    name: str,
    version: str = "0.1.0",
) -> PluginInstance:
    man = _base_manifest(plugin_id, name, "extension", ["ui"], version=version)
    man["ether"]["contributes"] = {
        "pages": [{"key": "home", "label": name, "route": "/ether/view/home"}]
    }

    def _view(_req: Dict[str, Any], ctx: PluginContext) -> Dict[str, Any]:
        raw = ctx.ether.kv.get("refresh_count")
        count = int(raw) if raw and str(raw).isdigit() else 0
        return {
            "spec": {
                "specVersion": 1,
                "layout": "list",
                "header": [
                    {
                        "kind": "page-header",
                        "title": {"$": "title"},
                        "description": {"$": "subtitle"},
                    }
                ],
                "body": [
                    {
                        "kind": "panel",
                        "title": "Status",
                        "blocks": [{"kind": "text", "content": {"$": "statusText"}}],
                    }
                ],
            },
            "data": {
                "title": name,
                "subtitle": f"Refreshed {count} times",
                "statusText": "Plugin view is ready.",
            },
        }

    def _action(req: Dict[str, Any], ctx: PluginContext) -> Dict[str, Any]:
        if req.get("action") == "refresh":
            raw = ctx.ether.kv.get("refresh_count")
            current = (int(raw) if raw and str(raw).isdigit() else 0) + 1
            ctx.ether.kv.put("refresh_count", str(current))
            return {"toast": "State refreshed.", "refresh": True}
        return {"toast": "Unknown action."}

    return plugin(manifest=man, routes={"view": _view, "action": _action})


def create_ranker_template(
    plugin_id: str,
    name: str,
    version: str = "0.1.0",
) -> PluginInstance:
    man = _base_manifest(plugin_id, name, "extension", ["ranking_step"], version=version)
    man["ether"]["settings"] = {
        "fields": [
            {
                "key": "boost4k",
                "kind": "bool",
                "label": "Boost 2160p releases",
                "default": True,
            }
        ]
    }
    man["ether"]["contributes"] = {
        "rankingSteps": [
            {
                "key": "resolution_boost",
                "label": "Boost 2160p releases",
                "hint": "Adds a score bonus when a candidate is 2160p",
            }
        ]
    }

    def _rank_key(candidate: Dict[str, Any], config: Dict[str, Any]) -> int:
        res = candidate.get("resolution")
        is_4k = res == 2160 or res == "2160p"
        bonus = 1000 if config.get("boost4k") and is_4k else 0
        return (500 if candidate.get("cached") else 0) + bonus

    return plugin(
        manifest=man,
        default_settings={"boost4k": True},
        routes={"rank_key": _rank_key},
    )


def create_condition_template(
    plugin_id: str,
    name: str,
    version: str = "0.1.0",
) -> PluginInstance:
    man = _base_manifest(plugin_id, name, "extension", ["rule_condition"], version=version)
    man["ether"]["contributes"] = {
        "ruleConditions": [
            {
                "key": "cached_release",
                "label": "Release is cached",
                "operators": ["eq"],
            }
        ]
    }

    def _cond_match(candidate: Dict[str, Any], _op: str, value: Any) -> bool:
        want = value if isinstance(value, bool) else True
        return bool(candidate.get("cached")) is want

    return plugin(manifest=man, routes={"cond_match": _cond_match})
