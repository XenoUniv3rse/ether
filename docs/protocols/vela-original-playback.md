# Vela original playback and secure origin

Vela may send `client.originalPlayback` independently of native media-element
capabilities: `{version:1, range:true, demuxContainers:[], videoDecoders:[],
audioDecoders:[], webgpu:boolean}`. These arrays identify installed decoder
families; clients must still probe exact demuxed decoder configurations and
performance. Missing or unsupported negotiation retains the native decision.
Live originals are not supported.

An admitted original has a ready `direct`/`file` path with `original:true`,
`range:true`, optional `byteLength`, and `sourceId`/`timelineId` naming the pinned
source revision. `originalVideo` contains only observed source codec, picture,
colour and Dolby Vision facts; missing facts stay absent. Audio and subtitle
`streamIndex` values are actual source indexes, not track-list positions.
No width, bitrate or tone-map conversion is applied to original bytes.

The existing broker grant and shared engine serve GET/HEAD, bounded and suffix
ranges, multipart ranges, Content-Length, Accept-Ranges, Content-Range, ETag and
Last-Modified. Invalid original ranges return 416; an If-Range mismatch returns
412 instead of sending the entire file. A changed pinned source is rejected.
Each media request checks the refresh family, active viewer and current media
policy. Refreshing the same family keeps the session, source and media URL;
revocation/expiry denies subsequent reads. Reopen a lost/ended session through
normal playback admission; do not combine its bytes with an earlier sourceId.

Compatible native HLS fallback paths have `readiness:"on-demand"` and no URL.
The canonical `switch` command starts the selected output at `atSeconds` on the
same pinned source, returns its ready URL and advances seekGeneration. It never
resolves a different edition. Other output URLs become on-demand. Native HLS
paths carry `timelineOriginSeconds` at the requested absolute title position;
original paths use zero. Empty boundaries do not claim an independently proven
automatic splice point. Clients must request the server switch.

## Independent audio fallback

`features.audioFallback.available` requires a real local AAC encoder and HLS/
MP4 muxers. With that capability and an installed AAC decoder, a runtime that
supports the original video may admit it even when it cannot decode the source
audio. No encoder runs until requested.

`POST /vela/v0/sessions/{id}/audio-fallback` accepts
`{idempotencyKey,trackId,positionSeconds,sourceGeneration,seekGeneration,
preferredCodec:"aac"}`. It returns `{id,audioGeneration,trackId,url,
mimeType:"application/vnd.apple.mpegurl",codec:"mp4a.40.2",
timelineOriginSeconds:0}`. The URL serves only the selected audio as AAC fMP4
through the existing HLS pipeline, under the session's media grant. Packet PTS
remain absolute title time; playlist numbering starts at zero for that requested
position. Schedule the first playlist entry at positionSeconds. Source and seek
generations stay unchanged. Replacing audio invalidates the old rendition URL
and increments only audioGeneration. `DELETE` on
`/vela/v0/sessions/{id}/audio-fallback/{audioId}`, with `{idempotencyKey}`, cancels
only that rendition and returns `{accepted:true}`. Session stop retires audio
before releasing the original's source pin. Opus/raw output is not advertised.

## HTTPS discovery and handoff

Capabilities, negotiate and bootstrap expose optional `secureOrigins:string[]`
from the same published connection snapshot as Plex resources, preferred LAN or
remote first. Values are HTTPS base origins without paths, query or fragments;
unknown origins are omitted. There are no request-time network probes or
hostnames synthesized from Host/forwarded headers.

The coordinator verified this lab origin and certificate:

`https://192-168-68-65.9cf6a8a376b9403baf1de133251c4c46.plex.direct:32550/vela`

The shared listener serves Vela assets, SPA routes, `/vela/sw.js` (worker allowed
scope `/vela`), API and relative media URLs there. `/vela/` redirects relatively
to the canonical `/vela`. Vela uses bearer/device grants, not origin cookies;
no Vela asset CSP blocks workers, WebAssembly or WebGPU. No extra port or
certificate override is required.

An authenticated `POST /vela/v0/auth/web-code` accepts
`{idempotencyKey,targetOrigin,device}` and returns `{code,targetOrigin,expiresAt}`.
The source device authenticates issuance only. The target must exactly match a
trusted advertised origin. The shared console Watch-button issuer/table stores
a hashed, single-use, 60-second code bound to account, Vela and destination.
Navigate with `#code=<encoded-code>` only, scrub it immediately, create a new
HTTPS-origin device, and use the existing `/auth/web-code/redeem`. Direct TLS and
an exact trusted target Host are required for origin-bound redemption. A wrong
origin/surface does not consume the code. Legacy console handoffs still use the
same existing redeem path. TLS-terminating proxy handoff is not advertised by
this implementation; direct plex.direct TLS is supported.

Real-device release verification must cover HTTP→HTTPS new-device handoff,
service-worker/deep-link loading, MKV/HEVC/DV original seeks and tail reads,
long playback across refresh, revocation, native HLS fallback position and
track preservation, and independent AAC audio switch/cancel/A-V sync. The worker
did not access production or the lab; deployment must pair the runtime client
and server before enabling original admission.

## Offline downloads

An authenticated viewer sees `capabilities.features.downloads` as
`{enabled,maxPerDay?,remainingToday?}`. The optional counters are supplied
for a daily title-count limit. Download permission and usage are shared with
Plex and Jellyfin/Emby, including any configured byte or monthly limits.

`POST /vela/v0/titles/{id}/download` takes `{idempotencyKey,versionId?,client?}`.
Send the current playback `ClientCapabilities` as `client` on every grant
request, including renewals. A valid `client.originalPlayback` takes
precedence over the last negotiated declaration for the authenticated
device and allows a grant immediately after sign-in or a server restart.
Existing clients that omit the declaration can use the device's recent
negotiation while it remains cached.

In the Vela API client, `DownloadRequest.client` uses the same type as
`NegotiateRequest.client`. The download store already obtains this declaration
from `session.declared()` for both start and resume. Pass it through to the
Ether adapter's `resolveDownload` request.

A supplied version uses an ID from the title's version menu. With no
`versionId`, Ether picks an original whose container and default audio and
video streams the device can decode. The file needs no server-side conversion
or audio fallback at playback.

A grant returns
`{titleId,versionId?,url,expiresAt,etag,sourceId,byteLength?,mimeType,filename,container,originalVideo?,audioTracks,subtitleTracks,chapters,markers?}`.
`sourceId` is the original file's opaque ETag, as on a playback path. It
stays the same when a fresh ticket is issued for the same file.
`container`, `audioTracks`, `subtitleTracks`, `chapters` and
`originalVideo` describe the selected original. Tracks, chapters and
video facts use the playback plan shapes. `originalVideo` is present
when probe facts are available. Track and chapter arrays may be empty.
`markers` uses the playback plan's cached intro and credits ranges on
the original file's timeline and is omitted when no ranges are known.

Supported embedded subtitles use `delivery.kind: "embedded"`. Cached
text sidecars associated with the selected release or picked for the
title use `delivery: {kind: "external", format: "webvtt", url}`.
`GET` and `HEAD` on that URL use the same bearer ticket and expiry as
the original. External delivery requires WebVTT bytes in the local
subtitle cache.

Retain the idempotency key with the download. Reposting for the same title
and version issues a fresh bearer URL for the same bytes and leaves the shared
quota unchanged, including across a quota reset. `etag` is a required, quoted
strong validator derived from the release, file identity, and size.
`byteLength` is included when Ether knows the original's size. A range
response provides the total in `Content-Range` when the grant omits it.

`GET /vela/v0/download/{ticket}` serves the original with HTTP range and
conditional requests, including `Range` with `If-Range: <etag>` for resume.
`HEAD` provides headers without a body. The URL grants access to the file
until `expiresAt` without additional authentication headers. A transfer that
started before expiry can finish. Download reads use no playback stream
slot or playback idle timeout and can run alongside a playback session.

Grant refusals include a stable `error.code` for client behavior and a
human-readable `error.message` localized using the request language.

| HTTP status | Code | Meaning |
| --- | --- | --- |
| 400 | `invalid_request` | The grant request is malformed or lacks an idempotency key. |
| 403 | `download_not_allowed` | Offline downloads are disabled for this viewer. |
| 403 | `feature_unavailable` | The server cannot issue a download ticket right now. |
| 403 or 409 | `cannot_play_now` | The viewer's current playback policy blocks the request. |
| 404 | `not_found` | The requested title was not found. |
| 409 | `not_downloadable_kind` | A show, season, or book cannot be downloaded through this endpoint. |
| 409 | `title_unavailable` | The title itself is currently unavailable. |
| 409 | `capabilities_unknown` | Neither the request nor the device's cached negotiation supplies a valid `originalPlayback` declaration. |
| 409 | `version_changed` | The existing receipt is bound to a different version or file. |
| 409 | `version_not_found` | The supplied version ID does not match a current offer for this title. |
| 409 | `version_unavailable` | Ether could not open a compatible original file for the selected version or automatic selection. |
| 429 | `download_limit` | The viewer has reached a download quota. |
| 503 | `resource_capacity` | All download ticket slots are occupied. |

Quota and ticket capacity refusals include `Retry-After` in seconds. For
quota this is the time until the current UTC quota period resets. An
expired ticket responds with 404. As with other rangefile responses, an
`If-Range` mismatch returns the full representation. Check the returned
status and validator before appending bytes to a partial file.

## Offline watch progress

Send offline progress to the normal authenticated
`POST /vela/v0/titles/{id}/watch` endpoint with
`{idempotencyKey,offline:true,positionSeconds,observedAt}`. `observedAt` is
the time the device observed the position, in RFC 3339. A future value is
clamped to the time Ether received the request. Ether applies only
observations newer than the stored `lastViewedAt`. It uses the normal
completion threshold and preserves titles already marked watched. A client
with only a completion observation can send `played:true` without a
position. Stale reports are acknowledged so clients can drop them from the
offline queue.

Accepted progress uses the usual watch sync with the observation time.
Plex clients can report the same observation with `POST /:/timeline`,
`offline=1`, and `updated` as Unix epoch seconds, alongside the usual
millisecond position and duration. The offline report does not modify an
active Plex playback session.
