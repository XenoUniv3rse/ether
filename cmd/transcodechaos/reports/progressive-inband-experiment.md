# Progressive H.264 in-band parameter-set experiment

This is an experiment, not an enabled recovery path. On this Mac, VideoToolbox
and libx264 generated different avcC codec-private data for the same 320x180
H.264 input. A new complete Matroska file cannot be appended to an existing
one; this experiment kept the original header and appended only complete
clusters from a software encode with `-bsf:v dump_extra=freq=keyframe`.

To reproduce with the pinned ffmpeg, generate two three-second Matroska pipe
outputs from `testsrc2=size=320x180:rate=24` and
`sine=sample_rate=48000`, mapping the same audio and video in both. Use
`-c:v h264_videotoolbox -c:a aac -flush_packets 1 -f matroska pipe:1` for
the first. Use `-c:v libx264 -threads 2 -bsf:v dump_extra=freq=keyframe
-c:a aac -output_ts_offset 3.1 -flush_packets 1 -f matroska pipe:1` for the
second. Retain the first EBML/Segment header and its complete Cluster elements,
then append only the complete Cluster elements of the second; do not append a
second header. Decode the combined file with `ffmpeg -v error -xerror -i FILE
-f null -` and inspect stderr as well as the exit code.

One run with a 100 ms gap decoded without diagnostics. With a 3.0 second
replacement offset, ffmpeg logged non-monotonic audio DTS even though it exited
zero. Neither result proves compatibility with Kodi or Plex Desktop or with
other codecs, channel layouts and hardware. Do not enable this splice based on
this experiment. Current behavior aborts the response and uses software for a
new request; it does not claim seamless continuation.
