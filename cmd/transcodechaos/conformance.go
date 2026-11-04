package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Extract one elementary stream without using archive paths on disk. Both
// extraction and mux output fit inside the shared corpus budget.
func muxConformance(ctx context.Context, ff, cache, input, digest string) (string, error) {
	suffix := ".mp4"
	// FMO is deliberately retained as raw AVC: the pinned stock decoder
	// cannot decode it, so the expected-failure case tests clean rejection.
	if filepath.Base(input) == "FM2_SVA_C.zip" {
		suffix = ".264"
	}
	out := filepath.Join(cache, "conformance-"+digest[:16]+suffix)
	if st, err := os.Lstat(out); err == nil {
		if !st.Mode().IsRegular() || st.Size() == 0 {
			return "", fmt.Errorf("invalid cached mux")
		}
		return out, nil
	}
	used, err := diskBytes(cache)
	if err != nil || maxCorpusBytes-used < 64<<20 {
		return "", fmt.Errorf("insufficient cache budget for mux")
	}
	z, err := zip.OpenReader(input)
	if err != nil {
		return "", err
	}
	defer z.Close()
	var stream *zip.File
	for _, f := range z.File {
		if strings.HasSuffix(f.Name, ".bit") || strings.HasSuffix(f.Name, ".264") {
			if stream != nil || f.UncompressedSize64 > 16<<20 {
				return "", fmt.Errorf("unexpected conformance archive")
			}
			stream = f
		}
	}
	if stream == nil {
		return "", fmt.Errorf("archive has no bitstream")
	}
	r, err := stream.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	format := "hevc"
	if strings.HasSuffix(stream.Name, ".264") || strings.HasSuffix(stream.Name, ".jsv") {
		format = "h264"
	}
	raw, err := os.CreateTemp(cache, ".conformance-*.hevc")
	if err != nil {
		return "", err
	}
	defer os.Remove(raw.Name())
	n, copyErr := io.Copy(raw, io.LimitReader(r, 16<<20+1))
	closeErr := raw.Close()
	if copyErr != nil || closeErr != nil || n > 16<<20 {
		return "", fmt.Errorf("invalid bitstream")
	}
	if suffix == ".264" {
		if err := os.Link(raw.Name(), out); err != nil {
			return "", err
		}
		return out, nil
	}
	tmp, err := os.CreateTemp(cache, ".conformance-*.mp4")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(name)
	if err := command(ctx, ff, "-v", "error", "-y", "-fflags", "+genpts", "-r", "25", "-f", format, "-i", raw.Name(), "-t", "30", "-map", "0:v:0", "-c", "copy", "-fs", "33554432", "-movflags", "+faststart", name); err != nil {
		return "", fmt.Errorf("FFmpeg mux failed")
	}
	st, err := os.Stat(name)
	if err != nil || st.Size() == 0 || st.Size() > 40<<20 {
		return "", fmt.Errorf("invalid mux size")
	}
	if err := os.Link(name, out); err != nil {
		return "", err
	}
	return out, nil
}
