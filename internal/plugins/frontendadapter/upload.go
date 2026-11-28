package frontendadapter

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/plugins"
)

const (
	maxUploadParts    = 64
	maxUploadMetadata = 128 << 10
)

var (
	errUploadMetadata  = errors.New("adapter upload metadata exceeds limit")
	errMalformedUpload = errors.New("malformed adapter upload")
)

type uploadManager interface {
	BeginAdapterUpload(context.Context, string, string, string, int64) (*blobcache.Upload, int64, error)
}

func (f *Frontend) readUpload(w http.ResponseWriter, r *http.Request, subpath string) (*plugins.FrontendUpload, func(), error) {
	manager, ok := f.mgr.(uploadManager)
	if !ok {
		return nil, nil, plugins.ErrUnavailable
	}
	stage, limit, err := manager.BeginAdapterUpload(r.Context(), f.decl.PluginID, f.decl.Meta.Key, subpath, r.ContentLength)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			stage.Close()
		}
	}()
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	upload := &plugins.FrontendUpload{ContentType: r.Header.Get("Content-Type")}
	mediaType, _, _ := strings.Cut(upload.ContentType, ";")
	// A multipart request must declare a usable boundary. Other types are
	// passed through as supplied by the client.
	if strings.EqualFold(strings.TrimSpace(mediaType), "multipart/form-data") {
		_, params, parseErr := mime.ParseMediaType(upload.ContentType)
		if parseErr != nil || params["boundary"] == "" {
			err = errMalformedUpload
		} else {
			err = splitUpload(r.Context(), body, params["boundary"], stage, upload)
		}
	} else {
		upload.Blob, err = stagePart(r.Context(), stage, body, "", "", upload.ContentType)
	}
	if err != nil {
		return nil, nil, err
	}
	return upload, stage.Close, nil
}

func stagePart(ctx context.Context, stage *blobcache.Upload, body io.Reader, field, filename, ct string) (*plugins.FrontendUploadBlob, error) {
	if ct == "" {
		ct = "application/octet-stream"
	}
	ref, stat, err := stage.Stage(ctx, filename, ct, body)
	if err != nil {
		return nil, err
	}
	return &plugins.FrontendUploadBlob{
		Ref: ref.String(), Size: stat.Size, ContentType: ct, Field: field, Filename: filename,
		SHA256: stat.SHA256, SHA1: stat.SHA1, MD5: stat.MD5, CRC32: stat.CRC32,
	}, nil
}

func splitUpload(ctx context.Context, body io.Reader, boundary string, stage *blobcache.Upload, upload *plugins.FrontendUpload) error {
	reader := multipart.NewReader(body, boundary)
	upload.Fields = make(map[string][]string)
	remaining := maxUploadMetadata
	for parts := 0; ; parts++ {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			// Count the epilogue, including bytes beyond multipart's buffer.
			_, err = io.Copy(io.Discard, body)
			return err
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) || errors.Is(err, multipart.ErrMessageTooLarge) {
				return err
			}
			return errMalformedUpload
		}
		if parts >= maxUploadParts {
			return errUploadMetadata
		}
		disposition, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		field := params["name"]
		if err != nil || disposition != "form-data" || field == "" {
			return errMalformedUpload
		}
		filename := part.FileName()
		ct := part.Header.Get("Content-Type")
		remaining -= len(field) + len(filename) + len(ct)
		if remaining < 0 {
			return errUploadMetadata
		}
		if _, isFile := params["filename"]; isFile {
			blob, err := stagePart(ctx, stage, part, field, filename, ct)
			if err != nil {
				return err
			}
			upload.Files = append(upload.Files, *blob)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, int64(remaining)+1))
		if err != nil {
			return err
		}
		if len(data) > remaining {
			return errUploadMetadata
		}
		remaining -= len(data)
		upload.Fields[field] = append(upload.Fields[field], string(data))
	}
}

func writeUploadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, blobcache.ErrQuotaFull):
		w.WriteHeader(http.StatusInsufficientStorage)
	case errors.As(err, &tooLarge), errors.Is(err, blobcache.ErrTooLarge),
		errors.Is(err, errUploadMetadata), errors.Is(err, multipart.ErrMessageTooLarge):
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	case errors.Is(err, errMalformedUpload), errors.Is(err, io.ErrUnexpectedEOF):
		w.WriteHeader(http.StatusBadRequest)
	default:
		writePluginError(w, err)
	}
}
