Vela playback uses the shared broker and lazy transcode ladder. A ready HLS path
publishes scoped rendition URLs, dimensions and peak bitrates. codecStatus starts
pending with empty videoCodec; actual fMP4 initialization bytes supply RFC6381
video/audio codec tokens and promote that rung to verified in a newer plan/SSE
revision of the same generation. Merely opening a session or reading playlists
starts no encoder. Requesting a rendition's init/segment selects that rung.
The path URL is the ABR master; qualities[].url allows manual rendition selection.
The on-demand original-file fallback has no URL or prepared rungs before switch.

Track.delivery follows Vela protocol/v0: external (webvtt or webvtt-window plus
URL), embedded, or burn-in. Track IDs/streamIndex are actual full source stream
indices and remain stable across audio, subtitle and quality changes. PGS,
VobSub and DVB retain their image codec. They are never advertised as WebVTT.
Image subtitle selection and off use tracks commands, restart output at the last
accepted observed title position, and advance source and seek generations.
Audio selection follows that same command path. The source revision stays pinned.

WebVTT windows reuse the playback demux cue tee; no separate source scan or
subtitle encoder is launched. delivery.url and legacy track.url carry the scoped
media grant. Set from_ms to ABSOLUTE title milliseconds. Each response covers
[from_ms, from_ms + 120000); returned cue timestamps are ABSOLUTE title seconds,
including after seek. Convert HLS-relative media time at the player clock boundary
using path.timelineOriginSeconds. Empty windows mean cues have not been published
or there are no cues in that interval: retry while playback is interested (about
one second is sufficient), cancel requests on generation changes, and fetch the
next window before its end. Do not interpret empty windows as subtitle failure.

Original-file embedded delivery is offered only for explicitly declared source
subtitle codecs. Unsupported original text is omitted while its only possible
external URL would need an inactive HLS cue tee; it appears as external WebVTT
when fallback activates. Burn-in always means actual image subtitle burn-in.

File quality choices use the same broker ladder for direct-file, remux and
transcode starts. `primary` is the starting route (the menu's Original choice);
only actual original-file delivery carries `original:true`. Lower paths are
`quality-<rung ID>` with optional `quality:{width,height,bitrate}` target metadata
(bits/second). They are URL-less/on-demand until selected. The canonical Vela
protocol addition is d33421a. No label or decoder claim should be inferred from
an ID. The playback lead and protocol owner agreed these fields directly.

`switch` at absolute title seconds preserves position and advances both source
and seek generations. Actual HLS qualities remain real broker rendition URLs,
with pending/empty codecs until init bytes prove them. A prepared switch retains
the prepared timeline origin. Seek/track restarts invalidate prepared paths.
Original-file URLs stay ready and range-authorized while HLS is prepared/played.
See the [Vela session and events contract](../../../docs/protocols/vela-sessions.md)
for switching by bitrate and observation semantics.

`prepare-fallback` with positive maxBitrate selects the highest allowed lower rung
whose peak bitrate fits the limit, requests a bounded media range on the existing
ladder, and reports ready only after publication. It leaves the selected path and
generations unchanged. The host advertises proactiveFallback only through the
optional broker quality service backed by the local manager; unsupported hosts
keep it false. Capacity is still checked on actual output activation. Explicit
quality sessions retire the old run before starting a replacement, so the ladder
has one encoder at a time. Other surfaces retain their existing behavior.

Coordinator lab checks (no deployment/streaming performed by this worker):
- 1917 primary H264-copy/AAC remux: menu shows Original first plus lower choices;
  opening the session and fetching metadata starts no unused encoder.
- Choose 720p at a nonzero position, then Original; verify position/audio/subtitle
  selection and source/seek generations, including subsequent seek/track changes.
- While Original plays, prepare-fallback at 2000000 bits/second; selected path and
  generations stay unchanged, fallback becomes ready only with a real segment;
  switching to its path reuses the prepared origin and output.
- Observe process/admission diagnostics: at most one active encoder, repeated
  prepare reuses cached output, stop cleans up. Check MSE and native HLS Auto.
- Repeat with original-file and transcode starts, allowTranscode=false (Original
  only), constrained decoder/size/bitrate caps, and exhausted encoder capacity.

Window responses carry `X-Ether-Subtitle-Complete`. `0` means a growing window
read from the live cue tee: keep polling as described above. `1` means the body
is the complete track (cached extraction or sidecar file) with absolute cue
times: stop polling that track for this generation.

Explicit Vela quality ladder (w0312/vela-quality-ladder)
------------------------------------------------------
Vela's menu is now independent of the shared ABR throughput-drop heuristics.
Original/primary stays first. Source-sized and lower choices use video targets:
2160p 40 Mbps; 1080p 20/12/8; 720p 6/4; 480p 1.5; 360p 0.8. Scope movies use
width-based classes and report actual even dimensions; no rung upscales.
The shared Plex/Jellyfin candidate ladder and its spacing remain unchanged.
One shared correctness fix uses explicitly selected audio channel counts when
estimating encoded audio bitrate, matching the actual encoder/video budget.

HEVC requires both the declared decoder and the selected, already-probed local
hardware encoder. Software libx265 remains excluded from real-time planning.
Vela's hdr list is passed into capability construction; exact allowed formats
are preserved after owner rules. Kept HDR requires the matching declared HDR10
or HLG format. Encoded DV delivers its supported base range, never promises
preserved DV metadata. DV-only displays do not implicitly authorize HDR10/HLG.
A capable HDR client gets an additional SDR tone-mapped 2160p choice. Without
HDR display/HEVC support all generated rungs are SDR tone maps. Missing tone-map
support and unsupported DV5 reshaping omit affected choices.

The blanket MSE 720p/4 Mbps tone-map safety cap is removed: MSE alone supplies
no evidence of a decoder or encoder resolution limit. Source geometry, explicit
size limits, codec-specific height/profile limits, output profile conditions,
viewer/remote bitrate ceilings and allowTranscode remain authoritative. HEVC
level-limited clients remain conservative (decoder_level_unverified): there is
no enforced HEVC level contract yet, so estimated RFC6381 is not proof.
qualityLimitations contains {height,reason} stable codes for absent rung
candidates (always height > 0; never height 0):
resolution_limit, decoder_resolution_limit, bitrate_limit,
decoder_profile_limit, decoder_level_unverified, decoder_unavailable,
encoder_tonemap_unavailable, encoder_dv5_tonemap_unavailable,
encoder_hevc_hdr_unavailable, decoder_hevc_unavailable, and
display_hdr_undeclared.

Both on-demand path.quality and prepared qualities[] expose target width/height,
videoBitrate, peakVideoBitrate, averageBitrate (video+selected audio), bitrate
(total peak, including mux/reservoir budget), codec family and intended
output dynamicRange. All rates are bits/second. Peak video excludes audio/mux
and includes the reservoir budget over the HLS peak window. Targets are not
output verification. videoCodec stays empty/pending until real init bytes prove
RFC6381, then both rendition and corresponding path target become verified.
Protocol owner agreed these fields in canonical protocol commit 8b7a74a.

Example: 3840x2160 HDR10, 192 kbps copied audio, no caps, HEVC hardware available:

 class/video Mbps  total average Mbps  total peak Mbps
 2160/40           40.192              48.230
 1080/20           20.192              24.230
 1080/12           12.192              14.630
 1080/8             8.192               9.830
 720/6              6.192               7.430
 720/4              4.192               5.030
 480/1.5            1.692               2.030
 360/0.8            0.992               1.190

HEVC+HDR10: Original, HEVC 2160 HDR10, HEVC 2160 SDR, then the remaining
rows in HEVC HDR10. H.264-only: Original, then all rows in H.264 SDR.
480p is 852x480 with the existing even/aspect scaler; other 16:9 sizes are
3840x2160,1920x1080,1280x720,640x360. Original source rates remain source facts;
these explicit target figures describe generated choices.

Offline checks: pure-Go build, vet on touched packages; focused transcode
quality invariants (no upscale, declared HDR/decoder, real targets, rule caps,
no process during planning and cached preparation reuse), broker policy test,
and Vela quality switch/position invariant test passed.

Coordinator lab checks (worker did not deploy or access production/:32550):
- With a real 2160p HDR10 source compare HEVC+HDR10, HEVC SDR-display and
  H.264-only clients. Confirm the menu/limitations and actual init codecs.
- Select 2160 SDR and every 1080 bitrate; inspect real dimensions, rate,
  HDR tags/colour, audio channel count and tone mapping. Repeat HDR/HLG/DV
  base-layer sources; profile 5 must refuse without reshaping support.
- Keep Original playing while reading plans/playlists: zero unused encoders.
  Switch at nonzero position among same-size bitrate/HDR/SDR choices and back
  to Original; preserve position and audio/subtitle selection. Observe at most
  one encoder per session, including prepare-fallback and repeated switches.
- Apply owner resolution/profile ceilings, viewer/remote bitrate caps and
  allowTranscode=false; check omitted choices explain themselves. Test encoder
  capacity on actual activation. Verify Plex/Jellyfin ladder/wire parity in the
  coordinator's full gate. Inspect HEVC HDR encoder support on lab hardware;
  planning trusts existing hardware probes, not new per-session probes.
