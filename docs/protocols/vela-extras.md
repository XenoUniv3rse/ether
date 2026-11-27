# Vela v0 title extras and YouTube trailers

`GET /vela/v0/titles/{id}` returns extras in `extras`.
`GET /vela/v0/titles/{id}/extras` returns the same entries in `items`.

Each entry includes `id`, `parentId`, `kind`, `name`, `durationSeconds`, and
`availability`. `availability` continues to describe server playback and can
be used to decide whether to try that path. Teasers are represented as
`kind: "trailer"`.

A YouTube entry includes `source: "youtube"` in either playback mode. Entries
from other sources omit `source`. `clientPlayback` is present only when the
owner allows direct playback and the entry is a YouTube trailer or teaser
whose source ID matches `^[A-Za-z0-9_-]{11}$`.

```json
{
  "id": "0000000000000001001c6bf5263401c8",
  "parentId": "000000000000000100038d7ea4c6807b",
  "kind": "trailer",
  "name": "Official trailer",
  "durationSeconds": 120,
  "availability": "unavailable",
  "source": "youtube",
  "clientPlayback": {
    "provider": "youtube",
    "videoId": "xI4vbUvezLU",
    "embedUrl": "https://www.youtube-nocookie.com/embed/xI4vbUvezLU"
  }
}
```

The capability is advertised under `features.trailerEmbed` in the capabilities
document.

```json
{
  "features": {
    "trailerEmbed": { "available": true, "mode": "clientWhenPossible" }
  }
}
```

The default is `{"available": false, "mode": "server"}`. The shared owner
setting is `frontends.vela.settings.trailerPlayback`, editable under Apps ›
Vela › Trailers. In `clientWhenPossible` mode, an app that uses the embed URL
connects to YouTube from the viewer's device.

With extras enabled, `clientWhenPossible` includes TMDB YouTube trailers
and teasers even when Ether has no resolver command. These client-only entries
have `availability: "unavailable"` and carry `clientPlayback`. Plex Discover
clips and TMDB videos can appear together in the same best-first list, with
repeated video IDs deduplicated. When a resolver is present, server playback
can still fail even though the trailer is listed.
