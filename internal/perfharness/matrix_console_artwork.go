package perfharness

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
)

func matrixConsoleArtworkScenarios() []Step {
	asset := matrixOnAirImage("#345678")
	replacement := matrixOnAirImage("#56789a")
	asset.Name, asset.Path = "fixture upload On-Air image", "/onair/assets"
	asset.Discover = discoverOnAirDigest("matrixUploadedArtwork")
	replacement.Name, replacement.Path = "fixture replace On-Air image", "/onair/assets/{matrixUploadedArtwork}/replace"
	replacement.Discover = discoverOnAirDigest("matrixReplacementArtwork")
	icon := matrixConsoleSVG("icon.svg", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128"><rect width="128" height="128" fill="#345678"/></svg>`, "")
	icon.Name, icon.Path = "fixture upload Vela icon", "/frontends/vela/icon"
	wordmark := matrixConsoleSVG("wordmark.svg", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 240 60"><rect width="240" height="60" fill="#56789a"/></svg>`, "")
	wordmark.Name, wordmark.Path = "fixture upload Vela wordmark", "/frontends/vela/wordmark"
	return []Step{
		asset,
		replacement,
		{Name: "fixture discard uploaded images", Method: http.MethodDelete, Path: "/onair/assets/{matrixReplacementArtwork}"},
		{Name: "fixture create look draft", Method: http.MethodPost, Path: "/onair/looks", Body: "{consoleLookDraft}", Discover: discoverMatrixID("matrixLookDraft")},
		{Name: "fixture save look draft", Method: http.MethodPut, Path: "/onair/looks/{matrixLookDraft}", Body: "{consoleLookDraft}"},
		{Name: "fixture detach standalone look", Method: http.MethodPost, Path: "/onair/looks/{matrixLookDraft}/detach"},
		{Name: "fixture duplicate look draft", Method: http.MethodPost, Path: "/onair/looks/{matrixLookDraft}/duplicate", Discover: discoverMatrixID("matrixLookCopy")},
		{Name: "fixture remove copied look", Method: http.MethodDelete, Path: "/onair/looks/{matrixLookCopy}"},
		{Name: "fixture remove look draft", Method: http.MethodDelete, Path: "/onair/looks/{matrixLookDraft}"},
		{Name: "fixture validate On-Air scene", Method: http.MethodPost, Path: "/onair/scenes/validate",
			Body: `{"duration":8,"poster":4,"layers":[{"id":"background","type":"rect","x":0,"y":0,"w":1920,"h":1080,"fill":"#345678"}]}`},
		icon,
		wordmark,
	}
}

func matrixOnAirImage(color string) Step {
	return matrixConsoleSVG("matrix.svg", `<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48"><rect width="48" height="48" fill="`+color+`"/></svg>`, "image")
}

func matrixConsoleSVG(filename, svg, kind string) Step {
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	// The static field and file names are valid and bytes.Buffer never
	// returns a write error.
	if kind != "" {
		_ = form.WriteField("kind", kind)
	}
	file, _ := form.CreateFormFile("file", filename)
	_, _ = io.WriteString(file, svg)
	_ = form.Close()
	contentType := form.FormDataContentType()
	return Step{Method: http.MethodPost, Body: buf.String(), Headers: func(h http.Header, _ map[string]string) {
		h.Set("Content-Type", contentType)
	}}
}

func discoverOnAirDigest(variable string) func(map[string]string, []byte) {
	return func(vars map[string]string, body []byte) {
		var asset struct {
			Digest string `json:"digest"`
		}
		if json.Unmarshal(body, &asset) == nil {
			setOnce(vars, variable, url.PathEscape(asset.Digest))
		}
	}
}
