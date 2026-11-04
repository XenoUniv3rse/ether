Transcode chaos (on demand; not part of default CI)

Run from the repository root with a local Go toolchain and the pinned ffmpeg:

  go run ./cmd/transcodechaos -mode short -corpus \
    -ffmpeg /path/to/ffmpeg -ffprobe /path/to/ffprobe -report chaos.json

Use -mode full for the larger generated-input, source-fault, lifecycle,
VideoToolbox, Live TV and public-media matrices. The command prints a
scenario/path scoreboard and saves JSON. A failing check exits nonzero.
Set -cache-dir to select a local cache outside the repository; its hard limit
is 10 GB. The curated public samples are downloaded only when -corpus is set,
verified against pinned hashes, and remain in the cache. Refer to the upstream
licence links in corpus.go; no public media is redistributed here. Netflix
samples are cut to 30 seconds locally. -fixture-dir can preserve generated
fixtures outside the repository for investigation. Runs are serial to limit
load on a shared machine.

The local mode exercises the real transcode manager and the existing Plex
handler fixtures, using loopback-only media sources. It runs ffprobe and
FFmpeg decode checks and exercises generated damaged inputs, client requests,
source faults, segmented output, direct-stream remux, download conversion,
and the live relay. The corpus includes representative Matroska, GStreamer,
Chromium, Netflix and Apple samples. Apple HLS is replayed via a local
byte-range playlist, not fetched by the relay from the internet. See the JSON
for actual cases, skips, and failures; a passing run is not a proof of
compatibility with every codec or player.

To exercise a separate Ether lab server, explicitly pass -server-url URL and
-media-key RATING_KEY and set ETHER_CHAOS_TOKEN in the environment. Stage a
short test title on that server first. The driver verifies the Ether health
endpoint, refuses cross-origin media URLs and redirects, and stops the sessions
it starts. The lab mode currently covers HLS, DASH, and progressive MKV;
it does not upload corpus files, drive downloads, or run the complete local
fault and lifecycle matrix against the remote server. Never use a production
server or a real Plex server as the target.

expected-failures.json contains only pinned public inputs confirmed to be
undecodable by stock FFmpeg, with a reason. An unlisted stock decode failure
is reported for triage; it is not automatically accepted. The currently
curated list is empty.

Known coverage gaps: public HEVC/AVC conformance bitstreams, DASH-IF relay
vectors, Netflix HDR/Dolby Vision/Atmos cuts, full gst-validate scenarios on
every file and path, external-server corpus staging, and VAAPI/NVENC hardware
require further work. PGS and Dolby Vision synthetic fixtures may be skipped
when unavailable. A failed progressive hardware stream is aborted at a complete cluster and a
new request uses software; automatic resume by the client is not guaranteed.
