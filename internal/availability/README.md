# Availability monitoring

Ether observes recent releases and reuses successful resolver health checks. It
never removes catalogue/search results because evidence is missing. Movie dates
and episode air dates still use the catalogue's existing release eligibility;
quality, CAM acceptance, identities, blocklists and episode targeting use the
resolver's existing policies rather than a second rules engine.

## Owner controls

Sources → Availability provides monitoring, selective validation, participating
feeds, debrid participation, preference for checked recommendations and optional
newly available shelves. Monitoring and validation start disabled. Presentation
preferences start enabled and can benefit from ordinary playback observations.

Default daily background allowances (UTC, shared across nodes):

- Per indexer: 10% (Gentle 5%; Unlimited up to 100 − `reservePercent`) of its
  daily API limit: the owner's, else the one it reports (Newznab `apilimits`),
  else 1,000 (the strictest private tier), so 100 feed and search requests by
  default. The day's remaining head polls are reserved, so gap pages and title
  searches cannot starve the feeds; a feed whose allowance cannot fit 30-minute
  polls in half of it is polled less often. Requests to one indexer are at least
  `Deps.Pace` (3 s) apart.
- In total, the sum of those (or the owner's custom total).
- 100 validation API requests, 20 candidate checks and 20 descriptor grabs.
- 512 MiB of conservative data reservations. NNTP BODY reservations use the
  provider's maximum article size, so actual transferred bytes can be much lower.

An allowance of zero disables that work. Foreground requests still have their
normal account limits. Every feed page, provider API request and source read goes
through admission before network work. Ordinary playback does not use the
background allowance. Shared worker contexts retain the admission hook.

## Discovery and evidence

Newznab/Torznab recent-release pages use an empty `t=search` request, following the
incremental model documented by [Radarr](https://wiki.servarr.com/radarr/faq#how-does-radarr-work).
Built-in sites list their latest page (a keywordless search, as Prowlarr does);
Stremio addons and plugin sources are search-only. One page describes up to 100
releases. Head polls default to every 30 minutes. A sync reads from the head
back to the previous head: one page usually, otherwise only the pages the gap
needs (at most `MaxPages` per sync), resuming an unfinished gap every minute
while its allowance has room. A first sync starts at the head; **catch-up**
(optional) also walks recent history. Failed requests do not advance the cursor.

Feeds detect; title searches are events. A movie in the window is searched by id
once when it enters the window, once when its digital or physical date arrives,
and once after its found release proves unplayable (plus finishing a search an
outage or allowance cut short). A search asks one source at a time in priority
order and stops at the first acceptable release. Nothing is searched on a timer.

Matching scans local title identities and resolves episodes locally; it does not
call TMDB for every feed item. Conflicting identifiers and ambiguous title matches
are rejected. A season pack only contributes known, already-aired episodes, and
its health check is targeted to the chosen episode using the existing resolver.

The queue prioritizes popular titles, checks one candidate per title per pass,
and defers extra candidates when the title already has fresh successful evidence.
It never searches the entire catalogue in the background. Playback attempts can
reuse discovered descriptors before normal search, with a five-second shortlist
budget. Viewer/device policy is always reapplied on that path.

Debrid cache lookups are batched and capability-dependent. Background validation
can inspect existing account items and validate a bounded HTTPS range; it never
starts an uncached download or adds torrents. Unsupported cache APIs leave
availability unknown. A debrid account by itself is not a global discovery feed;
recent torrent discovery requires a configured Torznab source. Usenet, debrid,
both, and no playback provider are supported configurations.

Successful evidence is source-, title-, policy- and configuration-specific. It
expires after six hours. Configuration changes invalidate old positives. A source
failure never creates a title-wide negative. Timeout, exhausted allowance and
unsupported checks remain unknown. Normal resolver health-cache hits do not renew
evidence timestamps. Sampled health is useful evidence, not a guarantee of all
bytes or all clients.

Discovery filtering captures one isolated settings value, policy fingerprint,
source list and clock reading per operation. Nested episode and retained-release
reads, pagination exclusions, recommendation priority and newly available shelves
reuse those inputs. The next operation sees settings and source-generation
changes; existing observation invalidation still prevents stale memo writes.
`config.Store.Get` retains its deep-copy isolation. Background admission remains
live so owner pauses and allowance changes take effect during background work.
Resolver candidate checks still apply their existing policy; this snapshot does
not bypass access, identity or quality checks or promise to remove all Home delay.

Library pages and section hubs never judge feed releases inside a request.
They read the last computed hidden set for the evidence window; a set older
than fifteen seconds, or older than a new observation or invalidation, is
served while one background refresh recomputes it. Before the first set
exists every title in the window stays hidden, as it would until evidence
arrives. Each refresh asks the resolver only about (title, release) pairs
without a durable verdict under the current policy key (profile, resolver
settings, catalogue language, rule fields): verdicts live in
`availability_verdicts`, bound to the release descriptor and the title facts
matching reads, pruned with feed items and capped at 60,000 rows. Short-lived
gates (blocklist marks, a brief health-failure skip) are reapplied on every
read; hosted ranking sessions make every verdict short-lived.

Only fresh checked movies move ahead within popular/trending/recommendation
shelves; no items are removed. Explicit chronology and Continue Watching retain
their order. Newly available shelves mean first successfully checked by this
server, not necessarily a film's worldwide premiere.

## Operations

The existing cluster leader runs one scheduler; cursors, observations and atomic
usage counters are durable in the shared database. Hot settings are respected by
admission. Checks pause while local playback is active. Each cycle is bounded,
cancellation propagates, transient outcomes back off, and descriptors expire
after 30 days with a hard retention cap of 30,000 observations. Signed release
locators are sealed using the config store; owner status never exposes them.

Tests use fake feeds/providers and local HTTP fixtures. They cover admission,
account reserve, midnight reset, restart/overlap/gaps, identity conflicts,
source/policy invalidation, stale evidence, disabled-provider behavior, diverse
candidate selection, resolver reuse, and budget propagation across shared workers.
