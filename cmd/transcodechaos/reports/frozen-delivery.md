# Frozen transcode chaos batch for v0.3.4

Branch: `feat/transcode-chaos`. All tests use loopback fixtures or cached public
media; no production requests or issue comments were made.

| Run | Pass | Fail | Expected errors | Skip | Rows |
| --- | ---: | ---: | ---: | ---: | ---: |
| [Before expanded corpus](corpus-tier2-before.json) | 549 | 37 | 0 | 26 | 612 |
| [Complete rerun](corpus-tier2-run.json) | 585 | 6 | 3 | 27 | 621 |
| [Final with focused triage](corpus-tier2-after.json) | 592 | 4 | 3 | 27 | 626 |

The final scoreboard applies only the separately recorded
[focused reruns](corpus-tier2-focused.json) to the complete rerun. It retains
all four real failures. The full matrix gained a supported AVC input and
five vendor argument-graph checks; counts are not an identical input set.
Expected errors are two unrecoverable source faults and one unsupported
stock FFmpeg AVC FMO source, not playable results. No decoder limitation is
silently promoted to a pass.

Remaining failures: near-end unindexed Apple TS copy seeks for HLS TS,
HLS fMP4, progressive Matroska and remux. The live HLS relay passes. The full
source is decodable, so it is not in the stock expected-failures list.
See [root evidence and limits](corpus-tier2-observations.md) and the
[DV5/PCE measurements](dv5-pce-observations.md).

Verified fixes in this branch

| Cause | Fix commits | Evidence |
| --- | --- | --- |
| Advertised empty HLS output | f8df742a6 | Empty output no longer returns a successful media response. |
| Segmented hardware death ended on-demand sessions | b46f4d781, b9f3f9684 | Injected VideoToolbox death; software continuation and decoder continuity for TS/fMP4/DASH. |
| No encoder-progress watchdog | 57dd0a27e | Stalled loopback source restarts from unpublished output; bounded retry budget. |
| Progressive/download input stalls and cancellation | d0c6da5a5, 65bba3bdc | Real Plex transient 503/416, short/drop/stall/cancel and download tests. |
| Live corrupt-input exit ended the relay; live hardware retried the failed accelerator | integrated 334c993d3 via b6ea7ba75, plus 2380abf21 | Real Plex live relay before 502, after playable continuation/discontinuity; live hardware switches to CPU. |
| Progressive hardware failure could masquerade as finished playback | 9a66edd57, 1ad295192 | Real HTTP incomplete transfer at a complete cluster, 416 on byte-range retry, no false watched state, preserved resume position, fresh CPU response decodes. No seamless splice shipped. |
| AAC PCE only at source start left independent segments undecodable | 2fc123319, 1abd6b855, 11935ee1f | Exact PCE probe classification, audio-only conversion, short and 70-minute bounded-seek handler checks. |
| DV5 IPT picture sent through an unsuitable tone mapper | ae95ae631, 227387b08, 41523d220 | RPU-aware tonemapx; real Plex four-path decode, Mac visual comparison, all-vendor argument graphs. |
| Copied ADTS AAC not converted by nested HLS MP4 muxer | 9758f41e0 | Ordinary TS corpus fMP4 initialization and decode now pass. |
| Negative initial DTS wrapped into invalid MP4 tfdt | a2bc2f677 | Official HEVC conformance reproducer: wrapped tfdt before, zero after; real handler passes. |
| Damaged MP4 video packet killed copy-to-TS bitstream filter | e3b265dbe | Real Plex HLS previously 502 after first segment; now one-time encode continuation, playable segments and discontinuity. |

Integration notes

Preserve DV5-specific routing in args.go even when reconciling other hardware
and HEVC changes: tonemapx must consume decoder RPU side data with apply_dovi=1,
and both modern and legacy GPU filter branches must defer to it for DV5.
Keep hardware encoding where available. Direct/copy capability decisions are
not broadened or narrowed by this batch; do not strip DV5 RPU as HDR10.
Preserve the existing static-header contract and per-run in-band headers for
segmented fallback. Do not reintroduce the rejected progressive SPS/PPS splice.
The new core probe fields are additive optional timestamps; keep snapshot deep
copies and probe-facts version invalidation together with the bounded PCE seek.


Verification

The command's full opt-in matrix ran once for this batch. Focused reruns
classified the Apple copy warnings, repeated the intermittent stall deadline,
and checked all five vendor DV5 graphs. Quick existing session/timeline,
argument/FMP4, core/playback snapshot and URL-origin checks passed during the
corresponding steps. No full default suite or race suite was run. Hardware
runtime validation is VideoToolbox only; VAAPI/NVENC/QSV graphs are not GPU
runtime or colour validation. No external URL lab or physical Shield/LG
validation is claimed. The README states further endpoint/corpus gaps.

The integrated implementation at `43428f1f0` was read for critical conflict
resolution: both ReleaseID and SoftSubtitleStream, live start reset, stalled
recovery, CPU/copy fallbacks, DV5 graph routing and HTTP timeout were retained.
ProbeFactsVersion 5 correctly combines the two independent probe additions.
A borrowed SoftSubtitleStream request pointer was reported to integration to
copy into session-owned storage; the frozen worker branch does not contain
that other branch's field and does not attempt its fix.

Ready-to-post reply for #322

Corrupt live TS input could stop the encoder and leave the next Plex relay
segment returning 502. We reproduced that failure offline through the real
Plex handler. The fix tolerates damaged packets and restarts at the live edge
with continued segment numbering and a discontinuity; hardware failures switch
to software. The harness decoded the following segments after injected input
damage and encoder interruption. This ships in v0.3.4. If the stream still
returns to the guide after updating, please send the failure time and sanitized
server/client logs so we can match the next failure. Refs #322
