# Native debrid providers

A hosted source plugin may declare `debrid_provider` and
`ether.contributes.source.class: "debrid"`. Its account belongs in secret settings,
read with `ether.secret.get`. It registers automatically with the native debrid
pipeline while enabled; disabling or removing the plugin unregisters it.

The host sends JSON POST requests to these routes:

| Route under `/ether/debrid/` | Request | Response |
| --- | --- | --- |
| `check` | `{hashes: string[]}` | `{cached: Record<string, boolean>}` |
| `find` | `{infoHash}` | `{torrentId}` |
| `acquire` | `{infoHash, magnet, torrentId, cachedOnly}` | `{torrentId, ready, added, files: [{id, path, size, link?}]}` |
| `link` | `{torrentId, fileId, path, link, magnet, infoHash, size}` | `{url, size, expires?}` |
| `remove` | `{torrentId}` | `{}` |
| `account` | `{}` | `{user, premium, expires?}` |

Times are RFC3339. File sizes are bytes. `added` must be true only when this
request created the provider torrent. The host selects the movie or episode
from `files`; it never sends executable selection callbacks to a plugin.
Unready acquisitions become cache misses. Cleanup only applies to newly added
torrents when the caller's removal policy permits it. A cached-only service may
return HTTP 501 for `find` and `remove`.

HTTP 401/403 means invalid credentials, 404/410 gone, 409 not cached, 429 rate
limited, and 501 unsupported. Other failures use a generic provider error.
Plugin links must be public HTTP(S) URLs; both link validation and every network
dial/redirect use the guarded transport. Native retry, cache, limiter and file
selection behavior applies. Plugin providers currently run after configured
built-in providers at priority 1000; source-card display order does not change
that priority.

Provide `/ether/health` with `status: "attention"` and a useful `reason` when
credentials are absent. `/ether/test` should verify the account and return
`{ok, message}` without exposing credentials or signed URLs. Offline installation
checks cannot establish that a paid account works; test it after setup.
