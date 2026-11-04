# Transcode chaos (experimental)

Run on demand, outside CI, with ffmpeg and ffprobe that support libx264,
AAC, and the desired hardware encoder:

```sh
go run ./cmd/transcodechaos -ffmpeg /path/to/ffmpeg -ffprobe /path/to/ffprobe -report chaos.json
```

The driver generates a 12-second TS clip, uses a loopback-only range source,
and calls the real transcode manager directly. It probes and decodes each
fetched segment (fMP4 media with its init), bounds each scenario, prints a
scoreboard, and writes JSON. A failure exits nonzero. It runs sequentially to
limit load. No service or real media server is contacted.

The saved before/after results are one local run, not a conformance claim.
The 503 case intentionally keeps the only source down, so a 502 is expected
but continuity still fails. The truncated case intentionally lies about the
original duration; after the fix it fails with 502 rather than an empty 200.
The live case exercises `LiveInput` on the transcode manager with finite input;
it does not exercise the Live TV relay or verify recovery from an encoder death.

This is a starting harness, not a complete chaos matrix. It does not yet drive
protocol endpoints, playback source failover, DASH, progressive MKV, downloads,
concurrent clients, timed sessions, mid-stream hardware failure, stalls,
short reads, subtitle tracks, timestamp and format changes, resource leak
checks, timeline monotonicity or A/V drift. VideoToolbox is exercised only for
healthy encoding. VAAPI and NVENC need suitable hardware. A PASS means only
that this specific short scenario met the checks described above.
