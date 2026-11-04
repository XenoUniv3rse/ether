# DV profile 5 and PCE AAC observations

On the pinned Jellyfin FFmpeg, the Chromium profile-5 sample previously
returned HTTP 502 for Plex HLS TS, HLS fMP4, DASH and progressive Matroska.
The software graph selected zscale for a Dolby IPT picture that needs RPU
reshaping; zscale reported that no colorspace path existed. With the DV-aware
`tonemapx` graph, all four endpoint paths returned media that ffprobe opened
and FFmpeg decoded. A VideoToolbox HLS run used
`tonemap_videotoolbox=...:apply_dovi=1` with a hardware encoder and decoded.

A visual comparison used the first frame from the real Plex HLS responses and
a direct FFmpeg decode of the same public file through `tonemapx`. The CPU
output closely resembled the reference. The VideoToolbox output was visibly
brighter and less saturated, consistent with a different tone-mapping
implementation; this is a visual observation of one frame, not a calibrated
colour or display conformance test. The public media and frame grabs remain
outside the repository.

The AAC PCE fixture previously failed fMP4 HLS initialization with HTTP 502.
The configuration exists only in the beginning of this TS source; the
`aac_adtstoasc` copy filter also fails after a seek past it. Audio-only
conversion for the identified PCE source produces independently decodable TS
and fMP4 segments. An offset-six-second request through each real Plex path
returned audio packets and decoded; one measured fMP4 start was 6.021 s for
video and 5.979 s for audio. This seek decodes from the beginning to retain the
PCE, so startup time and source I/O grow with the offset. Long-title seeks
remain a performance limitation, not a tested bounded-latency guarantee.

Follow-up: routing profile-5 tone mapping through `tonemapx` with a
VideoToolbox encoder removed the visible brightness/saturation difference in
the same side-by-side first-frame comparison. The hardware HLS output decoded
successfully. This remains a single-sample visual check, not calibrated display
validation. The earlier PCE seek approach is being replaced because its
source reads grow with the offset.
