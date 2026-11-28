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

## Cache hints

A source or integration plugin that knows which torrents a debrid service
already holds may declare `debrid_cache_hints`. It needs no contribution block
and no debrid account of its own. Ether calls `POST /ether/debrid/hints` only
when source preparation checks the shortlist for one of two starts: Vela's
playback request and Silo's playback start, including one that names a
version (not a queued next item). Nothing else asks: not Jellyfin or Emby
clients (some send `IsPlayback` true for queue prefetches, so a `PlaybackInfo`
does not prove a Play), not Plex clients, a lazy stream fetch or file probe,
WebDAV, Stremio or Xtream, a detail page, a console search, a warm-up or
prefetch, a download, the next episode or a queue advance, a version switch
during playback, failover to another release or a background check. It is not
called either when the lead torrent is already known cached, in memory or by
an account's remembered answer.

```json
{"candidates": [
  {"infoHash": "0123456789abcdef0123456789abcdef01234567", "private": false, "providerKinds": ["realdebrid", "torbox"]},
  {"infoHash": "89abcdef0123456789abcdef0123456789abcdef", "private": true, "providerKinds": ["realdebrid"]}
]}
```

The request lists at most the 12 torrents preparation looks at. For each hash,
`providerKinds` names the enabled debrid services (`realdebrid`, `torbox`,
`alldebrid`, `premiumize`, `debridlink`, `offcloud`, or `plugin:<id>` for a
plugin provider) with an account that has no answer of its own for that hash,
neither a recent check nor an answer the account remembers from an add;
hashes every service has answered are left out. `private` is true when any
copy of the torrent comes from a private tracker. A plugin that sends hashes to
an outside service decides what it sends there: leave private hashes out unless
the owner opted in.

```json
{"cached": {"0123456789abcdef0123456789abcdef01234567": ["realdebrid"]}}
```

List only the hashes and kinds known to be cached. Ether keeps only the pairs it
asked about and treats them like an add-on's cached marker: a hint in memory for
30 minutes that never replaces an account's own answer, and an account with a
hint is tried before the others when the torrent is added. A miss, an error or a
missing hash writes nothing. One 500 ms deadline, started before Ether reads
what it knows, covers every hint plugin together; a plugin whose instances are
all busy is skipped, and an answer after the deadline is ignored. A timeout, on
that deadline or on the route's own, does not count against the plugin's health.
Answer from what the plugin already knows or can learn at once, and never wait
for a rate limiter on this route.
