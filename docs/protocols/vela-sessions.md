# Vela v0 playback sessions

## Observations and events

Send an increasing `sequence`, the current `sourceGeneration` and
`seekGeneration`, the selected path, state, and absolute title position to
`POST /vela/v0/sessions/{id}/observations`. Duplicate or older sequences
are acknowledged without applying them again. A stale generation returns
`409 stale_generation`.

An accepted observation advances the plan revision and updates the snapshot's
position, state, and `lastObservationSequence` even when the server suppresses
the SSE plan event. A position update or a transition between the player's
`playing` and `recovering` echoes can produce a plan keep-alive at most once
per 10 seconds, while observations continue. Derived live lag follows the
same position rule. Substantive changes such as pause, track selection, path
changes, and updated delivery facts publish immediately. The SSE comment
heartbeat is independent.

`GET /vela/v0/sessions/{id}/events` uses the event cursor for replay.
A cursor advances only when an event is appended. Plan revisions order
plans and snapshots and may skip between events. A snapshot may have the
same cursor as an earlier event with a newer revision. Match observations
and commands by source and seek generation.

Playing and buffering observations may include `throughputBps`, the client's
measured active download rate for playback media in bits per second. Omit
the field until there is a fresh measurement. A supplied value must be a
finite number greater than zero and at most 10,000,000,000. Use bytes read
while downloading to calculate the rate, excluding idle player time.

Ether keeps at most five recent samples per viewer, device, and client
address on the serving node. It takes at most one sample every two seconds
and requires three usable samples within 90 seconds. The lower median with
25% headroom is an advisory bitrate budget for Auto source ranking and
existing down-failover. Client throughput alone does not trigger a source
swap. An explicit source selection or owner pin still takes precedence.
The estimate expires in memory as samples age out.

The title versions response and an Auto playback plan may contain
`autoSelectionReason: {code, message}`. The versions response describes
the offered Auto pick. An active playback plan describes the admitted pick.
`connection_estimate` indicates recent client download measurements
favored a lower bitrate. `source_delivery` indicates recent media-source
delivery favored a lower bitrate. `resume_source` indicates that Auto reused
a source saved from earlier playback. The message is localized. The reason is
separate from `rememberedChoice`.

## Switching by bitrate

Send a session command with the current source and seek generations and
either a `pathId` or a positive integer `maxBitrate` in bits per second.
For example:

```json
{
  "kind": "switch",
  "sourceGeneration": 1,
  "seekGeneration": 1,
  "maxBitrate": 8000000,
  "atSeconds": 120
}
```

For a client that opts into adaptive HLS, the bitrate form opens a master
whose variants all fit the requested peak cap and the viewer and upload
ceilings. The highest fitting rendition is listed first. The player can
switch up and down inside the capped set. `prepare-fallback` offers a
capped master while leaving the selected path in place. A speed-driven
source down-failover can also tighten the active master's `maxBitrate`.
Reload the master when an updated plan lowers that value.

For other clients, the bitrate form selects the highest allowed fixed
rendition within the cap. If no rendition fits, the command fails. A
successful switch advances the source and seek generations. `atSeconds`
is the absolute title position. If omitted, Ether uses the session's
current position for either form. Prepared playback can retain its
timeline origin.

## Adaptive HLS

A Vela client can set `client.features.adaptiveHLS: true` when starting a
session. Check `features.adaptiveHLS.available` in the server capabilities
to discover support. When the selected HLS path has `adaptive: true`, give
its `url` directly to Media3's HLS media source or to AVPlayer. Leave native
adaptive track selection enabled and do not pin an initial rendition or send
a `switch` command in response to bandwidth changes. Explicit path switches
remain available for manual quality choices. Eligible original files keep
their direct-play primary. For an opted-in client, Ether also offers an
`adaptive-hls` path with its own master URL. After sustained buffering
on direct play, Ether can select that path automatically and publish a
plan event with new source and seek generations and a master capped to
measured delivery. The handoff uses the current title position. Ether
prefers fresh client throughput and can also measure bytes delivered by
the file GETs. It waits for 15 seconds of buffering or a second buffering
episode within 30 seconds and requires delivery below 90% of the source
bitrate. Server measurements must span at least 15 seconds. Changing
source or seek generation resets the evidence.

The master uses one muxed audio track across its variants. Every variant has
its own media playlist and, for fMP4, its own initialization segment. Ether
uses one segment timeline and forces transcode cuts at that timeline's
boundaries. A remux can share the master when Ether can establish its
keyframe grid and the opening GOPs have no leading pictures. Ether checks
this before publishing the timeline using the available index and bounded
packet probes. If copy alignment cannot be established, Ether builds an
encoded timeline with the largest compatible picture its bitrate and
transcode budgets allow. Each rendition keeps its CODECS declaration for
the lifetime of the stream. The automatic variants use the same video
codec family, dynamic range, frame rate, and audio selection. If fewer
than two variants qualify, the path has no `adaptive` flag. An opted-in HLS path without adaptive quality carries
`adaptiveUnavailable: {reason, message}`. The reason is a stable code and
the message is localized. The viewer and upload ceilings apply when the
stream is opened. Variant playlists can be read without starting an encoder. The
first initialization or media GET selects a rung. An idle encoder releases
its slot and already published segments remain available.

Jellyfin and Emby clients can request this master with
`EnableAdaptiveBitrateStreaming=true` on PlaybackInfo or their session's
`master.m3u8` request.
