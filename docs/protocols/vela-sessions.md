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

Like `prepare-fallback`, this selects the highest offered, currently
allowed rung whose peak bitrate is at or below the cap. If none fits,
Ether returns `422 quality_unavailable`. The successful command returns
the new plan with the selected path and advanced source and seek
generations. `atSeconds` is the absolute title position. If omitted in the
bitrate form, Ether uses the session's current position. A previously
prepared rung can retain its timeline origin.
