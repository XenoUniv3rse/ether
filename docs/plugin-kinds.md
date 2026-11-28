# Plugin content kinds

A content plugin adds a whole **library** to Ether — audiobooks,
comics, music, books, photos, field recordings, anything flat or
hierarchical — with no core changes. The owner meets each kind you
declare as a library beside Movies and TV Shows, in the console and
in every client app you publish it to.

This page is the contract. The worked example is
[`sdk/examples/kind-library`](../sdk/examples/kind-library); the
Studio "A new library" template generates the same skeleton from a
description.

## Declare a kind

In the manifest's `ether` block: `kind: "content"`, at least one of
the `catalog_provider` / `kind_resolver` capabilities (plus optional
`matcher` and `frontend_adapter`), and one entry per kind in
`contributes.mediaKinds`:

```jsonc
{
  "key": "recordings",              // ^[a-z][a-z0-9-]{0,31}$, unique in the plugin
  "label": "Field recordings",
  "media": "music",                 // optional: video|music|audiobook|book|podcast|photo
  "library": {
    "name": "Field recordings",     // default name; the owner can rename
    "card": "landscape",            // optional: poster (default) | landscape | square
    "dateDisplay": "date",          // optional: year (default) | date (full YYYY-MM-DD)
    "experience": "listen",         // optional: watch | listen | read | gallery | live, or another id (default by media class)
    "tabs": [                       // optional (<= 50); defaults to Recommended + Library
      { "key": "recommended", "label": "Recommended", "kind": "recommended" },
      { "key": "library", "label": "Library", "kind": "library" },
      { "key": "albums", "label": "Albums", "kind": "library", "level": "album" }
    ]
  },
  "levels": [                       // 1–5 levels, first is the root
    { "key": "collection", "label": "Collection", "role": "artist" },
    { "key": "album", "label": "Album", "role": "album", "container": true },
    { "key": "track", "label": "Track", "role": "track", "playable": true }
  ],
  "fields": [                       // validated per item; indexed ones can facet
    { "key": "genre", "type": "string", "label": "Genre", "indexed": true },
    { "key": "durationSec", "type": "duration", "label": "Duration", "indexed": true }
  ],
  "present": { /* see "Publishing" */ },
  "browse": {
    "sorts": ["title", "index"],
    "facets": ["genre", "durationSec"],
    "search": true,
    "liveSearch": true,
    "expand": { "facets": ["genre"], "levels": ["artist", "album"], "ttlHours": 168 } // optional on-demand slice & container expansion
  },
  "sync": { "mode": "pull", "refresh": "1h" },
  "cache": { "maxBytes": 2147483648 },        // optional; per-library blob cache
  "match": { "thresholdDefault": 0.8 }        // optional
}
```

Rules that will bite if ignored: at least one level must be
`playable`; facet fields must be `indexed` (except `people`, which is
always indexed); `enum` fields declare a closed `enum` list, while
`strings` fields may declare an optional `values` vocabulary shown in the
rules editor even before any item carries them; field keys may not reuse
envelope names (`id`, `title`, …); declarations are **additive across
versions** — removing or retyping a field/level is refused at upgrade
unless the owner agrees to replace the library's contents.

### Library presentation, sub-tabs, sorts and facets in Vela

`library` and `browse` shape how Vela renders the library without per-plugin client logic:

- `media` and `levels[].role`: optional semantic hints (`media`: `"video"`, `"music"`, `"audiobook"`, `"book"`, `"podcast"`, or `"photo"`; `role`: `"item"`, `"artist"`, `"album"`, `"track"`, `"series"`, `"book"`, `"audiobook"`, `"part"`, `"show"`, `"episode"`, or `"collection"`). A level may also declare `parent` (to branch from an earlier level rather than the previous one) and its own `media` class (for example a `video` concert branch under a `music` artist). For containers with stored children, `media.childCounts` carries direct-child counts keyed by child role (or level key when role-less) from the indexed per-level tally on both list and detail responses, and `media.childCount` is the direct-child total (the sum over `media.childCounts`, not the recursive leaf count). On a branching container's detail response (`GET /vela/v0/titles/{id}`), `seasons` lists only the primary child level (the first child level matching the container's media class, else the first declared child level); other child branches are listed via `GET /vela/v0/titles/{id}/children?role=<role>`. Music kinds (`media: "music"`) and `"track"`-role items are excluded from Continue Watching and skip saving mid-track resume offsets while still recording playback timestamps and completions so history shelves work. Other media classes automatically localize the Continue shelf title per request locale (`Continue Watching` for video/photo, `Continue Reading` for book, `Continue Listening` for audiobook/podcast).
- `library.icon`: optional package-relative SVG or PNG. Ether fetches it alongside the manifest on URL install and serves it at `/ether/api/plugins/<id>/icon?kind=<key>`. The console uses it for the catalogue and library, with `ether.icon` and then the Stremio `logo` as fallbacks.
- `library.card`: `"poster"` (2:3, default), `"landscape"` (16:9; Ether serves the item's `thumb`, else `backdrop`, as `artwork[]` kind `"thumb"`, falling back to `poster`), or `"square"` (1:1).
- `library.dateDisplay`: `"year"` (default) or `"date"`. When set to `"date"`, Vela receives `"dateDisplay": "date"` on the library (and on projected items alongside canonical `"airDate": "YYYY-MM-DD"`), and both Vela and the owner console display full `YYYY-MM-DD` dates from `item.released` instead of year-only labels. Useful for dated broadcasts, tournament VODs, or daily recordings.
- `library.experience`: how apps present the whole library, as an experience id. Ether's ids are `"watch"` (films and TV: the app's ordinary library pages and tabs), `"listen"` (a listening app — in Vela a library sidebar, a persistent now-playing bar, a queue and full-screen Now Playing, in place of the tab strip), `"read"` (books and comics), `"gallery"` (photos and home videos) and `"live"` (a guide, multiview, sports). The set is open: any id matching `^[a-z][a-z0-9-]{0,31}$` is accepted, and an app that does not know an id presents the library as `watch`. It defaults by the kind's media class: `listen` for `music`, `audiobook` and `podcast`; `read` for `book`; `gallery` for `photo`; `watch` for everything else — so a music kind opts out with `"watch"` and a sports kind opts into `"live"`. Vela receives it as `experience` at the top level of every `/vela/v0/libraries` entry (always sent), and `features.libraryExperiences.experiences` lists Ether's ids. It is library-level only and unrelated to a tab's `layout` (`grid`/`list`/`timeline`). In the listen experience Vela builds its pages from the same data as any library: the library's rows (a `continue` row reads "Continue listening"), item roles, `media.credits` (author, narrator, host), episode dates and durations, played state and resume, and chapters.
- `library.tabs`: up to 50 entries of `{key, label, kind, level?, facet?, order?, card?, layout?, search?, searchPlaceholder?, sort?, direction?, filters?}` where `kind` is `"recommended"`, `"library"`, `"collections"`, `"people"`, `"view"`, or `"live"`. Keys match `^[a-z][a-z0-9-]{0,31}$` and are unique within the library; labels support `$t:` keys and follow copy rules (≤40 characters). Omitting `tabs` defaults to `Recommended` (`recommended`) and `Library` (`library`). `library`, `view`, `collections`, and `people` tabs may set optional `level` (naming a declared `levels[].key`) to scope browsing, facets, facet collections, and people lists to a non-root hierarchy level (for example an `Albums` or `Tracks` tab on an `artist -> album -> track` kind). Vela exposes `level` on each tab and accepts `?level=<key>` on `/items` and `/facets/{field}` (or inherits the tab's `level` from `?tab=<key>`); Plex `?type=8/9/10` and Jellyfin `IncludeItemTypes=MusicArtist/MusicAlbum/Audio` resolve to matching declared levels on the indexed SQL path.
  - **`"collections"` tabs**: may set `facet` (one of `browse.facets`), `order` (`"count"` default, or `"name"` for case-insensitive alphabetical ordering), `search` (`true` by default; set `false` to hide in-tab search), `searchPlaceholder` (optional localizable `$t:` or literal copy, ≤60 characters, when `search` is enabled), `sort` (one of `browse.sorts` or `"lastViewed"`), and `direction` (`"asc"` or `"desc"`, defaulting to `released` `desc` when `released` is declared). `GET /vela/v0/libraries/{id}/collections?tab=<tabKey>&query=<text>` pages those facet buckets (optionally filtered by case- and diacritic-insensitive word prefix on `query`, 1–100 characters after trimming) with the newest item's artwork, and opening one navigates to `/vela/v0/libraries/{id}/items?facet.<field>=<valueId>&sort=&direction=` (or `?collectionId=facet:<field>:<valueId>`).
  - **`"people"` tabs**: require `facet` referencing a declared `browse.facets` entry whose field has `type: "people"`, and accept `order` (`"name"` case-insensitive alphabetical by default, or `"count"`), `search` (`true` by default), and `searchPlaceholder` (optional localizable `$t:` or literal copy, ≤60 characters, when `search` is enabled). `sort` and `direction` are not used on `"people"` tabs. `GET /vela/v0/libraries/{id}/collections?tab=<tabKey>&query=<text>` pages the distinct root-level people in that field (with the same `query` word-prefix filter when `search` is enabled) as `{"id":"person:<personId>","kind":"person","person":{"id":"<personId>","name":"…","photo":…},"itemCount":N,"receiptToken":""}`, with portraits resolved in one batch per page; `<personId>` opens `GET /vela/v0/people/{id}` and `/vela/v0/people/{id}/titles`.
  - **`"view"` tabs (Smart Views)**: define filtered, sorted sub-views inside the library — including per-viewer History, Watchlist, Favorites, Unwatched, or field-filtered tabs. Accepts optional `layout` (`"grid"` default, `"list"`, or `"timeline"`), `sort` (one of `browse.sorts` or `"lastViewed"`), `direction` (`"asc"` or `"desc"`, defaulting to `"desc"` for `"lastViewed"`), `search` (`true` by default), `searchPlaceholder` (≤60 characters when `search` is enabled), and `filters` (`{match?: "all"|"any", rules: [...], exclude?: [...]}`). Filter rules may target declared indexed fields (`"browse:<field>"`, `"year"`, `"rating"`, or mapped indexed facts) and per-viewer state (`{"field":"viewer","op":"is","value":{...}}` supporting `history`, `watched`, `unwatched`, `inProgress`, `favorite`, `watchlist`, `lastViewedAfter`, and `lastViewedBefore`, evaluated strictly for the requesting viewer). Vela reads items via `GET /vela/v0/libraries/{id}/items?tab=<tabKey>` and receives `title.watch.lastViewedAt` (RFC3339 UTC) on viewed items.
  - **`"live"` tabs**: surface ephemeral currently-live streams inside the library without registering Live TV channels or requiring Live TV setup. Accepts optional `card` (`"landscape"` default, `"poster"`, or `"square"`), `search` (`true` by default), and `searchPlaceholder` (≤60 characters when `search` is enabled); `facet`, `order`, `sort`, and `direction` are not used. Vela lists items via `GET /vela/v0/libraries/{id}/live?tab=&query=&filter=&cursor=&limit=` and starts playback via `POST /vela/v0/libraries/{id}/live/play` (returning a standard Vela `PlaybackPlan` and session). Thumbnails are proxied and signed by Ether — upstream URLs never reach clients.
  - For example, a documentary & broadcasts library can combine dated items (`dateDisplay: "date"`), a searchable `"live"` tab, tournament collections, and a per-viewer `"view"` History tab:
    ```json
    "library": {
      "name": "Documentaries",
      "card": "landscape",
      "dateDisplay": "date",
      "tabs": [
        { "key": "live", "label": "Live", "kind": "live", "card": "landscape", "searchPlaceholder": "Search live streams" },
        { "key": "recommended", "label": "Recommended", "kind": "recommended" },
        { "key": "library", "label": "Library", "kind": "library" },
        { "key": "topics", "label": "Topics", "kind": "collections", "facet": "topic", "order": "name", "searchPlaceholder": "Search topics" },
        {
          "key": "history",
          "label": "History",
          "kind": "view",
          "layout": "timeline",
          "sort": "lastViewed",
          "direction": "desc",
          "searchPlaceholder": "Search watch history",
          "filters": {
            "match": "all",
            "rules": [
              { "field": "viewer", "op": "is", "value": { "history": true } }
            ]
          }
        }
      ]
    }
    ```
- `browse.sorts` and `browse.facets`: surfaced directly on `GET /vela/v0/libraries` under `browse.sorts` and `browse.filters.facets` (with `"lastViewed"` always available for recently watched ordering). Facet values are paged at `GET /vela/v0/libraries/{id}/facets/{field}` (`int`, `date`, and `duration` fields are served as discrete buckets — years for `date`, duration ranges for `duration`, and distinct integers up to 50 or ranges above 50) and filtered with repeatable `facet.<field>=<bucketId>` on `GET /vela/v0/libraries/{id}/items` alongside in-library `query=`.
- Recommended rows (`GET /vela/v0/libraries/{id}/rows`):
  - Accepts optional `?tab=<tabKey>` to query a specific `"recommended"` tab's shelves. An unknown `tab` id answers `404` (`not_found`), and a tab whose `kind` is not `"recommended"` answers `400` (`unsupported_library_query`).
  - When `library.shelves` is omitted, Ether builds auto-generated shelves (up to 8 by default, 20 items each) from your stored items — Continue watching, Newest, Recently added, Top rated (when `rating` is a declared sort), and daily-rotating values from your declared facets.
  - To define curated default Recommended shelves directly in the manifest, declare `library.shelves` (up to 40 entries). When `library.shelves` is non-empty, Ether prepends Continue watching whenever the viewer has in-progress items, then evaluates each declared shelf in order (up to 20 items each, skipping empty shelves) without clamping to the auto-generated 8-row default. Each shelf receives the stable row ID `plugin:<viewID>:shelf:<key>`, so the library's Recommended row editor can reorder, hide, pin, or set `MaxRows` (owner overrides always win). `GET /vela/v0/libraries/{id}/rows` supports cursor pagination (`limit` and `cursor` query parameters, returning `items`, `continuation`: `"more" | "exhausted"`, and `nextCursor`) so clients can page through 30+ shelves:
    ```json
    "shelves": [
      { "key": "jump-back-in", "label": "Jump back in", "level": "album", "sort": "lastViewed", "direction": "desc", "purpose": "recentContexts", "filters": [{ "field": "viewer", "value": { "history": true } }] },
      { "key": "recently-played", "label": "Recently played", "level": "track", "sort": "lastViewed", "layout": "tracks" },
      { "key": "latest-episodes", "label": "Latest episodes", "sort": "released", "direction": "desc" },
      { "key": "science-nature", "label": "Science and nature", "facet": "topic", "values": ["Science", "Nature"] },
      { "key": "longform-history", "label": "$t:shelf_longform_history", "sort": "rating", "direction": "desc", "filters": { "topic": ["History"], "format": ["Feature", "Miniseries"] } }
    ]
    ```
    Each shelf accepts:
    - `key` (required): unique in the library, matching `^[a-z][a-z0-9-]{0,39}$`.
    - `label` (required): literal or `$t:<key>`, at most 60 characters.
    - `level` (optional): declared `levels[].key` to query instead of the root level.
    - `sort` (optional, default `"released"`): one of `title`, `added`, `released`, `year`, `rating`, `index`, `lastViewed`.
    - `direction` (optional, default `"desc"`): `"desc"` or `"asc"`.
    - Optional filtering: either `facet` plus `value` or `values` (OR), or `filters` as either a legacy facet map (`{ "<facet>": ["..."] }`, AND across facets, OR within each facet), a `TabFilter` object (`{ match?, rules, exclude? }`), a single rule object, or a list of rules (`[{ "field": "viewer", "value": { "history": true } }]`; omitted `"op"` on `"viewer"` rules defaults to `"is"`). Every referenced field must be a declared indexed field (or `people` field, or envelope facet `year`/`released`/`rating`, or `"viewer"`). Viewer history filters and `lastViewed` ordering roll descendant playback up to ancestor containers (so playing a track surfaces its album on an `album`-level `lastViewed` shelf without ever leaking unviewed items).
    - `purpose` (optional): what the shelf is for, so apps can draw it their own way and keep its titles out of other rows: `recentContexts` (a "Jump back in" of albums, playlists and artists the viewer played) or `liked` (the viewer's liked items, such as Liked songs with `filters: [{ "field": "viewer", "value": { "favorite": true } }]`). Omitted, the shelf is a plain recommendation.
    - `layout` (optional): the shelf's default look, `standard`, `hero`, `wide`, `tiles` or `tracks` (a track list, for track-level shelves in listening libraries). An owner's look for the row on a Recommendations page wins.

## The item envelope

Your provider returns `KindItem`s: your own `id` (letters, digits,
`. _ ~ -`, ≤128 chars — Ether namespaces it, you never see the stored
identity), `level`, `parentId`, `index` within the parent, `title`,
optional `summary/year/released/rating`, `playable`, counts, `art`
(`poster/backdrop/thumb/logo` plus declared roles), `fields` (checked
against your declaration; unknown keys are dropped, wrong types
reject the item), `hints` (an opaque JSON object ≤16 KiB, stored and
handed back to your resolver verbatim — put ids and locators there,
never secrets), an optional `recording` (≤200 characters: the work the item
is a copy of, identical on every release that carries it, e.g.
`"isrc:GBDUW0000053"` for a song on its album, single and compilations;
apps use it to group duplicates and Vela receives it as
`media.recordingId`), and an `etag` for cheap re-syncs.

Caps: title 500 characters, summary 20k, each field 4k, 16 art roles.

## Routes Ether calls

Kind and setup routes are `GET`/`POST` with JSON in and out, through the
normal plugin transport. Frontend-adapter traffic keeps whatever method and
query the client sent:

| Route | When | Notes |
|---|---|---|
| `/ether/v1/kinds/{kind}/browse` | Pull sync walks your catalogue | One level per call (`parentId`, `level`, `cursor`, `limit`). Return `nextCursor` or the full set up to `limit`; never more than `limit` items. |
| `/ether/v1/kinds/{kind}/changes` | Pull sync refresh | Optional delta feed `{upserts, deletes, nextCursor, complete, checkpoint?}`. A feed of replayable upsert-only pages may set `checkpoint: true` on each intermediate page. Ether commits its upserts before saving the next cursor, so an interrupted sync can resume at that cursor. Feeds with deletions keep whole-pass deletion checks. Answer 404/501 and Ether re-walks and diffs by `etag` instead. |
| `/ether/v1/kinds/{kind}/expand` | Viewer opens a declared facet bucket, plugin person, or container at a declared `browse.expand.levels` level | For facets/people: background only when `browse.expand.facets` lists that facet, receiving `{facet, value, valueId?, cursor?}`. For declared container levels (`browse.expand.levels`, each a `container: true` level): triggered when a viewer opens the container (`Item`) or its children (`Children`, including `?role=` / `?level=`) across every frontend whenever no fresh expansion exists within `ttlHours` or the container has 0 stored children; receives `{parentId, level, cursor?}` where `level` is the container's own level key. Single-flight per `(plugin, kind, itemRef)`, bounded first-page wait (~1.8s), background continuation via the expand job, additive upserts (direct children at any child level, optional grandchildren, optional self-refresh of the container), never triggered on Search, lists, Home, or in the background. Returns `{items, cursor?}` (or `nextCursor`) or `{job: "/ether/job/..."}`. |
| `/ether/v1/kinds/{kind}/items/{id}` | Conformance and detail refresh | One full item. |
| `/ether/v1/kinds/{kind}/search` | Client searches, only when `browse.liveSearch` is on | Same page shape as browse with `query` set. Short budget; see below. |
| `/ether/v1/kinds/{kind}/browse` with `level` = a list level | Pull sync, after the main tree | A **list level** (`levels[].members: true`, e.g. role `playlist`) is a root of its own: no parent, never playable, and no level names it as parent. Its items carry `members`: up to 500 external ids of other items of the kind, in order, which become its children. A page of a list level may also return those members and their parents (an album and artist a track needs), parents first, so everything is stored together; members not stored are skipped when the list is read. |
| `/ether/v1/kinds/{kind}/import` | A viewer imports a playlist link from one of the sites in `import.links` (for example `deezer`) | `{url}` → `{name, description?, tracks: [{title, artist, album?, isrc?, durationSec?}], total?, truncated?}`; answer 404 for a link you do not read. Set `truncated` when the source shared fewer tracks than the playlist has (with `total` when it says how many), so apps can tell the viewer instead of silently importing part of it. At most 500 tracks are used. 10 s deadline. |
| `/ether/v1/kinds/{kind}/identify` | Ether matches an imported playlist's tracks (from a link or an M3U/CSV file) to your items, in batches, only when `import.identify` is true | `{tracks}` → `{items, matches: [{index, id?, by?, candidates?}]}`. `items` are stored like live-search results (parents first). One match per track you answered: `id` and `by` (`isrc` or `title`), or `candidates` (external ids) when several releases fit equally, or neither when nothing matched. Leave out tracks you ran out of time for; Ether asks for them again (up to three times). Viewers see matched and unmatched tracks honestly and pick a release for ambiguous ones. 10 s deadline. |
| `/ether/v1/kinds/{kind}/related` | A viewer opens an item (purpose `similar`), or a queue of the kind's items nears its end (purpose `continuation`), only for purposes declared in `related: {similar, continuation}` | `{item, purpose, limit}` → `{label?, items, order?}`. `items` are stored like live-search results (send any parents a playable item needs); `order` names the external ids to show, in order (default: items at the same level for `similar`, playable items for `continuation`). One call per item and purpose at a time; answers are remembered for six hours (empty ones for one). `similar` becomes the item's detail row (Vela `GET /rows?placement=detail`); `continuation` is Vela `GET /titles/{id}/continuation` (autoplay). |
| `/ether/v1/kinds/{kind}/lyrics` | A viewer opens a playable item's lyrics, only when the kind declares `lyrics: true` | `{item}` (the stored item, with `hints` and `fields`) → `{syncedLyrics?, plainLyrics?, instrumental?, source?, lines?}`; `syncedLyrics` is LRC (`[mm:ss.xx] words`; enhanced `<mm:ss.xx>word` stamps give word timing). `lines` (optional, used instead of `syncedLyrics`) are structured `{startMs, endMs?, text, words?: [{startMs, endMs?, text}], translation?, romanization?}` for providers with per-word timing, translations or romanization; Vela receives them on `GET /titles/{id}/lyrics` (`features.lyrics.words`/`romanization`). Answer 404 when there are none. Never called ahead of use; Ether remembers answers for a day and misses for six hours, and serves them to every client (Vela `GET /titles/{id}/lyrics`). |
| `/ether/v1/kinds/{kind}/live` | Viewer or owner opens a `kind: "live"` tab | `{tab?, q?, filter?, cursor?, limit?}` → `{items:[{id, title, subtitle?, badge?, viewers?, tags?, thumb?, live?}], filters?:[{key, label}], nextCursor?}`. Cached briefly (~45s); 10s interactive budget. |
| `/ether/v1/kinds/{kind}/live/resolve` | Viewer or owner plays a live item | `{id}` → `{url, headers?, mime?, title?}` (or 404 when offline). Never cached; 5s interactive budget. Ether tunes HLS passthrough/relay directly and reuses the sniffed master manifest across relay fallback (safe for single-use stream tokens). |
| `/ether/v1/kinds/{kind}/resolve` | A viewer presses play | `{libraryKey, item, partIndex?, source?, client?}` → ordered `sources`. Never called during browse. |
| `/ether/v1/kinds/{kind}/match` | Resolve-time matching, console re-match | `{item, candidates}` → `{matches:[{candidateRef, score 0..1, reasons}]}` best first. |
| `/ether/v1/setup/{step}/test` | The setup wizard | `{step, fields}` → `{ok, detail, fields?}`. Secrets arrive via `SecretGet`, never in this body. |
| `/ether/v1/frontend/{key}/…` | Your frontend adapter's traffic | Any method, and the original query string (repeated keys flatten to the first value for remote plugins; hosted plugins receive every value of a repeated key, such as Subsonic's `id=1&id=2`, as an array). Ether authenticated the viewer first; you get `X-Ether-Viewer` headers, `X-Ether-Origin` (`scheme://host[:port][/prefix]` derived from the request using Ether's trusted proxy policy, with any client-supplied `X-Ether-Origin` stripped), and a request-scoped `X-Ether-Viewer-Token` for `kinds.visible`, `kinds.browse`, and remote `frontend.streamURL` callbacks — never a credential. Outside declared upload routes, bodies are capped at 1 MiB (larger gets 413). The exact request bytes always arrive base64-encoded in `bodyBase64` with the client's `contentType`; `body` additionally holds the parsed JSON value when the bytes are valid JSON, and is absent for non-JSON bodies (multipart, form-encoded, binary). The TypeScript SDK's `frontendBodyBytes(req)` returns the exact bytes. |

Clients are **always served from Ether's store** — browse, search
and detail never call you live, so nothing scales with your catalogue
size. You are called at sync time and at play time.

Sync modes: **pull** (Ether walks `browse`, then polls `changes` on
your `refresh`, minimum 15m) or **push** (you call the `KindUpsert`
host service; refused for pull kinds — one writer per kind). A pull
walk that would delete more than half the library aborts and raises
owner attention instead of wiping it.

Push writes are journalled for undo, which keeps up to 30 days, 10,000
entries, or 256 MiB of inverse data per plugin. By default (`sync.undo:
"snapshots"`), unchanged items with the same content hash are skipped
and only modified items are snapshotted. Large push catalogues can
declare `sync.undo: "ids"` to record only created and touched IDs
without reading snapshots; undo then removes only items the batch
created.

### Live search

With `browse.liveSearch: true`, a client search on your library also
calls your `/search` route with a short budget (about two seconds).
Results are validated, **upserted into the store like synced items**
(stubs are fine — playable leaves resolve at play time), and merged
with local results. On timeout or error the viewer gets local
results in time and your late answer still lands in the store for
the next search. Declare it only if your source can answer a search
quickly; the Studio template has it on with the explanation beside
the toggle. Pair it with `browse.search: true` so earlier live
results stay findable locally.

### On-demand facet, person, and container expansion (`browse.expand`)

When a plugin seeds a starter catalogue (for example top games across studios,
or artists with 1–4 featured albums out of 80+) and wants to fetch the full
back-catalogue of a developer, person, or container only when someone actually
opens it, declare `browse.expand`:

```json
"browse": {
  "sorts": ["title", "released", "rating"],
  "facets": ["developers", "voiceCast"],
  "search": true,
  "expand": {
    "facets": ["developers", "voiceCast"],
    "levels": ["artist", "album"],
    "ttlHours": 168
  }
}
```

- `browse.expand` requires `catalog_provider` and must list at least one entry
  in `facets` or `levels`. Every entry in `facets` must appear in
  `browse.facets`, and every entry in `levels` must name a declared level with
  `container: true`. `ttlHours` defaults to `168` (7 days) when omitted or `0`
  and applies to both facets and container levels.
- **Facet and person slices (`browse.expand.facets`)**: when a viewer opens a
  bucket's items (`GET /vela/v0/libraries/{id}/items?facet.developers=Nintendo`
  or `?collectionId=facet:developers:Nintendo`, or the Plex/Jellyfin
  equivalents) or a plugin person's titles (`GET /vela/v0/people/{id}/titles`),
  Ether checks freshness within `ttlHours` and launches one background
  expansion job per `(plugin, kind, facet, value)` in the two-slot background
  job pool at lowered OS priority (waiting up to ~1.8 s on page 1). Listing
  tabs or collections never triggers expansion. Ether calls
  `POST /ether/v1/kinds/{kind}/expand` with `{facet, value, valueId?, cursor?}`
  (`value` is the bucket label such as `"Nintendo"` or person name; `valueId` is
  the synthetic plugin person ID when expanding a `people` facet) under
  owner-granted background limits.
- **Container expansion on open (`browse.expand.levels`)**: when a viewer opens
  a container at a declared level — either its title detail (`Item`) or its
  children list (`Children`, including `?role=` / `?level=`), on every frontend
  (Vela, Jellyfin/Emby, Plex, Silo, Stremio, Xtream) — Ether checks whether a
  fresh expansion exists for `(plugin, kind, item)` within `ttlHours` **and**
  the container has at least one stored child. If no fresh expansion exists or
  the container has 0 stored children, Ether calls
  `POST /ether/v1/kinds/{kind}/expand` with `{parentId, level, cursor?}` where
  `parentId` is the container's external ID and `level` is the **container's**
  level key (`facet`, `value`, and `valueId` are omitted).
  - `items` holds the container's direct children at any declared child level
    (each with `parentId` set to the container and its own `level`), may
    include grandchildren, and may include the container itself (same
    external ID and level) to refresh its metadata in place while preserving
    its stored `parentId` (if the self-refresh names a different `parentId`,
    such as a collaboration album naming its primary artist, Ether keeps the
    stored parent rather than reparenting or failing the expansion).
  - Concurrent opens of the same container (such as detail + children) share
    one single-flight call per `(plugin, kind, itemRef)`. Page 1 waits at most
    ~1.8 s, upserts additively (never deleting omitted children; `parentId` and
    `level` are validated against the declared level tree), then answers with
    accurate tallied `childCount` / `childCounts`. Further pages continue in
    the background through the expand job.
  - Failed expansions record `state = "error"` with their completion time in
    `plugin_facet_expansions` and enter an error cooldown (default 15 minutes,
    clamped by `ttlHours`) so repeated opens do not retry in a storm even when
    the container still has 0 stored children.
  - Child counts (`childCount`, `childCounts`, `leafCount`) for tallied parent
    levels come strictly from stored parent tallies — plugin-supplied
    `childCount` values on container items are ignored for tallied levels.
  - Never triggered in the background, Search, lists, Home, or artwork loads.
- Return `{items: [...], cursor?: "next"}` (`nextCursor` is also accepted; Ether
  pages up to 20 pages or 2,000 items per expansion and upserts them through the
  standard kind item pipeline so policy, FTS, `v_fold`, tallies, and art apply)
  or `{job: "/ether/job/..."}` to finish via a continuation job that writes items
  with `KindUpsert`. Answering `404` or `501` is treated as a clean no-op.
- While a facet/person expansion for that slice is running, Vela adds
  `"expanding": true` to `GET /vela/v0/libraries/{id}/items` and
  `GET /vela/v0/people/{id}/titles`, and bumps the response `ETag` and kind
  sync generation when it finishes.

## Resolving bytes

`resolve` returns ordered `PlayableSource`s. Ether opens them with the
same ranked sliding window it uses for movies and TV: two opens stay in
flight in rank order, failing opens advance immediately, a ready
equivalent backup may overtake a stalled lead after the 2 s grace
period, two confirmed permanent failures widen the window to four for
equivalent candidates, and sources that spend an indexer grab (`nzbRef`,
or `torrentRef` without an `infoHash`/`magnet`) join a single healthy
in-flight open only as a hedge after the learned health-hedge delay
(default 1.5 s) so a healthy open normally costs one grab. Losing opens
are cancelled and their readers closed before playback starts.

When a resolver returns multiple interchangeable releases of the same
quality (for example a Usenet release and a cached debrid release of the
same 1080p encode or lossless album), tag them with the same optional
integer `tier` (`*int` in Go). Within each contiguous tier, Ether
reorders candidates using its source router — honoring the owner's
routing setting (`auto`, `usenet`, `debrid`), measured protocol startup
times (fed by both core and plugin release opens), known-cached debrid
copies, Usenet backoff, and stall-backed delivery demotion — and lets
candidates in the same tier overtake each other after the grace period
or take widened slots. Sources in different tiers always keep the
plugin's tier order (Ether never swaps a lower tier ahead of a higher
tier), and omitting `tier` keeps the plugin's exact order.

Patterns:

- **file** — a hosted resolver path inside an owner-picked folder; requires
  `folders: read` and live containment checks when resolving and opening.
- **range** — a URL Ether reads with HTTP Range (demoted to download
  when the host cannot range and the size fits the cache). Set `connections`
  to 2–6 to split large read-ahead windows into concurrent requests. This
  shares the per-origin limit with cached downloads and uses no disk.
  The default is one. Parallel reads require ordinary HTTP byte ranges and
  a strong ETag or usable Last-Modified date. Other sources read sequentially.
- **download** — fetched whole into the per-library blob cache
  (digests checked when you provide them), then served locally.
- **direct** — the client surface is handed the URL (only where the
  surface supports it).
- **container** — one member of a zip/RAR/7z, possibly multi-volume:
  `{volumes:[{url,size}], member}` or `memberIndex`. Passwords come
  from a named settings secret (`passwordSecret`), never from your
  response. Store-mode members stream by byte range; compressed ZIP
  (deflate), non-solid 7z (LZMA/LZMA2) and RAR4/RAR5 members decode
  from ranged reads into the library cache; solid 7z caches the volume
  first under the request context and library cache quota. Encrypted
  non-stored members and unsupported archive formats or compression
  methods fail with `archive_unstreamable`.
- **release** — hand Ether a magnet/infohash/NZB and its existing
  debrid/Usenet/torrent pipeline opens it. For torrents, set
  `release.fileIndex` (zero-based index in the provider's complete file list),
  `release.candidate.fileIndex`, or `release.file` (exact path or unique
  basename) to open audio, books, comics, ROMs or other non-video files.
  A top-level `fileIndex` overrides the candidate's index. If both index and
  `file` are supplied they must identify the same file. Empty files,
  invalid indices and missing or ambiguous names fail without fallback.
  Without either selector the existing video picker is used. In Go, indices
  are `*int`: a pointer to zero selects the first file; nil leaves selection
  automatic. Indices refer to the provider's ordering, not its file IDs;
  prefer a path when the provider's ordering is unknown. On an NZB release
  the same selectors narrow the NZB to that file (and its archive/PAR2 set):
  `fileIndex` counts NZB document order and `file` matches a full name or
  unique basename. `fileHint` retains its exact archive member path or
  unique basename matching semantics unchanged. When inner filenames inside
  a multi-track audio release or scene RAR cannot be known before opening,
  set `release.track` (`{disc?, number?, title?}`) instead: Ether matches
  candidates by disc and track number first (using `title` to break ties),
  then by normalised title tokens when no number matches, failing on
  ambiguity. Torrent releases follow the owner's debrid setting: Ether tries
  every candidate cached-first, and when the owner has turned "Cached
  torrents only" off and every candidate is an uncached public torrent (or
  permanent non-torrent failure), Ether sends the best eligible release to
  the first debrid service to download and answers `preparing` (`503` with
  `Retry-After`) until ready, following the queued download on retries and
  across items that share the same torrent. Set `release.cachedOnly: true`
  to opt a release out of uncached downloads explicitly. Private torrents
  and background/warm-up calls are always cached-only. When a torrent has
  100 or more files (known from parsed `.torrent` / indexer metadata, a
  prior `release_files` call, or an optional `release.fileCount` hint on a
  magnet whose file list Ether has not seen yet), debrid providers that
  keep large torrents cached only as a single server-side ZIP (such as
  TorBox) ignore a `checkcached` miss, probe with a cached-only add, and
  read the named file directly from inside the server-side ZIP over HTTP
  range requests (stored or deflate members). If that member is itself an
  archive (for example a ROM `.zip`) and the source also sets `container`,
  Ether unwraps the inner member transparently and checks declared digests
  against the final bytes served.

Any source may declare `sha256`, `sha1` or `crc32` (hex; CRC32 is the IEEE
polynomial as 8 hex digits) and `size` (plus optional `sizeApproximate: true`
when `size` is a decoded estimate rather than an exact byte length). Ether
checks them against the bytes actually served — for a release, the selected
file; for a container, the member (whose size the archive directory reveals
before any data is read). Exact sizes (`sizeApproximate` false or omitted)
require exact byte equality; approximate sizes (`sizeApproximate: true`) are
treated as a hint with a tolerance band. Bytes that fit `maxBytes` are read
into the library cache and verified
before serving; larger sources are hashed as they stream and the read
fails at the end on a mismatch. A mismatch is an ordinary open failure:
the next candidate source is tried. When several sources are returned and
an earlier one fails, Ether logs the failure (plugin, library, item,
source id, pattern, error) even though a later source plays; if the
failure contradicts your answer itself (a missing archive member, an
out-of-range index, a digest, size, or duration mismatch), the fallback
that played is not saved as the item's resolution, so the next play
resolves again.

To verify a file's running time on open (for example when picking one
file out of a multi-file pack by name where a trailer or bonus clip
could share tokens with the episode), set both `durationSec` and
`durationToleranceSec` (a positive number of seconds) on the
`PlayableSource`. Ether consults the durable `release_probes` cache
first (scoped per selected file/member so entries in a pack never
collide), otherwise reads the container headers within a 10 s bound
using its native prober (and ffprobe fallback when configured), and
refuses the source as wrong content (`core.ErrWrongContent` /
`ErrPluginAnswer`) when `|observedDurationSec - durationSec| >
durationToleranceSec`, advancing to the next candidate. Leaving
`durationToleranceSec` omitted or zero keeps `durationSec` as estimated
metadata only.

Background jobs, actions, and sync routes can also inspect container
metadata ahead of time via `ether.media.probe(source)` (hosted) or
`POST /ether/plugin-host/media/probe` (remote). It accepts the same
`PlayableSource` shape `/resolve` returns, returns
`{durationSec, container, width, height, sizeBytes}`, reuses
`release_probes`, is hard-bounded at 10 s per probe and rate-limited per
plugin (60/min), requires `permissions.media: "probe"`, and is refused
on every viewer/client request path (including `/resolve` and search).
Release probes run strictly cached-only without queuing uncached debrid
downloads; pattern-specific network, folder, secret, and blob grants
still apply.

`ResolveSources({kind: "indexer_search", query?, category?, profile?, stream?, firstResults?, settleMs?, minResults?, skipIndexers?, onlyIndexers?, timeoutMs?})` accepts a
comma-separated list of Newznab category IDs (`"3000"`, `"7030"`) or standard
names: `console`, `movies`/`movie`, `audio`/`music`, `pc`, `tv`, `adult`,
`books`, `comics`, `other`. It sends a generic search with those categories
and the query unchanged, rather than assuming a movie or TV request.
When `query` is omitted or empty and `category` is set, Ether issues a
recent-releases / RSS request (`t=search&cat=…` with no `q`) and returns up to
the newest 100 candidates; an empty `query` with no `category` is refused.
On a deadline-bounded request (any viewer or interactive route such as `/resolve`
or `/match`), `indexer_search` is deadline-aware by default: even when no fan-out
options are passed, Ether caps the wait to the remaining route deadline minus
`max(1.5 s, 35% of remaining time)` (at least 25% of remaining time when under
2 s left — for example 7.8 s on a fresh 12 s `/resolve`, leaving 4.2 s for the
plugin to rank, check cache, and reply). Candidates that have already arrived are
returned immediately, while slower indexers are reported as `"pending"` (and
listed in top-level `pending`) and continue finishing into Ether's search cache
in the background up to the 30 s indexer ceiling. On unbounded or background-job
calls (`/ether/job/*`, kind sync, Live TV sync) whose remaining deadline exceeds
the 30 s ceiling, `indexer_search` still waits for every enabled indexer up to
the 30 s ceiling unchanged.

Indexers that time out or fail repeatedly (3 consecutive transient failures at
Ether's search ceiling, sharing the same per-indexer circuit breaker as Ether's
built-in resolver) enter exponential backoff (2 min up to 30 min, with a single
half-open recovery probe once backoff expires). While paused in backoff, cache
misses skip the stalled indexer immediately with `status: "skipped"` and
`reason: "paused_backoff"` (`ms: 0`) so healthy indexers answer without waiting
out the cut-off window.

Optional fan-out controls let a resolver tune latency further:

- `stream: true`: starts the fan-out (reusing Ether's `SearchWith` arrival
  callback and `WithIndexerSkip` filters) and answers immediately with
  `{candidates: [], searchId, pending, done, indexers}`. Poll incremental
  batches with `sources.resolve({kind: "indexer_search_results", searchId, waitMs?})`,
  which waits up to `waitMs` (capped at `10000` ms; `0` returns immediately) for
  at least one new arrival or completion and returns only candidates that
  arrived since the previous poll alongside `pending: [{indexerId, indexer}]`,
  `done` (`true` once every indexer has answered, failed, timed out, or been
  skipped), and per-indexer `indexers` statuses. Stop unfinished indexers at any
  time with `sources.resolve({kind: "indexer_search_cancel", searchId})`.
  Streaming searches are scoped strictly to the calling plugin (another plugin's
  `searchId` returns `"not_found"`), capped per plugin (`4` live searches),
  bounded by the 30 s indexer ceiling (or a shorter `timeoutMs`), dropped if
  never polled (`15 s`), and expired after idle polls (`3 min` after the last
  poll) or cleaned up once `done: true` is consumed or cancelled.
- `firstResults: true`: return as soon as the fastest indexers produce results
  plus `settleMs` (default `250` ms when omitted; `0` returns immediately;
  capped at `30000` ms). Slow indexers still in flight continue in the
  background (bounded by the 30 s ceiling and coalesced with matching follow-up
  searches) so their results populate Ether's search cache for subsequent calls.
- `minResults`: minimum deduplicated candidate count required before returning
  early. Combined with `firstResults: true`, returns as soon as either
  `minResults` is reached or the `settleMs` window expires. Without
  `firstResults`, waits until `minResults` is reached (plus `settleMs` if
  explicitly provided) or all indexers finish.
- `skipIndexers` / `onlyIndexers`: case-insensitive lists (up to 256 entries
  each) of indexer IDs (`indexerId`) or display names (`indexer`) to exclude or
  include for this search. `skipIndexers` takes precedence over `onlyIndexers`.
- `timeoutMs`: per-call wait budget in milliseconds, capped by Ether's 30 s
  ceiling and any tighter deadline-aware reserve on viewer/interactive calls.
  When the effective wait budget is shorter than the 30 s ceiling and expires
  while slow indexers keep filling the search cache in the background, those
  indexers are reported as `"pending"` (and listed in top-level `pending`).
  Indexers that hit the 30 s ceiling (or are cancelled because the detached
  background cap is full) are reported as `"timed_out"`.

For `indexer_search`, `indexer_search_results`, and `indexer_search_cancel`,
the `sources.resolve` response envelope includes
`indexers: [{indexerId, indexer, status, results?, ms, reason?, error?}]`
alongside `candidates`, where `status` is `"answered"`, `"pending"`,
`"timed_out"`, `"skipped"` (carrying `reason: "paused_backoff"` when skipped by
Ether's stalled-indexer circuit breaker), or `"error"` (with a sanitized short
`error` code on failure), plus top-level `pending: [{indexerId, indexer}]`
whenever indexers remain running in the background (and `searchId`/`done` for
streaming calls).

`ResolveSources({kind: "indexers"})` (gated by the same `permissions.sources: "search"`
permission) lists the owner's currently enabled indexers in priority order under
`indexers: [{indexerId, indexer, kind, medianMs?, lastMs?, lastError?}]`, where
`kind` is `"newznab"`, `"torznab"`, `"builtin"`, `"stremio"`, `"zilean"`, or
`"addon"`, `medianMs` and `lastMs` summarize recent search timings (omitted
until the indexer has been searched), and `lastError` carries a sanitized error
code when the indexer's most recent search failed. The listing never includes
API keys, URLs with secrets, or cookies.

Non-indexer candidate queries (`debrid_check`, `debrid_acquire`, `release_files`,
`usenet_check`) omit `indexers`, `pending`, `searchId`, and `done`. In Go,
TypeScript, and Python SDKs, `ResolveSources` / `kinds.resolveSources` /
`resolve_sources` continue returning candidates only, while
`ResolveSourcesWithResult` / `kinds.resolveSourcesWithResult` /
`resolve_sources_with_result` return the full
`{candidates, indexers?, pending?, searchId?, done?}` envelope (raw hosted
`ether.sources.resolve` returns that envelope directly).

### Request an NZB

Call `ether.sources.resolve({kind: "nzb_request", nzbRef})` with a reference
returned by indexer search. A native release can also be requested with
`nzbIdentity: {indexerId, guid}`. The single candidate in the response contains
`files` (up to 2,000 descriptor entries, with estimated sizes), `fileCount`,
and an `nzbRef` for the stored NZB. Save that reference for future playback
using `{pattern: "release", release: {nzbRef}}`. Pass the original candidate
to `release_files` when you need Ether to inspect files inside archives, or to
`usenet_check` to sample article availability.

Ether checks the NZB vault first. A miss uses the originating indexer's normal
grab limits and stores the NZB in the vault. A temporary grab refusal leaves
other candidates available. Installed plugins with `kind_resolver` and
`permissions.sources` can request NZBs. Background requests also need the
`+check` grant and use the background allowance. Ether refuses plugin HTTP
calls to configured indexer hosts and keeps indexer URLs and API keys in
server configuration. Sidecars use this same sources API. Their own HTTP
clients run under OS or container network policy, so operators can restrict
direct access there. Existing `nzb:<download link>` sources for configured
indexer endpoints still use the shared grab path.

Use `Context.RequestNZB` in Go, `requestNzb(ctx, ref)` in TypeScript, or
`client.kinds.request_nzb(ref)` in Python. The SDKs also expose the
`nzb_request` query through their source resolver methods.

Each `indexer_search` candidate carries `indexer` and `indexerId`, and video
release names also receive `score`, `resolution`, `source` and `codec` scored
against `profile` (or the owner's default quality profile when `profile` is
empty); non-video candidates leave those four fields empty, and unknown profile
names are rejected. Usenet candidates carry an opaque `nzbRef`, and torrent
candidates that need a descriptor download carry a `torrentRef`. The deprecated
`nzbUrl` and `torrentUrl` fields mirror those stable references as
`ether-nzb:<nzbRef>` and `ether-torrent:<torrentRef>`. Existing plugins can pass
those values back in the deprecated fields. New code should use the reference
fields directly.
Invalid categories are rejected. Stremio add-ons and
source plugins are asked by title id only, so they reject scoped searches,
unless a source plugin declared the category (see "Source plugins that answer
category searches" below). Omit `category` for the default media search.
On a category search, `lookup: {type, fields}` (for example
`{type: "album", fields: {artist: "Air", title: "Moon Safari"}}`) adds a typed
lookup for source plugins that declared that type; every other indexer
searches `query` alone, so keep `query` meaningful. A lookup needs a
`category`, a simple `type` key and at most 8 camelCase fields of up to 256
bytes. Results and acquired files are cached separately by
category and file selection. The wire shapes are documented in
[`plugin-sources.v1.json`](../schemas/plugin-sources.v1.json).

URLs may carry `{itemIndex}`, `{pageNumber}` (= itemIndex + 1),
`{byteStart}`, `{byteEnd}` and `{byteRange}` placeholders; Ether
substitutes integers only. Resolver `headers` may use
`{secret:name}` placeholders, substituted host-side — never put a
raw secret in a response (responses are logged). Redirects on playable
sources (`range`, `download`, `direct`, and `container` volumes) recheck
network permissions on every hop and keep credentials/headers only within
the same origin, strict same-site subdomains (verified via the Public Suffix
List, with no port change or HTTPS→HTTP downgrade), or hosts listed in
optional `keepHeadersOnRedirect` (still within granted network hosts and
never across HTTPS→HTTP downgrades). Returning zero
sources means "not available right now" and is cached briefly.

With `permissions.sources: "search"` (or `"acquire"` to open debrid
content) your resolver can ask Ether to find bytes through the
owner's indexers and debrid via the `ResolveSources` host service,
rank the candidates yourself (or in `/match`), and return a
`release` source.

`sources.resolve({kind: "debrid"})` returns the owner's debrid availability
and cache policy as `{enabled, cachedOnly}`. It requires `permissions.sources`
with search or acquire access. Read these fields through `ResolveSourcesWithResult`
in Go, `ctx.ether.kinds.resolveSourcesWithResult` in TypeScript, or
`ctx.resolve_sources_with_result` in Python.

`ResolveSources({kind: "debrid_check", infoHashes})` (or `infoHash`) checks
which owner-configured debrid services currently hold each torrent and returns
one candidate per hash with `cached` and `cachedBy: [{id, name}]` in the
priority order Ether would acquire from them (using the service's configured
ID and display name, e.g. `"TorBox"` or the owner's account name; omitted when
not cached). The result also includes the owner's `cachedOnly` policy at the
top level. `indexer_search` candidates that Ether already knows are cached
and `debrid_acquire` results carry the same `cachedBy` field (for
`debrid_acquire`, the service that actually served the acquisition).

`ResolveSources({kind: "release_files", infoHash})` (or `magnet`,
`torrentRef`, or `candidate` carrying an `indexer_search` result back) returns
one candidate whose `files` lists what is inside the release:
`[{index, path, size, sizeApproximate?, members?}]` in the order Ether's own
file selection counts (provider file-list order for torrents, NZB document order
for Usenet). Torrent candidates carry their download or details page link in
`torrentRef`: when `infoHash` is absent, Ether resolves
`torrentRef` through the originating indexer first, populates `infoHash` and
`magnet` on the returned candidate, lists files from a debrid provider when
cached, and falls back to `.torrent` metainfo files (with `cached: false`)
when uncached or when no debrid provider is configured. NZBs are fetched
through their originating indexer (the same fetch playback makes, so the NZB
is vaulted for playback), reporting exact decoded sizes from PAR2 or yEnc
`=ybegin size=` headers when readable and falling back to a segment-derived
decoded estimate with `sizeApproximate: true` when only the encoded segment
sum is known. When an NZB contains RAR, 7z, or ZIP archives, Ether also reads
the archive directory (bounded and cached per NZB digest, reusing Ether's
archive readers) and populates `members: [{index, path, size, stored?}]` on the
first volume's entry so a plugin can see inner filenames before opening. Match a
file to your item by name, then return the release with `release.file` or
`release.fileIndex` set (propagating `sizeApproximate` when copying `file.size`
to `source.size`, and setting `release.fileHint` to an exact member path from
`members` or `release.track` when matching by disc/track/title); the choice is
honoured on Usenet as on debrid.

A candidate with `pluginRelease: {plugin, id}` came from a source plugin's
category search (below). It has no `infoHash` or `nzbUrl`: hand the candidate
back to `release_files` (Ether asks the plugin for its files) and return it in
a `release` source with `release.file` or `release.fileIndex`; Ether opens it
through that plugin. It is never cached on debrid, so skip `debrid_check` and
`usenet_check` for it.

### Source plugins that answer category searches

A `stream_source` plugin is asked by title id (`/stream/{type}/{id}.json`). To
also answer the category searches content kinds make (Music searches
`category: "3000"`, Books `"7000"`), it declares them:

```json
"contributes": {
  "source": {
    "class": "indexer",
    "search": { "categories": [3000, 7000], "lookups": ["album"] }
  }
}
```

`categories` lists 1 to 16 Newznab categories; a top category (`3000`) covers
its subcategories (`3010`, `3040`), a subcategory covers only itself.
`lookups` lists up to 8 typed lookups the plugin understands. Ether then calls
the plugin for an `indexer_search` whose category it declared, and only then;
every other add-on keeps refusing scoped searches:

- `GET /ether/search?query=&category=3000[&purpose=][&lookup=album&lookup.artist=&lookup.title=]`
  (10 s route deadline, bounded by the caller's search deadline). `category`
  holds the requested categories the plugin declared, `purpose` why Ether
  searches (`playback`, `detail`, `warmup`, ...), and the lookup appears only
  for a declared type. Answer in the stream route's shape,
  `{streams: [...]}`: a lazy stream (`ether: {lazy: true, id, releaseName,
  size?, languages?}`) becomes a candidate with `pluginRelease`; a torrent
  (`infoHash`) or NZB stream becomes an ordinary torrent or Usenet candidate.
  Plain `url` streams and owner-folder `file` streams are skipped there. The
  answer goes through Ether's search cache, the plugin's search limits and
  the caller's quality profile like any indexer result.
- `GET /ether/files/{id}` answers `release_files` for a lazy release:
  `{files: [{path, size?}]}` in the order `fileIndex` counts (at most 2,000).
- `GET /ether/open/{id}?file=&fileIndex=` opens the chosen file, answering
  `{url, headers?}` (or `{file}` inside an owner-granted folder) as a lazy
  stream does. Ether reads the URL with HTTP Range requests; the URL and every
  redirect must be on the plugin's `permissions.network` hosts (or an
  owner-granted local host), so a plugin cannot point Ether at an arbitrary
  host. Header values may use `{secret:name}` with the plugin's own secrets.

A `pluginRelease` handed back by a caller is honoured only for an enabled
plugin that declares `source.search`.

`ResolveSources({kind: "usenet_check", candidate})` (or `nzbRef`) checks an
NZB's article availability before your resolver offers it, returning a single
candidate with `usenetCheck: {available, checked, missing, cached?,
playbackFailed?, reason?, checkedAt?}`:

- **Bounded & playback-safe:** runs Ether's sampled, bounded STAT/BODY check
  (capped at 32 articles per NZB, regardless of file or segment count) at
  background (`prefetch`) NNTP priority through the owner's shared provider
  connection limits and live-reserve rules, so checks never starve active
  playback.
- **Shared verdict & playback-failure cache:** background checks, on-demand
  checks, Ether's own availability checks, and Play share the same bounded
  per-NZB verdict cache (indexed by keyless NZB URL, raw NZB SHA-256 hash,
  and cross-indexer NZB message-ID hash) for 15 min on available verdicts and
  30 min on missing/failed verdicts. A check made ahead of time is reused at
  Play, and a release Ether's own availability or playback already found bad
  is answered from cache without hitting the indexer or NNTP again. Real
  playback-time article or release failures (`ErrArticleMissing`,
  `ErrArticleCorrupt`, `ErrReleaseRejected`, etc., at open time or mid-stream)
  record `playbackFailed: true` and `available: false` and are never
  overwritten by a sampled check while live. Transient provider or indexer
  errors are not cached as unavailable.
- **On-demand viewer calls (`permissions.sources: "search"` or `"acquire"`):**
  never count against the daily background allowance; bounded only by the
  per-plugin burst limit (`20` checks per minute; excess calls fail with
  `rate_limited` / HTTP 429 carrying `retryAfterSec` / `retryAt` /
  `Retry-After`).
- **Background jobs and schedules (`permissions.sources: "search+check"` or
  `"acquire+check"`):** jobs, scheduled routes, sync, and expand tasks may call
  `usenet_check` when the plugin declares `"search+check"` or `"acquire+check"`
  (independently revocable under the `sources.check` live grant). Each uncached
  background check counts against the plugin's per-day allowance (default `20`
  candidate checks per UTC day per plugin, configurable under
  **Sources › Indexers › Daily budgets**; `0` disables background checks) using
  Ether's `availability` admission machinery and foreground indexer reserve.
  Cached verdict hits do not consume the daily allowance. Once spent, further
  uncached background checks answer `rate_limited` (HTTP 429) with
  `retryAfterSec`, `retryAt` (next UTC midnight), and `Retry-After`.

### Multi-file items (`hints.parts` and resolve-time `parts`)

When one playable item consists of several ordered files (multi-disc sets,
CUE + BIN tracks, multi-part audiobooks, split archives), list its parts on the
item in `hints.parts` (an array of `{name, mime?, size?, durationSec?, fileIndex?}` objects
or filenames; Ether also reads a declared `fields.parts` array when
`hints.parts` is absent), or return `parts` at resolve time on
`KindResolveResponse.parts` (or `PlayableSource.parts`) — useful on both push
and pull kinds when the part list is only learned from the resolved archive or
release. Ether persists ordered parts with stable child `RatingKey`s (`role:
"part"`), exposes them as children of the parent item, and lets per-part plays
target `part.fileIndex` against a cached parent release or resolve with
zero-based `partIndex`:

```json
{
  "id": "ff7",
  "level": "game",
  "title": "Final Fantasy VII",
  "playable": true,
  "hints": {
    "parts": [
      { "name": "Disc 1.chd", "mime": "application/octet-stream", "size": 524288000 },
      { "name": "Disc 2.chd", "mime": "application/octet-stream", "size": 512000000 },
      { "name": "Disc 3.chd", "mime": "application/octet-stream", "size": 498000000 }
    ]
  }
}
```

When `parts` is non-empty, Ether calls `/ether/v1/kinds/{kind}/resolve` per
part with zero-based `partIndex` (`0 .. len(parts)-1`), caches and invalidates
resolved sources per `(item, partIndex)`, and isolates pending prepare jobs per
part:

- **Whole-item opens** (standard protocol playback or `/ether/v1/kind-media/{ticket}`
  without a part selector) resolve `partIndex: 0` for single-part items and
  deliver multi-part items as an ordered, byte-rangeable `Sequence` across all
  parts (`0 .. N-1`).
- **Per-part, per-source, and absolute stream URLs** let frontend adapters and
  multi-file or native clients fetch a specific file, pin a resolver
  `PlayableSource.id`, or embed an absolute URL (`<origin>/ether/v1/kind-media/{ticket}`):
  - Mint a scoped ticket URL with `ether.frontend.streamURL(itemRef, { partIndex?, source?, fallback?, absolute?, wait? }).url`
    (hosted JS; the positional `ether.frontend.streamURL(itemRef, partIndex).url` form still works),
    `streamUrl(ctx, itemRef, { partIndex?, source?, fallback?, absolute?, wait? })` (TypeScript SDK; positional `partIndex` also works),
    `ctx.StreamURLWithOptions(itemRef, StreamURLOptions{PartIndex, Source, Fallback, Absolute, Wait})` /
    `ctx.StreamPartURL(itemRef, partIndex)` (Go SDK), or
    `ctx.stream_url(item_ref, part_index=..., source=..., fallback=True, absolute=...)` (Python SDK) — backed by
    `POST /ether/plugin-host/frontend/stream-url` with `{"viewerId", "itemRef", "partIndex"?, "source"?, "fallback"?, "absolute"?, "wait": {"maxSeconds": 1200}?}`
    (forwarding `X-Ether-Viewer-Token` from remote adapters so Ether can resolve the active request origin and adapter key).
  - Set `wait: {maxSeconds: 1200}` on tickets minted by download routes whose
    clients need one response. Ether accepts positive whole seconds and caps
    the wait at 1800 seconds. It sends no headers while preparation is pending,
    then serves the file (200 or 206), returns a terminal open error, or sends
    503 with `Retry-After` when the wait expires. The server holds at most four
    requests per plugin and 32 total. Requests above either limit use the ordinary
    media open. Omit the option for player routes to keep the 18-second default.
    A disconnect frees the wait slot while the shared preparation job continues.
    Set the wait below the client and reverse proxy response-header timeouts.
    The ticket must be redeemed before it expires. Once redeemed, its expiry
    does not interrupt the wait or the download.
  - By default stream URLs stay root-relative (`/ether/v1/kind-media/{ticket}`).
    When `absolute: true` is set, Ether prepends the request's `X-Ether-Origin`
    (`scheme://host[:port][/prefix]`, honouring trusted proxies only and omitting
    container bridge hosts), falling back to the adapter's first published
    address (`frontends["plugin:<id>:<adapter>"].publishedAddresses`, preserving
    any path prefix) when the request carries none, and finally to the
    root-relative path if neither is available. HTTP redirects (`Location`)
    still require root-relative paths; use `{ absolute: true }` for protocol
    documents handed to native clients (such as Kodi or Subsonic players) that
    open URLs verbatim.
  - For a remembered choice, pass `{ source: rememberedID, fallback: true }`. Ether tries that source first. If it is missing or fails to open, Ether tries the remaining sources for the same item and part in the usual order. A source still preparing follows the normal wait policy.
  - When only `source` is set in the ticket grant, Ether passes `KindResolveRequest.source`
    to `/resolve` and opens **only** the matching source (matching `PlayableSource.id`)
    with no failover to other sources and without clobbering the shared default resolution
    or candidate cache. A plugin that ignores `source` still works: Ether picks the source
    with the matching `id` from its response, or answers `409 no_source` if it is absent.
  - `partIndex` is bounded by the item's declared parts: a playable item that declares no
    parts rejects any `partIndex` with `404 item_not_found`.
  - Unpinned item tickets also accept `?part=N` (`/ether/v1/kind-media/{ticket}?part=0`).
    A malformed or negative `part` answers `400 invalid_part`, an out-of-range
    part (or a part index on a playable item with no declared parts) answers
    `404 item_not_found`, and a `?part=N` that disagrees with a part-scoped
    ticket answers `404 ticket_invalid`. `source` is bound strictly in the
    ticket grant and cannot be supplied or overridden by query parameters.

Watch state and progress are keyed per playable leaf. For paged
media, store progress against the leaf the viewer is on (its index
is the natural unit); containers themselves carry no progress.

## Publishing (`media`, `levels[].role`, and `present`)

A kind's media class and level roles are **facts about the kind**:

- `mediaKinds[].media`: `"video" | "music" | "audiobook" | "book" | "podcast" | "photo"`
- `mediaKinds[].levels[].role`: `"item" | "artist" | "album" | "track" | "series" | "book" | "audiobook" | "part" | "show" | "episode" | "collection"`

Ether derives a fitting presentation for every app from `(media, levels[].role, levels)`
using one shared inference table. When `media` or `role` is omitted, Ether infers them
from declared `present.mediaBrowser` hints (`MusicArtist→artist`, `MusicAlbum→album`,
`Audio→track`, `AudioBook→audiobook`, `Book→book`, `Photo→photo`), then from `present.plex.base`,
then from the level keys and hierarchy depth (one playable level → movies; multi-level → shows;
album/track → music; photo → photos; book/audiobook → books/audiobooks).

An optional `present` block overrides **how** specific apps see the library (types,
base, `itemTypes`) and may suggest `default: false` to start an app switched off. The
manifest never owns **whether** an app publishes a library: owner switches in the
Console's **Show in apps** section are sticky across plugin updates and restarts. When
a plugin update changes a kind's `media`, `levels[].role`, or `present`, Ether updates
each app's presentation shape (such as `itemTypes` or `plex.base`) without turning on a
library the owner switched off or turning off one the owner switched on. Where an app
has no fitting library type (for example books or audiobooks on Plex, or non-video
kinds on Stremio, Xtream, and Silo), that app stays off with a plain-language reason
unless the plugin explicitly declares a custom mapping.

- `vela: {type?: "movies"|"shows", default?: boolean}` — Ether's own app (`shows` needs at
  least two levels). Fits every media class.
- `plex: {base?: "movie"|"show"|"artist"|"photo", default?: boolean}` — root→show/artist,
  middle→season/album, leaf→episode/track. More than three levels
  are flattened with indexes kept (the Studio preview shows this).
  A single-level `artist` kind is a flat album list in a music
  section: its items present as albums, never as artists (name the
  level `artist` or `track` to present as that instead). Fits video,
  music, and photo kinds; book, audiobook, and podcast kinds stay off
  unless explicitly declared.
- `mediaBrowser: {collectionType?, itemTypes?, default?}` — Jellyfin/Emby.
  `collectionType` is one of `movies, tvshows, music, audiobooks,
  books, photos, homevideos`. `itemTypes` maps each level key to
  one of `Movie, Series, Season, Episode, MusicArtist, MusicAlbum,
  Audio, AudioBook, Book, PhotoAlbum, Photo` (anything else is a
  manifest error). Any unmapped level is inferred from `media`,
  `levels[].role`, and hierarchy shape. The resolved type drives the item's `Type`, its
  `MediaType` (`Audio` for `Audio`/`AudioBook`, `Book` for `Book`,
  `Photo` for `Photo`, `Video` for `Movie`/`Episode`), `IsFolder`
  (`MusicArtist`/`MusicAlbum`/`PhotoAlbum`/`Series`/`Season` hold
  children), parent links (a track's `Album`/`AlbumId` name its
  album) and `IncludeItemTypes` filtering. Audio
  items serve through the same media routes; their MediaSources
  carry only the streams the source actually has.
  A single-level kind whose one playable level is typed
  `MusicAlbum` (each album is one playable item, no track items)
  still plays: the album lists itself once as an `Audio` child
  (same item, `Album`/`AlbumId` naming itself) and answers
  `IncludeItemTypes=Audio` library queries with that incarnation,
  which is what Jellyfin's album play button and Songs tab ask
  for. Plex gets the same shape as one synthetic track under the
  album.
- `stremio: {type?, default?}`, `xtream: {category?, default?}`, `silo: {type?, default?}` —
  inferred for video kinds; stay off for music, books, audiobooks, podcasts, and photos
  unless explicitly declared.
- `adapterOnly: true` — no generic mapping fits; ship a
  `frontend_adapter` and serve the protocol yourself. The console hides Tabs,
  card shape, and Releases for adapter-only libraries. The owner can turn off
  “Only in its own app” to publish to the built-in apps. Releases also requires
  a playable video level, which can be a video branch in a non-video kind.
  Embed
  `/ether/v1/kind-media/{itemId}` stream URLs for bytes (the SDKs'
  `StreamURL`/`stream_url`/`streamUrl` helper) instead of proxying
  media through the adapter. A refused stream URL answers
  `{"error": {"code", "message"}}` with the code also in the
  `X-Ether-Media-Error` header, and the host log records the cause
  (`kind media refused`):

  | Status | Code | Meaning |
  |---|---|---|
  | 400 | `invalid_part` | `?part=` is not a non-negative integer |
  | 404 | `ticket_invalid` | Unknown, expired (2 min), out-of-scope, or part-mismatched ticket |
  | 403 | `viewer_invalid` | The ticket's viewer can no longer play |
  | 404 | `item_not_found` | The item (or requested part index) is not in a library this viewer can open |
  | 409 | `no_source` | The item has no playable source (your resolve returned none) |
  | 422 | `archive_unstreamable` | This archive format or compression method cannot be streamed |
  | 503 | `preparing` | Resolve is still `pending` after the 25 s wait budget, or an uncached torrent release was queued on the owner's debrid service (`Retry-After` gives seconds to wait) |
  | 503 | `jobs_busy` | All four playback preparation job slots for the plugin are running (`Retry-After` gives seconds to wait) |
  | 503 | `source_unavailable` | The source is temporarily unavailable (includes `Retry-After` when the plugin instance pool is busy) |
  | 502 | `open_failed` | The source could not be opened |

A library the owner cannot see is invisible to that viewer on every
surface; you never check viewer permissions yourself.

### Frontend adapter icon

`contributes.frontendAdapters[].icon` names the adapter's own mark: a
path to an SVG or PNG inside the package (a PNG at most 512 KB and
1024 px on a side). The Apps page shows it on the adapter's tab and
cards, the way it shows Plex or Jellyfin. Without one, a `subsonic`
adapter shows the OpenSubsonic mark Ether ships; any other adapter
falls back to the plugin's `ether.icon`, and then a generic plug. Ether serves it from
the plugin's icon route (`/ether/api/plugins/{id}/icon?adapter={key}`),
cache-busted per version, and the conformance kit fails a package whose
adapter icon is missing or cannot be rendered.

```json
"frontendAdapters": [
  { "key": "subsonic", "label": "Subsonic", "protocolHint": "subsonic",
    "pathPrefix": "/rest", "auth": "query_credentials",
    "icon": "assets/opensubsonic.svg" }
]
```

Use a protocol's or app's own logo only to identify it, and record its
source and licence in your package's NOTICE.

### Streamed uploads and durable blobs

Declare upload routes in `ether.contributes.frontendAdapters[].uploadPaths`, for example:

```json
"uploadPaths": [{ "path": "/saves/*", "maxBytes": 536870912 }]
```

Paths are relative to the adapter's public prefix. The matcher ignores a trailing
slash and supports Go path globs such as `*` within a segment. The smallest
matching limit applies. Declare at most 16 paths, with 1 byte to 512 MiB per route.
Ether caps each request at 64 MiB by default. The owner can raise that ceiling
up to 512 MiB under the plugin's **Limits**. Ether applies both limits to the
whole HTTP body, including multipart headers and framing. Uploads also share
Ether's staging disk budget.
For `query_credentials` adapters, send credentials in the query on upload routes.

After viewer authentication, Ether stages the body on disk and passes `upload`
metadata in place of inline body bytes. Hosted handlers receive `req.upload`.
The TypeScript adapter receives it as the sixth argument to `handle`, Go uses
`FrontendUploadFromContext(r.Context())`, and Python uses `ctx.upload`. The
metadata contains the original `contentType` and either:

- `blob: {ref, size, contentType, sha256, sha1, md5, crc32}` for a raw body.
- `files: [{ref, size, field, filename, contentType, sha256, sha1, md5, crc32}]` and
  `fields: {fieldName: [value, ...]}` for multipart. Repeated fields and files are
  preserved. Multipart allows 64 parts and 128 KiB of inline text and metadata.

Ether computes the digests while staging each file. They are lowercase
hexadecimal, with eight digits for IEEE CRC-32.

A sidecar implementing its own HTTP or tunnel transport receives the metadata as
a JSON body with `X-Ether-Frontend-Upload: 1`. Uploaded refs work with
`cache.read` during the request and are cleaned up when the response ends.

Declare `ether.permissions.storage: "durable"` to retain data after the request.
The permission drawer shows the plugin's effective quota and lets the owner
revoke access. In the plugin's **Limits**, the owner can change the default
quota of 1 GiB and 10,000 unique blobs. The server-wide plugin limits set the
share of currently free space on the durable store's volume available to cap
raised quotas (25% by default). The byte ceiling uses that share directly and
the file-count ceiling budgets 4 KiB of that share per file. The default limits
remain available when the share is smaller. The staging free-space reserve
still applies. These are ceilings, so setting a quota does not reserve disk
space, and a drop in free space can reduce the effective quota. A lowered
quota preserves existing data, including reads and deletes, and refuses new
unique blobs that would exceed either limit with `quota_exceeded`.
Its blobs survive restarts, cache sweeps, and cache eviction. They are deleted
only by `cache.delete` or plugin uninstall. Keep the returned ref in your own
per-viewer metadata and check the active viewer before accessing that metadata.

```js
// Hosted handler. The SDK cache.put returns the ref directly.
const { blob } = ether.cache.put("", "", null, {
  durable: true,
  fromBlob: req.upload.files[0].ref,
});
```

Promotion links the staged file into permanent storage without copying it. Store
the returned `durable:<sha256>` ref. Identical saved bytes share a ref and quota
charge within the plugin, so delete a ref only after your last use. Call
`ether.cache.usage({durable: true})` on routes that allow cache reads.
With a live durable storage grant it returns `{bytes, files, quotaBytes, quotaFiles}`
for the calling plugin's durable namespace. Staged uploads and evictable blobs
are excluded. TypeScript sidecars use `ctx.ether.cache.usage({durable: true})`,
and Go sidecars use `BlobHost.CacheUsage(ctx, CacheUsageOptions{Durable: true})`.
The wire endpoint is `POST /ether/plugin-host/data/cache.usage` with
`[{"durable":true}]` as the body.

To save small
bytes directly, use `cache.put("", name, data, {durable: true})`. Go sidecars use
`CacheOptionsHost.CachePutWithOptions` and Python sidecars pass the options dict
to `ctx.ether.cache.put`. When the plugin needs the bytes itself, it can read
ranges with `cache.read`.

To serve a saved file from a hosted adapter, authorize the active viewer against
your per-viewer metadata. Return `{status: 200, blob: {ref}}` with the plugin's
own `durable:<sha256>` ref. Omit `libraryKey`, `memberIndex`, and the response body.
Ether rechecks the plugin's durable storage permission and ownership, then
streams the file from disk for GET and supports HEAD and single byte ranges.
An unavailable ref returns 404.

Call `ether.cache.stat(ref)` for `{size, sha256, sha1, md5, crc32}` on an
owned `blob:` or `durable:` ref. TypeScript and Python sidecars use
`ctx.ether.cache.stat(ref)`. Go sidecars use `BlobHost.CacheStat(ctx, ref)`.
Ether checks access even when the digests are cached. Staged or recently
promoted uploads already have digests. Cold reads on viewer paths are limited
to 256 KiB. A sidecar also has this limit while it is serving a viewer request.
Use a job or other background work for larger cold reads (up to 1 GiB).
Exceeding the applicable byte limit returns `quota_exceeded`. Cold hashing has
a ten-second deadline and shares two worker slots across plugins. When both
slots are in use the host returns `unavailable`.
The data directory's `plugin-blob-storage/saved` tree should be included in a
filesystem backup alongside the matching plugin database snapshot.

### Frontend adapter authentication

`contributes.frontendAdapters[].auth` picks how Ether authenticates the
viewer before your adapter sees a request:

| Mode | Client sends | Notes |
|---|---|---|
| `ether_token` (default) | `Authorization: Bearer <token>` (or the token as a Basic password) | Connect apps hands the viewer a token. |
| `basic` | HTTP Basic, username + an Ether-issued app password | |
| `none_lan` | nothing | Trusted LAN only, with an owner-picked viewer (DLNA rule). |
| `query_credentials` | `u` plus `p` (plain or `enc:<hex>`) or `t` + `s` (`t` = md5(password + `s`), lowercase hex) on every request | Subsonic's scheme. Ether issues each viewer dedicated, revocable **app logins** (a generated username and a 15-character app password, like Xtream logins) from Connect apps and the adapter's page under Apps. The household password is never accepted, failed attempts share Ether's per-address sign-in limiter, and `p`, `t` and `s` are removed before the request reaches you (`u` stays). |

In `query_credentials` mode a request without valid credentials is not
answered with a bare 401: Ether relays it to
`/ether/v1/frontend/{key}/.unauthenticated/<original path>` with **no**
viewer headers or token, so you can answer in your protocol's own error
shape (Subsonic error 40) or serve a document the protocol defines as
public (OpenSubsonic's `getOpenSubsonicExtensions`). Anything you do not
handle there (404) falls back to 401.

### Host-served media and artwork

An adapter response may name bytes for Ether to serve at the adapter URL
itself, instead of a body or a redirect:

- `{"media": {"itemRef", "partIndex"?, "source"?}}` — one of your playable
  items, opened for the authenticated viewer through the same path as a
  stream ticket (resolver, Range requests, read-ahead). Every Range request
  re-authenticates with the client's own credentials, so players that seek
  long after a 2-minute ticket would have expired keep working, and clients
  that do not follow redirects are fine.
- `{"art": {"itemRef", "role"?}}` — the stored artwork of one of your items
  (`poster`, `thumb`, `backdrop`, `logo`; empty picks the first). Ether
  fetches and caches the image; the URL comes from the stored item, never
  from your response.

Both are hosted-runtime fields (like `blob`), checked for item ownership
and library visibility before anything is written. A hosted adapter's
string `body` is the document itself (XML, HTML, text) and is written
unquoted; object and array bodies are JSON.

## Matching

Matching is yours; persistence and the console are Ether's. A manual
pin from the item drawer always wins, then your best score at or
above the kind's threshold (manifest default, owner-editable), then
an inline match during resolve. Give plain-language `reasons` — the
owner reads them.

## Host services

Beyond the usual services, kind plugins get (capability-checked on
every call): `KindUpsert` / `KindDelete` (push kinds), `KindItems`
(read your own library back unfiltered), `KindVisible` (`kinds.visible`,
batch-capped at 500 IDs) and `KindBrowse` (`kinds.browse`) for
viewer-authenticated `frontend_adapter` requests to apply the library's
Rules, library grants, and viewer safety policy using the request's
authenticated viewer (never guest-supplied input), `ResolveSources`, and
`CachePut` / `CacheOpen` / `CacheDelete` for the per-library blob cache
(content-addressed `blob:<sha256>` refs; you never name paths).
A `kinds.browse` query filters with facet maps, any-of groups, field
presence tests, and text matching that all combine with AND: `facets`
(any of the listed values per field), `allFacets` (every listed value
per field, for "tagged both X and Y"), `excludeFacets` (none of the
listed values, for "without X"), `anyGroups` (an array of OR-groups,
each either `{field, values}` on one field — for example `(Rock OR Hard
Rock) AND (Live OR Live Recording)` on `genres` — or `{any: [{field,
values}, ...]}` to OR alternatives across fields, for example `label =
"North" OR parentLabel = "North"`), `empty` (a `{field: bool}` map where
`true` keeps items with no value in the field and `false` keeps items
with at least one, covering declared fields and item columns such as
`released`, `year`, `rating`, `summary`, and `recording`; unknown fields
are rejected), `query` (full-text match), and `excludeQuery` (same text
matching as `query`; matching items are dropped). All facet filters
accept the same per-type values as `facets`, including `lo..hi` ranges
for numbers and dates and people names, and are evaluated inside the
browse query so paging and `total` stay correct. A query may carry at
most 32 `anyGroups` and at most 256 total facet values across `facets`,
`allFacets`, `excludeFacets`, and `anyGroups` (counting every
alternative's values in `{any: [...]}`); `empty` counts toward neither
bound. Pass `total` to control whether `KindPage.total` is
computed: `"cheap"` (the default) returns `total` only when it is O(1)
(item or single-facet field tallies, an already-cached exact count, or a
single-page result) and omits `total` otherwise rather than scanning the
library; `"none"` never computes or returns `total`; `"exact"` computes
and caches an exact count for the current library generation.

Frontend adapters also read and write the authenticated viewer's own
state for your items, in the same stores every Ether app uses (likes are
the viewer's favourites, plays and ratings their watch state, playlists
their Ether playlists), so a star in a Subsonic app is a like in Vela.
Hosted JS, all scoped to the adapter viewer and your own items of one kind:

| Call | Returns |
|---|---|
| `ether.kinds.get(kind, ids)` | stored items the viewer may see (≤500 ids), in request order |
| `ether.kinds.state(kind, ids)` | `[{id, playCount, lastPlayedAt?, favorite, rating?}]` (≤500) |
| `ether.kinds.favorite(kind, ids, on)` | likes or unlikes (≤500) |
| `ether.kinds.played(kind, id)` | records a completed play |
| `ether.kinds.rate(kind, id, rating)` | 0–10, `0` clears |
| `ether.kinds.favorites(kind, {level?, limit?})` | `[{id, level, addedAt}]`, newest first (≤1000) |
| `ether.kinds.playlists(kind)` | the viewer's playlists that are empty or hold your items: `[{id, title, summary?, createdAt, updatedAt, total, items, indexes}]` (`items` lists only your items; `indexes` are their positions in the whole playlist) |
| `ether.kinds.playlist(kind, id)` | one playlist |
| `ether.kinds.playlistCreate(kind, title, ids)` | the new playlist |
| `ether.kinds.playlistUpdate(kind, id, {title?, summary?, remove?, add?})` | `remove` takes whole-playlist positions and runs before `add` |
| `ether.kinds.playlistDelete(kind, id)` | |

A pull-synced kind takes no pushes, so an adapter never stores catalogue
items itself. When its viewer opens something Ether has no children for yet,
or searches for something Ether has not seen, ask Ether to do what an Ether
app would, then read the result with `kinds.browse`:

| Call | Returns |
|---|---|
| `ether.kinds.expand(kind, id)` | `{pending}`. Opens the container the way an app's open does: Ether runs your `/expand` (or hydrates the level) on the interactive path, stores the children, and waits as long as an open does. `pending: true` means children are still arriving; browse again shortly. A container the viewer may not see is `not_found`. |
| `ether.kinds.search(kind, query, {limit?})` | `{found}`. Runs the kind's search the way an app's search does, your live `/search` included (bounded), and stores what it finds; read the matches with `kinds.browse({query})`. `limit` defaults to 50, at most 200. |

`cache.delete(blobRef)` removes a blob from the calling plugin's own cache
namespaces and frees its quota immediately (deleting an absent or already
evicted ref succeeds), so large temporary downloads or extracts do not evict
smaller kept files.
The default quota is 2 GiB per library. A kind can declare `cache.maxBytes`
and the owner can adjust the quota in the library settings. Across all
libraries, Ether defaults to the smaller of 10 GiB and 5% of the cache
disk. Owners can change **Shared library cache (GiB)** under **Cache → Libraries**, above the individual library quotas. Clearing the field restores the automatic size. Both modes preserve
the free-space reserve.

For large disc images, allow room for both the downloaded archive and its
extracted member. The runtime limits for **Download (GiB)**, **File inside an
archive (GiB)** and **Archive total (GiB)** also apply. A plugin that declares
a lower download or extraction limit retains that limit.

## Setup

Declare `contributes.setup.steps` (any plugin kind): each step lists
settings fields and an optional test route. The console walks the
steps in a drawer; a content plugin's libraries stay hidden and sync
waits until required steps pass (the owner may skip, and sees a
setup banner instead).

## SDKs

Same contract in all three, pinned by the shared fixtures in
`sdk/conformance/kinds` (each SDK's tests replay them):

- **Go** (`sdk/go`): `Plugin.OnCatalog / OnLive / OnResolve / OnMatch /
  OnFrontend / OnSetupTest`, the `CatalogProvider`, `ExpandProvider`, `KindLiveProvider`, `KindResolver`,
  `KindMatcher`, `FrontendAdapter` interfaces, host kind methods on
  `Host`, and `sdk/go/kit` helpers — `scrape` (polite host-HTTP
  fetching), `archivex` (zip/cbz/epub members), `metaparse` (EPUB
  OPF, ComicInfo, ID3, FLAC), `feedparse` (RSS/Atom/JSON Feed →
  items), `hints` (typed hints/fields).
- **Python** (`sdk/python`): `CatalogProvider` (including optional `expand`), `KindLiveProvider`, `KindResolver`,
  `KindMatcher`, `FrontendAdapter`, `SetupTester` base classes with
  `on_catalog`, `on_live`, etc., `ctx.ether.kinds` / `.cache`, `parse_feed` and
  `archive_list` helpers.
- **TypeScript** (`sdk/typescript`): `onCatalog / onLive / onResolve /
  onMatch / onFrontend / onSetupTest` (`CatalogProvider.expand?`, `KindLiveProvider`), `ctx.ether.kinds` / `.cache`,
  `parseFeed`, `archiveList`, `scrapeText/Links`.

Content plugins are usually best as `remote` (scraping wants a real
process); `hosted` works for pure-API providers — the example and
the Studio template are hosted. In hosted JS, read settings with
`ether.settings.get()` and kind services via `ether.kinds`,
`ether.sources`, `ether.cache`.

## Checking your work

- The conformance kit (`kit.CheckKindProvider`, run by Studio and
  `ether plugin check`) walks your provider offline: pagination must
  terminate, pages must fit the limit, detail must round-trip, a
  declared live search must answer, resolver sources must use known
  patterns and placeholders.
- Studio's kind preview browses your draft, shows the hierarchy and
  the per-protocol projection, and dry-runs the resolver on a real
  leaf (template expansion included) before anything is installed.
- Start from the Studio "A new library" template or copy
  `sdk/examples/kind-library`; its test is the executable version of
  this page.


### Granted local files (hosted runtime)

A resolver can return a source such as
`{"id":"local","pattern":"file","path":"/picked/library/Game.nds","mime":"application/octet-stream"}`.
This requires `folders: read` and an owner-picked, non-revoked folder grant.
Ether uses the same `validateLocalFile` and `openGrantedLocalFile` path as
Stremio `ether.file`: it rejects `..`, resolves symlinks, checks containment at
resolve time and again at open time, and opens through `os.OpenRoot`.
Remote/sidecar resolvers cannot serve host files with this pattern.

### Discovering and listing picked folders

On a hosted job, action, schedule, event or other non-client route, call
`ether.folders.roots()` (Go SDK `FoldersRoots`) to get the absolute folder
paths the owner picked. Use each string directly as the `root` argument for
other folder calls. The return value is `[]` when there are no picks or the
`folders: read` grant is absent or revoked.

A hosted plugin can declare the `events` capability and subscribe to
`plugin.folders.changed` in `ether.contributes.events.subscribe`. When the
owner picks or removes a root or changes the folder grant's revocation state,
Ether queues a private event for the enabled plugin on `POST /ether/event`.
The payload is `{"roots":["/path/to/first","/path/to/second"]}` with the new
list, or `{"roots":[]}` after revocation or removal of all roots. Events use
the normal acknowledgement and retry behavior. Use `folders.roots()` to query
the current grant while handling a queued event.

Hosted plugins with a live `folders: read` grant can call
`ether.folders.list(root, path, {cursor?, limit?})` (Go SDK: `FoldersList`).
Omit the options to read the first page of up to 1,000 entries. The response
is `{entries, nextCursor, truncated}`. Pass `nextCursor` back with the same
root and relative path until it is empty. Names sort in ascending UTF-8 byte
order. Links that leave the picked folder and broken links are skipped before
the page fills. Use a job, action or other non-client route. Each page streams the directory with memory proportional to
the page size. Pages are stable while the directory stays unchanged and do
not represent a snapshot across directory edits.

### File identification and frame extraction

`ether.folders.hash(root, path, {algorithms?})` (Go SDK: `FoldersHash`)
requires a regular file within a currently granted root. It returns
`{size, modTime, crc32?, md5?, sha1?, oshash?, phash?}`, with Unix seconds
for `modTime` and lowercase hexadecimal digests. By default it computes CRC32
(IEEE, 8 digits), MD5 (32 digits), SHA1 (40 digits), and `oshash` for files
of at least 64 KiB. To compute only the OpenSubtitles hash, pass
`{algorithms:["oshash"]}`. That mode works at any file size of at least
64 KiB and reads only the first and last 64 KiB, even if they overlap.
It adds their unsigned little-endian 64-bit words to the file size modulo
2⁶⁴ and returns 16 hex digits.

Whole-file digests share one streaming pass. For files larger than 256 MiB,
call `hashStart(root, path, {algorithms?})`. Poll `hashPoll(id)` for job
state (`running`, `done`, `canceled`, or `error`) and bytes processed for
whole-file reads. A finished job includes `result`; an error includes `error`.
`hashCancel(id)` requests cancellation. Jobs survive the initiating call,
have a 24-hour deadline, and recheck the grant during the read. One job per
plugin and two globally may run at once. A duplicate active start for the
same root, path and algorithms returns the existing job ID.

For video `phash`, use `hashStart(root, path, {algorithms:["phash"]})` from
a job, action or background route permitted for `media.probe`. Viewer
and client routes cannot start it. Ether follows the
[Stash v0.31.1 sampling convention](https://github.com/stashapp/stash/blob/v0.31.1/pkg/hash/videophash/phash.go).
It uses the container duration rounded to hundredths of a second, then
seeks to `0.05 * duration + i * (0.90 * duration / 25)` for each `i = 0…24`.
Each frame is 160 px wide with aspect-preserving even height. The frames
go into a row-major 5×5 montage, hashed with
[goimagehash v1.1.0 PerceptionHash](https://github.com/corona10/goimagehash/tree/v1.1.0)
and returned as 16 hex digits. Extraction uses Ether’s FFmpeg and FFprobe,
with one decoder thread, a minute total for the video hash, and a ten-second
cap per media process. It requires a video stream and a known duration.

`ether.folders.frame(root, path, {atSec, width, libraryKey?})` (Go SDK:
`FoldersFrame`) returns `{ref, libraryKey}` for a PNG in the plugin's owned
media library cache. The library key defaults to the plugin's first declared
media kind. Pass a nonnegative timestamp and a maximum width from 16 to
1,024 pixels. Output preserves the aspect ratio and fits within that width
and 1,024 pixels of height. Use the ref with `cache.read` or an authorized
frontend adapter blob response. The same background-route restriction applies.
A frame call has ten seconds and the plugin can extract 20 uncached frames
per minute, sharing the two global folder hash slots.

Digests and frame references are scoped to the plugin and cached by file device,
inode, size and nanosecond modification time plus operation options. Ether
rechecks grants and file identity, discards results if the file changed, and
regenerates an evicted frame on demand. These folder operations are hosted only.

### Choices on plugin pages

The `ether.settingsForm` widget accepts `select` and `multiSelect` fields
with inline `options` or a runtime `GET /ether/options/{key}` route. Set
`search: true` to send search terms as the route's `q` query parameter.
The form submits the selected value or array of values to its action.
Fields with unsupported kinds are skipped when rendering the form.

### Item availability

An upsert can carry
`"unavailable":{"code":"no_verified_source","detail":"No source matched the DAT record"}`.
The code must be nonempty and at most 128 bytes; detail is optional and at most
2000 bytes. The item remains visible in browse/counts so owners can inspect the
coverage gap. Ether exposes the reason in the item drawer, marks the item
nonplayable, and never calls its resolver or opens a cached resolution while
this declaration is present. The declaration also travels in frontend item
details. The next upsert without `unavailable` clears it, even with the same
etag. This is independent of the level's `playable` capability; clearing the
reason does not grant playback to a nonplayable level.

### Large downloads and archive members (v0.3.12)

Hosted and remote clients can keep large data in the host's existing library
blob cache, reading only the metadata or descriptor bytes the plugin itself must parse.
Big media (video/audio streams, ROMs, disc images, firmware downloads) never goes through
`cache.read`, `archive.open`, or `archive.read` — Ether streams cached blobs and archive
members directly to clients itself (for example via frontend `{ blob: { libraryKey, ref, memberIndex } }`
descriptors in `internal/plugins/frontend_blob.go`, `frontend.streamURL`, and playable container sources):

```ts
const limits = await ctx.ether.runtime!.limits();
const readBudget = limits.readBytes ?? 8 * 1024 * 1024; // clamp(memoryBytes / 4, 8 MiB, 512 MiB)
const cached = await ctx.ether.http.fetchToCache!({
  url: "https://datasets.example/platform.zip",
  libraryKey: "pk-community-example-library-games", // this plugin's own library
  name: "platform.zip",
});
const members = await ctx.ether.archive!.list(cached.blob);

// Batch up to 4096 small/medium members per call within readBytes:
const xmlMembers = members.filter(m => !m.unsafe && m.name.endsWith(".xml") && m.size > 0);
let batch: Array<{ member: number; offset: number; length: number }> = [];
let batchBytes = 0;
for (const m of xmlMembers) {
  const length = Math.min(m.size, readBudget);
  if (batch.length === 4096 || batchBytes + length > readBudget) {
    const results = await ctx.ether.archive!.read(cached.blob, batch);
    // results: [{ index, member, size, body: Uint8Array }] in request order
    batch = [];
    batchBytes = 0;
  }
  batch.push({ member: m.index, offset: 0, length });
  batchBytes += length;
}
if (batch.length > 0) {
  await ctx.ether.archive!.read(cached.blob, batch);
}

const extracted = await ctx.ether.archive!.extract(cached.blob, xmlMembers[0]!.index);
// {ref: "blob:<sha256>", size, sha1, md5, crc32} streamed directly into the library cache
```

`http.fetchToCache({url, headers?, keepHeadersOnRedirect?, libraryKey, name, timeoutMs?, resume?, connections?})` performs a GET, streams
up to the owner download ceiling (8 GiB by default), further bounded by the library cache quota, into the plugin's own library cache, and returns
`{blob, size, crc32, md5, sha1, status}`. SHA-256 is the blob reference.
Digests cover the stored response bytes in one pass. Compare the digest with
trusted catalogue metadata before serving a verified download. Non-2xx responses,
network failures, size overflow and quota failures return errors; they do not
publish a truncated blob. Redirects recheck the live network grant and the
existing SSRF policy, and follow the same redirect credential policy as
`http.fetch` (keep credentials/headers on same origin, strict same-site
subdomains, or explicit `keepHeadersOnRedirect` hosts within granted network
permissions; never across HTTPS→HTTP downgrades). The existing content capability, network host declaration
and library cache quota apply; no new permission is needed. `timeoutMs` defaults
to 10 minutes (also the ceiling on viewer and interactive request paths); in a
job (`/ether/job/*`), schedule, or background context, `timeoutMs` may extend up
to the calling job's remaining deadline. Pass `resume: true` to continue an
incomplete partial blob with `Range: bytes=<cached>-` when the server supports
ranges and strong `ETag` / `Last-Modified` validators match (restarting cleanly
from byte 0 when validators mismatch or the server ignores ranges); partial
blobs obey per-library cache quotas, the shared cap, and the disk free-space
floor, and become readable via `cache.open` / `cache.read` / `archive.*` only
when complete. Jobs, schedules, and background calls can request `connections`
from 1 to 6 (default 1), sharing the host's limit of 6 per origin. When the
server advertises byte ranges, a known length, and a strong validator,
Ether fetches ranges in parallel and hashes the assembled file. Ranges served
from a different URL need the same strong ETag and total length as the probe. With
`resume: true`, retries continue each range's saved progress.
Incompatible range responses fall back to a single download. The operation
runs synchronously within the calling plugin invocation. Cache eviction still applies to returned refs.

The per-call byte ceiling `readBytes` for `cache.read`, `archive.open`, and `archive.read`
scales with the plugin's granted memory: `readBytes = clamp(memoryBytes / 4, 8 MiB, 512 MiB)`
(default 32 MiB plugin memory → 8 MiB per call; declaring `runtime.limits.memoryBytes` of 1 GiB
grants 256 MiB per call). Plugins read their live grant via `runtime.limits`
(`ctx.ether.runtime!.limits()` in TypeScript, `BlobHost.RuntimeLimits(ctx)` in Go,
`host.runtime.limits()` in Python, or `GET /ether/plugin-host/limits`) so batch sizes never rely
on hardcoded guesses.

`cache.read(blob, {offset, length})` reads `1..readBytes` from a cached blob.
`archive.list(blob)` returns `[{index, name, size, unsafe?, unsafeReason?}]`, reading ZIP, 7z or RAR (4.x/5.x) headers
without loading all compressed bytes. `archive.open(blob, memberIndex, {offset, length})`
returns `1..readBytes` from one **uncompressed** member, while `archive.read(blob, reads)`
reads `1..4096` `{member, offset, length}` ranges in one call whose requested `length`s sum
to at most `readBytes` (returning an error rather than truncating when exceeded) and returns
`[{index, member, size, body}]` in request order (`bodyBase64` on raw JSON wire calls).
Use `archive.open` when reading a single member or stepping sequentially through one large
file across range calls. Use `archive.read` when parsing many small or medium members from the
same cached archive (such as thousands of XML, DAT, or CUE descriptors), amortizing cache
ownership checks, directory lookup, and host crossings into one call.
Offsets are nonnegative integers; a read at EOF returns empty bytes and reads crossing EOF
return the available suffix. Every call opens only the calling plugin's cache namespace and
closes every member stream and blob handle before returning.
`archive.extract(blob, memberIndex[, {libraryKey?, name?}])` streams the
uncompressed member directly into the plugin's own library blob cache (defaulting to
the source archive's library namespace) in one pass with no full in-memory buffer,
computing `{ref, size, sha1, md5, crc32}` (`ref` is `"blob:<sha256>"`). It enforces
`archiveMemberBytes` and the target library's cache quota, falls back to
`member-<index>` as the cache entry name when `options.name` is omitted for an unsafe
member name, refuses directories, observes cancellation and
deadlines, and charges governance units per started MiB (`ArchiveExtractUnitsPerMiB`).
The Go `RemoteHostClient` provides `RuntimeLimits`, `FetchToCache`, `CacheRead`, `CacheRasterize`, `ArchiveList`,
`ArchiveOpen`, `ArchiveRead`, and `ArchiveExtract` through `BlobHost`; the Python
`RemoteHostClient` and `MockHostClient` expose `runtime.limits()` and `archive.list`,
`archive.open`, `archive.read`, and `archive.extract`.

`cache.rasterize(libraryKey, name, sourceBlob[, {width?, height?, format?}])` rasterises an
SVG already in the plugin's own library cache into a new cached bitmap blob, for clients
that only draw bitmaps. It returns `{blob, contentType, width, height, size}`; serve the
result as a blob response like any other cached image. A zero `width` or `height` keeps
the SVG aspect ratio and `format` is `"png"` (default) or `"jpeg"`. The host bounds the
source document (512 KiB) and each output dimension (2048), applies the same SVG safety
rules Ether uses for logos (no external resources, scripts, or unsafe constructs), and
returns an error for documents it refuses. Rendered variants are cached by source content
and size, and the stored blob is content-addressed, so repeat calls return the same
reference. The existing content capability and library cache quota apply; no new
permission is needed.

Archive sizes and member counts use owner global/per-plugin ceilings: defaults
are 8 GiB compressed archive/member, 32 GiB total expansion and 65,535 members.
Unsafe member names remain in listings with `unsafe: true` and `unsafeReason`
as informational warnings so callers never use stored names as filesystem paths.
Empty names, names over 1024 bytes, NUL, backslashes, absolute paths (including
drive-rooted paths), and traversal or noncanonical path segments are flagged.
Because `archive.open`, `archive.read`, `archive.extract`, and frontend blob
streaming address members by index and never write stored names to disk, any
non-directory member (including flagged names such as Windows-style backslash
paths) can be read and extracted by index.
These flags are retained in the cached directory. ZIP supports Store/Deflate and ZIP64;
7z supports Copy, LZMA and LZMA2 (including solid blocks); RAR supports stored
and compressed 4.x/5.x members. Other 7z coder chains are refused, including
BCJ filters and PPMd. Split/multi-volume input, encrypted headers or members,
links and other special files are refused. Directories may appear
in listings but cannot be opened. A 1000:1 expansion ceiling applies per ZIP
member and per 7z/RAR compression block. Nothing is extracted to a filesystem.

Metadata and decoder allocations are bounded by `memoryBytes` (32 MiB by
default). ZIP directories also retain the 64 MiB host ceiling. A large dictionary
or directory can be refused even when the member fits the byte grant. 7z uses
at most half the memory grant for its dictionary; RAR reserves one eighth per
decoder buffer, including PPMd and filters. Compressed input bytes read, including
metadata and decoder read-ahead, are cumulatively bounded by `fetchBytes` per
call. Expanded solid blocks and the whole declared expansion are bounded by
`archiveExpandedBytes`. Calls observe the runtime deadline and cancellation.

A range is a bound on **returned bytes**, not decompression work. Deflate reads
start at the member beginning. Solid 7z/RAR ranges decode dependencies from the
block beginning, then discard bytes through the requested offset. Calls are
stateless: sequential calls also restart decoding. RAR can additionally consume
earlier solid blocks in archive order. Prefer large sequential
chunks and avoid repeated random reads; persist verification digests in KV.
Range reads do not independently verify the full member CRC; use trusted digests
when integrity matters. Local owner-granted paths are not archive inputs: first
use a plugin-owned cached blob. Gzip files are unsupported here.
An HTTP `Content-Encoding: gzip` response may be transparently decoded by the
HTTP client; its digests then describe the decoded bytes. A `.gz` file is not
automatically decompressed by this archive service.

Direct `http.fetch` still limits the raw response to 10 MiB, with its JSON result
allowance including encoding expansion. Hosted WASM host-result envelopes also scale
automatically with the plugin's `readBytes` grant (including base64 and JSON framing
overhead, with a 4 MiB baseline floor), while hosted QuickJS binary reads (`cache.read`,
`archive.open`, `archive.read`) transfer decoded bytes directly into guest `ArrayBuffer`s.
Remember that big media never goes through these read calls — Ether streams blobs and
archive members directly to clients itself (`internal/plugins/frontend_blob.go`); reserve
`cache.read`, `archive.open`, and `archive.read` for bytes the plugin must parse.

### Owner-started multi-call work and resource requests

A page action or scheduled call returning `{job: "build-catalogue"}` starts a background job (up to two owner/scheduled jobs per plugin, separate from playback preparation jobs).
The job returns `continueInMs`, an opaque JSON `continuation`, and a `progress`
string to continue; Ether forwards the token in the next POST body. `done: true`
finishes, `failed: "reason"` fails, and an absent `continueInMs` finishes too.
Jobs use the owner's time ceiling (12h by default), a 1,000,000-call sanity bound,
a minimum 100ms pause, OS background CPU/IO priority, and cooperative pauses
between calls and host operations during playback, Live TV or high host load.
They are cancellable with visible progress. Persist checkpoints in KV for
recovery on a later owner action after restart; Ether does not resume eagerly.

`ether.runtime.limits` requests memory, time, compute, instance pool size, download,
archive and catalogue-item resources within owner-controlled global/per-plugin ceilings.
Defaults: 2 GiB memory ceiling (up to 4 GiB; omitted heap request stays 32 MiB),
12h, effectively unlimited background work units, 2 hosted WASM instances (`poolSize`,
requestable up to the owner ceiling, default ceiling 4 and raisable up to 16; with
`poolSize >= 3` at least one instance is reserved exclusively for playback resolves
and background jobs hold at most `poolSize - 2`; with `poolSize == 2` background jobs
and frontend adapters share at most one instance so playback resolves and health
checks keep one free; queued resolves also take priority and start their execution
deadline once an instance is acquired), 8 GiB download/member,
32 GiB total expansion, 65,535 archive members, and 2,000,000 stored catalogue items
(`maxItems`, with no upper cap when raised by the owner). Remote plugins can
also request data and `maxItems` limits. The library cache quota further bounds
downloads. The owner
sees requested versus granted values before install and in the drawer, and can
raise or lower ceilings under Plugins → Settings or for one plugin. Saving
stops affected jobs so the new ceilings apply immediately.

Viewer search and playback calls derive their unit budget directly from each
route's deadline (`10,000 units/s`): `/stream/*` and `/ether/open*` get 3 s
(30,000 units). `/subtitles/*`, `/ether/subtitles/*`, and kind `/live/resolve`
and `/match` get 5 s (50,000 units). Kind `/resolve` gets 12 s (120,000 units).
`/catalog/*`, `/meta/*`, `/ether/metadata/*`, `/ether/markers*`, and kind `/search`,
`/live`, and `/items/*` get 10 s (100,000 units). `ether.runtime.limits()` reports
the effective limits for the current call.
Background kind catalogue sync (`/browse`, `/changes`, and `/expand`), administrative setup,
actions, tools, events and jobs use owner grants. Health/configuration/view/options/test
keep short protocol deadlines. See `sdk/AGENTS.md` and the SDK README for exact
per-route limits and how owners raise ceilings.

### Pending resolve and match replies

A `/resolve` reply may include both `sources` and `pending: true`. Ether tries
the sources first. If every attempted option is an uncached torrent blocked by
the owner's cache policy or the source's `release.cachedOnly` flag, Ether starts the
optional preparation job and returns `preparing` with the supplied retry hint.
If a torrent can be queued, normal debrid queueing applies. The next Play can
pick up the prepared source through `/resolve`.

Both routes can return `{pending:true,retryInMs:1000,job:"/ether/job/prepare"}`.
`job` is optional. Ether clamps retry delays to 250–5000 ms, retries for up to
25 seconds total per Play, and caps each invocation at 12 seconds for `/resolve`
and five seconds for `/match`.
Pending is neutral for health. When the 25-second budget expires while the reply is still `pending`,
Ether returns `503` with `code: "preparing"` and `Retry-After` set from the last
`retryInMs` (defaulting to `5` seconds) across `kind-media`, Vela,
Jellyfin/MediaBrowser, and Plex API routes, and never saves a negative resolution.
Resolver preparation jobs have their own four-slot pool per plugin (separate from
the two-slot owner/scheduled pool); if all four resolver slots are running, Ether
returns `503` with `code: "jobs_busy"`, `Retry-After`, and the refusal message,
and records the refused job on the plugin Jobs view. A real empty `sources: []`
answer stays `409` `no_source`. A job receives the original request body (including `partIndex` when
resolving a multi-part item and `source` when opening a source-scoped stream ticket), uses promoted deadlines and continuation replies,
and is deduplicated per plugin/kind/item (and `partIndex` / `source`). Job completion wakes
the next resolve.

For remote verification call `ether.http.hash({url,algo:"sha256",expectSize,headers,keepHeadersOnRedirect})`.
A miss returns pending while a bounded host worker streams digests without
storing bytes; the next resolve reads the cached result. Optional `headers`
reject hop-by-hop and framing headers, expand `{secret:<name>}` placeholders via
`permissions.secrets` just like playable sources, follow redirects only under
Ether's redirect credential policy (same origin, strict same-site subdomains, or
explicit `keepHeadersOnRedirect` hosts within granted network permissions; never
across HTTPS→HTTP downgrades), and are never logged or persisted in the hash cache.
Plugins that derive temporary session credentials can store them sealed with
`ether.secret.set(name, value, ttlSec?)` (and clear them with `ether.secret.delete(name)`)
under a declared non-owner name in `permissions.secrets`, then reference them as
`{secret:<name>}` in source headers and `http.hash`. See the worked example
and exact timeout, cache, cancellation and byte limits in [the authoring recipe](../sdk/AGENTS.md#slow-resolution-and-remote-verification).


## Sync viewer watch state

Declare `ether.permissions.viewers.write_watch_state: "granted_viewers"` and
have the owner grant the viewers your plugin may update. Send the selected
viewer's ID, the item ID and a watched boolean. The host accepts a playable
item reference owned by your plugin (`pk:<pluginId>:<kind>:<externalId>`) or a
catalogue movie or episode ID returned by the host catalogue. Provider IDs
accepted by `catalog.get` also work when they resolve to a movie or episode.
For shows, send individual episode keys. The host checks viewer access and
writes through Ether's shared watch state. The SDK write takes a boolean.

```js
ether.viewers.setWatchState({ viewerId, titleId: itemRef, watched: true });
```

Subscribe with the `events` capability and `contributes.events.subscribe`.
Declare `viewers.identity: "granted_viewers"` to receive the viewer ID for
granted viewers. The `viewer.watch.scrobbled` subscription covers explicit marks
and playback completion. Alternatively, subscribe to `viewer.watch.watched`
and `viewer.watch.unwatched` for separate triggers, including current-state
updates from imports. `viewer.watch.progress` carries resume checkpoints in
`positionMs`. A plugin's writes are skipped when dispatching back to that plugin.

Events keep `titleKey` and `watched` and add `state` (`watched`, `unwatched`,
`progress`) and `source` (`playback`, `manual`, `plugin`, `import`). The
optional `surface` identifies the app or writing plugin. Use `itemRef` for your
own plugin's items. Catalogue events include available `tmdbId`, `tvdbId` and
`imdbId` fields. For episodes these are the series IDs, paired with `season`
and `episode`. Season zero represents specials.
