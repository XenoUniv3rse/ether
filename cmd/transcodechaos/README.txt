Transcode chaos (on demand; not part of default CI)

Run from the repository root with a local Go toolchain and the pinned ffmpeg:

  go run ./cmd/transcodechaos -mode short -corpus \
    -ffmpeg /path/to/ffmpeg -ffprobe /path/to/ffprobe -report chaos.json

Use -mode full for generated inputs, source faults, client sequences,
VideoToolbox failure injection, Live TV and the larger public-media matrix.
The command prints a scenario/path scoreboard and saves JSON. Unexpected
failures exit nonzero. EXPECTED_ERROR means permanently missing source bytes
were rejected promptly, with cleanup checked; XFAIL means a pinned stock
FFmpeg limitation was also rejected promptly by Ether. Neither is a playback
success. SKIP is an explicit coverage gap, not a pass.

-cache-dir selects a cache outside the repository with a 10 GB hard limit.
Public samples download only with -corpus, are verified against pinned hashes,
and are not redistributed. See corpus.go for upstream licence/usage links.
Netflix samples are trimmed locally to 30 seconds; conformance archives are
extracted with a size limit and muxed without re-encoding. -fixture-dir keeps
fixtures and raw offline diagnostic events outside the repository. Scenarios
run serially with a small Go CPU budget to limit load on shared machines.

Local mode exercises the transcode manager and real Plex handler fixtures
with loopback sources. It checks ffprobe/FFmpeg decode, timing, resource
cleanup, bounded memory/disk, generated damage, HTTP source faults, HLS TS
and fMP4, DASH, progressive Matroska, copy remux and download conversion.
The public subset includes official Matroska, GStreamer, Chromium, ITU AVC
and HEVC conformance, Netflix HDR, Apple HLS and DASH-IF media. Apple and
DASH-IF relay vectors are replayed from the cache over loopback, including
encoder interruption, rather than fetched live by Ether from the internet.

The full corpus client driver applies seek forward/backward, sampled scrubs
and fast-forward, pause/ping/resume, near-end playback, quality changes, and
available audio/subtitle changes across the streaming paths. These implement
gst-validate action concepts through Plex requests, not the GStreamer player
itself. Download conversion is a finite copy-only job, not a seekable session.
Very short conformance clips cannot run seek scenarios. HLS follows the
container actually negotiated; a HEVC target can be fMP4 even in the generic
HLS case. See each JSON row for actual coverage.

For a separate Ether lab server, pass -server-url URL and -media-key KEY
(or comma-separated keys) and set ETHER_CHAOS_TOKEN in the environment. Stage
short test titles through that lab's configured source first. The driver
checks Ether's health identity, refuses foreign media origins/redirects,
stops its own sessions, and removes only its own download-queue items.
Full lab mode drives HLS TS/fMP4, DASH, progressive MKV and download requests;
local fault injection and the full corpus client sequences use the isolated
handler fixtures. The URL mode does not upload or register corpus assets:
Ether has no local-media ingestion API. No external lab URL was available
for this run; origin/credential containment is checked with loopback fakes.
Never point it at a production server or a real Plex server.

expected-failures.json records only digest-pinned stock decoder limitations.
Currently ITU AVC FMO (FM2_SVA_C) is unsupported by the pinned FFmpeg. An
unlisted stock failure or a newly decodable expected failure requires triage.
Reports retain unresolved playable-source failures; do not silently add them
to the stock-decoder list.

Coverage limits: VAAPI/NVENC runtime and physical Shield/LG colour validation
require the corresponding hardware; local graph checks do not replace them.
Chromium supplies encoded DV5/DV8; Netflix's available DV/Atmos production
masters are not small packaged delivery streams suitable for this 10 GB tier.
PGS packet generation is unavailable with the pinned encoder. Jellyfin/Silo
HTTP endpoint matrices, automatic remote corpus staging, actual client rate
control, and exhaustive gst-validate scenarios remain outside this verified
matrix. A progressive hardware failure aborts at a complete cluster and a
new viewer-initiated request uses CPU; it is not seamless automatic resume.

See reports/corpus-tier2-before.json, reports/corpus-tier2-after.json and the
accompanying observations for measured results and remaining failures.
