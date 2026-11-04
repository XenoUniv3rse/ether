# Public corpus tier: evidence and limits

The complete matrix is retained in `corpus-tier2-before.json` and
`corpus-tier2-after.json`. These are measured runs, not a claim of exhaustive
player compatibility. Media and raw diagnostics stay outside the repository.

## Root fixes

- `11935ee1f`: PCE AAC seeks prime the decoder with a short source prefix,
  followed by a normal bounded HTTP seek. At a synthetic 70-minute offset,
  the real Plex handler served 14,782,928 of 31,092,192 source bytes after
  probing. It did not scan 70 minutes linearly. See `dv5-pce-observations.md`
  for the sparse-timestamp qualification and header/fallback restrictions.
- `9758f41e0`: the HLS child MP4 muxer did not automatically convert copied
  ADTS AAC. Explicit `aac_adtstoasc`, chained after seek preroll filtering,
  fixes initialization failures for ordinary TS AAC. Audio conversion remains
  narrowly selected for the separate PCE-only class.
- `a2bc2f677`: official HEVC BUMPING input starts with negative DTS. The HLS
  child MP4 muxer wrapped a negative tfdt into uint64, which Ether correctly
  refused as an invalid clock. `avoid_negative_ts=make_non_negative` in the
  child muxer produces tfdt zero while retaining edit-list reconciliation.
- `e3b265dbe`: corrupted H264 length fields made the MP4-to-Annex-B copy
  filter exit after one TS segment; the next real Plex request was HTTP 502.
  A one-time video encode fallback resumes at the first unpublished segment,
  preserves the segment plan and completed media, and marks discontinuity.
  It applies only to that fatal SDR H264 TS copy conversion; no static-header
  splice or speculative repair of copied damage is introduced.
- `41523d220`: the legacy VAAPI branch could bypass the DV5 exclusion in
  gpuPipeline and use a non-RPU-aware mapper. Both GPU filter branches now
  defer DV5 conversion to tonemapx. The opt-in graph matrix checks CPU,
  VideoToolbox, NVENC, VAAPI and QSV plans. Only Mac runtime is validated.

The earlier DV5 commits are `ae95ae631` (RPU-aware CPU graph) and
`227387b08` (consistent Mac colour mapping while retaining VT encoding).
The Chromium DV5 sample decodes through real Plex HLS TS/fMP4, DASH and
progressive MKV. The Mac first-frame comparison is visual, not a calibrated
colour/display test. Shield direct/copy decisions are unchanged: compatible
DV copy never invokes tone mapping. No Shield/LG crash fix is established.

## Triage

The previously reported eight damaged-MKV failures were four paths repeated
in two scoreboards. A strict `-xerror` assertion treated concealed source
artifacts as fatal. Independent opening keyframes and ordinary full decodes
passed. Those copied artifacts are not repaired or counted as playback bugs.

ITU AVC FM2 uses unsupported FMO. Stock FFmpeg reports it unimplemented and
produces no decoded frames. Its archive digest and reason are in
`expected-failures.json`; Ether rejects it promptly and releases resources.
The supported AVC BASQP vector and two HEVC conformance vectors also run.

Permanent HTTP 503 and a truncated source with no alternate cannot produce
missing media. Their bounded 502 and resource cleanup are EXPECTED_ERROR,
not successful playback. Earlier scoreboards called them FAIL. Transient
503/416, dropped/short reads, changed content and reported-size mismatch
retain playback assertions.

The five-second persistent-stall case hit HTTP 504 once under concurrent
machine load; its focused unchanged rerun passed in 2.875 seconds. Both
observations are retained (`corpus-stall-rerun.json`); the timeout was not
relaxed to hide it.

Apple HLS playback and live encoder restart pass through the actual relay.
The same cached three-segment TS excerpt is also deliberately treated as
unindexed VOD. Stock FFmpeg input-seeking to 14.965 or 28.430 seconds emits
leading AAC/H264 warnings too. One real HLS scrub output contained an
independently decoded keyframe, 150 video frames and 104 audio frames and
completed ordinary decode. That warning is cosmetic and now uses the same
playability standard as copied damaged MKV, plus an audio-frame check.

Seeking this unindexed TS excerpt near its end still cannot produce an
independent video start across all copy paths: the final keyframe is before
the requested offset. This remains a real playback limitation, not an
expected stock-undecodable file. It is retained as FAIL. A safe repair needs
bounded keyframe-aware seek preroll and explicit timestamp/response semantics
for progressive copy; blindly scanning from the beginning or passing an
interframe as a new start is unacceptable. The corpus is decodable from its
start and must not enter the expected-failures list.

## Coverage limits

- Remote URL mode requires explicitly staged lab titles. No Ether local-media
  ingestion API exists, so it cannot automatically register cached corpus
  files on a separate server. URL origin checks run offline; no external lab
  server was supplied. The local matrix uses real Plex handler fixtures.
- No VAAPI/NVENC runtime, physical Shield/LG, Jellyfin/Silo endpoint matrix,
  exhaustive gst-validate execution, or real client rate-control validation.
- Netflix HDR is covered by a local 30-second cut. Available DV/Atmos masters
  are production assets, not small packaged delivery streams within this
  tier's cache budget. Chromium encoded DV5/DV8 supplies DV coverage.
- No generated PGS fixture: the pinned encoder cannot make one. Very short
  conformance inputs skip inapplicable seek scenarios; unsupported copy
  targets skip remux while their transcode paths still run.
- Process, FD, goroutine, memory and temp cleanup assertions cover isolated
  manager scenarios; they do not establish resource bounds for every real
  client or every external-server workload.
