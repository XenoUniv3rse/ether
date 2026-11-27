package frontendadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// instance is a mounted adapter: the proxy handler plus the per-mount
// limiters. It holds only Manager and identity; it cannot reach another
// frontend's Host seams.
type instance struct {
	frontend *Frontend
	host     frontend.Host
	auth     *authenticator
	handler  http.Handler
	limits   *limiters
}

var _ frontend.Instance = (*instance)(nil)

func (i *instance) Handler() http.Handler { return i.handler }

// Listeners places the adapter on the shared listener under its path
// prefix by default, or on its own port when defaultPort is set (spec
// §6.5). The host decides whether the owner enabled the port; the adapter
// only advertises what the manifest asked for.
func (i *instance) Listeners() []frontend.ListenerSpec {
	prefix := i.frontend.decl.Meta.PathPrefix
	if port := i.frontend.decl.Meta.DefaultPort; port > 0 {
		return []frontend.ListenerSpec{{Address: ":" + strconv.Itoa(port), PathPrefix: prefix, Shared: false}}
	}
	return []frontend.ListenerSpec{{PathPrefix: prefix, Shared: true}}
}

func (i *instance) Discovery() []frontend.Announcer { return nil }

func (i *instance) Status(context.Context) frontend.Status {
	return frontend.Status{Mounted: true}
}

func (i *instance) Close(context.Context) error { return nil }

// proxyHandler builds the HTTP handler that authenticates, rate-limits and
// relays each request to the plugin.
func (f *Frontend) proxyHandler(inst *instance) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subpath := stripPrefix(r.URL.Path, f.decl.Meta.PathPrefix)
		limit := f.decl.Meta.UploadLimit(subpath)
		if f.decl.Meta.Auth == AuthQueryCredentials {
			// Query credentials carry no ambient authority (no cookies or
			// browser-held auth), so browser clients on any origin, such as
			// web Subsonic players, may call the adapter.
			if allowAnyOrigin(w, r) {
				return
			}
			if limit == 0 {
				if err := mergeFormIntoQuery(r); err != nil {
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
			}
		}
		auth, err := inst.auth.authenticate(r.Context(), r)
		if err != nil {
			if f.decl.Meta.Auth == AuthQueryCredentials && errors.Is(err, errUnauthenticated) {
				f.relayUnauthenticated(w, r, inst)
				return
			}
			writeAuthError(w, err)
			return
		}
		if !inst.limits.allow(auth.viewer.ID, time.Now()) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		req := plugins.Request{
			PluginID:      f.decl.PluginID,
			Method:        r.Method,
			Path:          pluginPath(f.decl.Meta.Key, subpath),
			Query:         firstValues(f.pluginQuery(r)),
			RepeatedQuery: repeatedValues(f.pluginQuery(r)),
			Headers:       outboundHeaders(r, auth, requestOrigin(r, inst.host)),
		}
		if limit > 0 && r.Body != nil && r.Body != http.NoBody {
			upload, cleanup, err := f.readUpload(w, r, subpath, limit)
			if err != nil {
				writeUploadError(w, err)
				return
			}
			defer cleanup()
			req.Upload = upload
			req.ContentType = upload.ContentType
		} else {
			body, err := readBody(r)
			if err != nil {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			setRequestBody(&req, r.Header.Get("Content-Type"), body)
		}
		resp, stream, err := f.call(r.Context(), req)
		if err != nil {
			writePluginError(w, err)
			return
		}
		if resp.Blob != nil {
			if stream != nil {
				stream.Close()
			}
			f.writeBlob(w, r, auth, resp)
			return
		}
		if resp.Media != nil || resp.Art != nil {
			if stream != nil {
				stream.Close()
			}
			f.writeHostMedia(w, r, auth, resp)
			return
		}
		if stream != nil {
			defer stream.Close()
			writeStream(w, resp, stream)
			return
		}
		writeResponse(w, resp)
	})
}

// mergeFormIntoQuery folds a form-encoded POST body into the query string.
// Query-credential protocols (Subsonic) let clients send every parameter,
// credentials included, either way; after this the request is handled exactly
// like the equivalent GET.
func mergeFormIntoQuery(r *http.Request) error {
	if r.Method != http.MethodPost || r.Body == nil {
		return nil
	}
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(ct), "application/x-www-form-urlencoded") {
		return nil
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	r.Body = http.NoBody
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil
	}
	q := r.URL.Query()
	for k, vs := range form {
		q[k] = append(q[k], vs...)
	}
	r.URL.RawQuery = q.Encode()
	return nil
}

// allowAnyOrigin sets permissive CORS headers and answers a preflight. It
// reports true when the request was a preflight and has been answered.
func allowAnyOrigin(w http.ResponseWriter, r *http.Request) bool {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, Content-Type")
	if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		h.Set("Access-Control-Allow-Headers", req)
	}
	h.Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// UnauthenticatedPath is the reserved plugin sub-path under which a
// query-credentials adapter receives requests that carried no valid
// credentials. They arrive without any viewer identity, so the plugin can
// answer only with its protocol's authentication error or a public document
// (for example a capabilities list the protocol defines as auth-free).
const UnauthenticatedPath = "/.unauthenticated"

// anonymousViewer is the shared rate-limit bucket for unauthenticated relays.
const anonymousViewer int64 = 0

// pluginQuery is the query the plugin sees: query credentials are removed so
// the plugin never receives a password or a token.
func (f *Frontend) pluginQuery(r *http.Request) map[string][]string {
	if f.decl.Meta.Auth == AuthQueryCredentials {
		return stripQueryCredentials(r.URL.Query())
	}
	return r.URL.Query()
}

// relayUnauthenticated forwards a request without valid query credentials to
// the plugin under UnauthenticatedPath, with no viewer identity, so the client
// gets the protocol's own error document instead of a bare HTTP 401. Relays
// share one anonymous rate-limit bucket; anything the plugin cannot answer
// falls back to 401.
func (f *Frontend) relayUnauthenticated(w http.ResponseWriter, r *http.Request, inst *instance) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeAuthError(w, errUnauthenticated)
		return
	}
	if !inst.limits.allow(anonymousViewer, time.Now()) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	headers := make(map[string]string, len(forwardedRequestHeaders))
	for _, name := range forwardedRequestHeaders {
		if v := r.Header.Get(name); v != "" {
			headers[name] = v
		}
	}
	req := plugins.Request{
		PluginID: f.decl.PluginID,
		Method:   r.Method,
		Path:     pluginPath(f.decl.Meta.Key, UnauthenticatedPath+stripPrefix(r.URL.Path, f.decl.Meta.PathPrefix)),
		Query:    firstValues(f.pluginQuery(r)),
		Headers:  headers,
	}
	resp, stream, err := f.call(r.Context(), req)
	if stream != nil {
		defer stream.Close()
	}
	if err != nil || resp.Blob != nil || resp.Media != nil || resp.Art != nil || resp.Status == http.StatusNotFound {
		writeAuthError(w, errUnauthenticated)
		return
	}
	if stream != nil {
		writeStream(w, resp, stream)
		return
	}
	writeResponse(w, resp)
}

// call relays one request. It streams when the plugin runtime supports it
// and falls back to a buffered, size-capped Call for hosted plugins that
// serve small protocol documents (spec §6.5).
func (f *Frontend) call(ctx context.Context, req plugins.Request) (plugins.Response, io.ReadCloser, error) {
	resp, stream, err := f.mgr.CallStream(ctx, f.decl.PluginID, req)
	if err == nil {
		return resp, stream, nil
	}
	if !errors.Is(err, plugins.ErrStreamUnsupported) {
		return plugins.Response{}, nil, err
	}
	resp, err = f.mgr.Call(ctx, f.decl.PluginID, req)
	if err != nil {
		return plugins.Response{}, nil, err
	}
	if int64(len(resp.Body)) > MaxHostedResponseBytes {
		return plugins.Response{}, nil, errors.New("frontendadapter: hosted response exceeds cap")
	}
	// A hosted plugin's body is the JSON value it returned. A string body is
	// the document itself (XML, HTML, plain text), so it is written unquoted;
	// object and array bodies are already the JSON document.
	if len(resp.Body) > 0 && resp.Body[0] == '"' {
		var text string
		if json.Unmarshal(resp.Body, &text) == nil {
			resp.Body = []byte(text)
		}
	}
	return resp, nil, nil
}

// setRequestBody preserves exact inline bytes and includes a parsed body only for JSON.
func setRequestBody(req *plugins.Request, contentType string, body []byte) {
	if len(body) == 0 {
		return
	}
	req.BodyBase64 = base64.StdEncoding.EncodeToString(body)
	req.ContentType = contentType
	if json.Valid(body) {
		req.Body = body
	}
}

// readBody reads an inbound request body, capping it at MaxRequestBodyBytes.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > MaxRequestBodyBytes {
		return nil, errors.New("request body too large")
	}
	return body, nil
}

// writeStream relays a streaming plugin response.
func writeStream(w http.ResponseWriter, resp plugins.Response, stream io.Reader) {
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	copyResponseHeaders(w, resp.Headers)
	copyRedirect(w, resp)
	w.WriteHeader(status)
	_, _ = io.Copy(w, stream)
}

// writeResponse relays a buffered plugin response.
func writeResponse(w http.ResponseWriter, resp plugins.Response) {
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	copyResponseHeaders(w, resp.Headers)
	copyRedirect(w, resp)
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

// copyResponseHeaders copies the allowlisted plugin response headers.
func copyResponseHeaders(w http.ResponseWriter, headers map[string]string) {
	for _, name := range forwardedResponseHeaders {
		if v := headerValue(headers, name); v != "" {
			w.Header().Set(name, v)
		}
	}
}

// headerValue looks up a header case-insensitively.
func headerValue(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// writeAuthError maps an authentication failure. A hidden library answers
// 404 (never 403) so its existence does not leak; missing credentials
// answer 401 with a Basic challenge so readers prompt for one.
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRateLimited):
		w.Header().Set("Retry-After", "900")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	case errors.Is(err, errNotVisible):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		w.Header().Set("WWW-Authenticate", `Basic realm="Ether"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// writePluginError maps a plugin-call failure to a gateway error.
func writePluginError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, plugins.ErrBusy):
		w.Header().Set("Retry-After", "5")
		http.Error(w, "adapter busy", http.StatusServiceUnavailable)
	case errors.Is(err, plugins.ErrDisabled):
		http.Error(w, "adapter disabled", http.StatusServiceUnavailable)
	case errors.Is(err, plugins.ErrUnsupportedBody):
		http.Error(w, "unsupported request body", http.StatusUnsupportedMediaType)
	default:
		http.Error(w, "adapter unavailable", http.StatusBadGateway)
	}
}

// stripPrefix removes the adapter's path prefix so the plugin sees the
// path it declared. A request outside the prefix keeps its full path.
func stripPrefix(path, prefix string) string {
	if prefix != "" && strings.HasPrefix(path, prefix) {
		rest := strings.TrimPrefix(path, prefix)
		if rest == "" {
			return "/"
		}
		return rest
	}
	return path
}

// maxRepeatedValues bounds how many values of one repeated query key reach a
// plugin, so a request cannot fan one call into unbounded work.
const maxRepeatedValues = 500

// repeatedValues keeps every value of keys that appear more than once, for
// runtimes that can deliver them (hosted plugins receive arrays).
func repeatedValues(values map[string][]string) map[string][]string {
	var out map[string][]string
	for k, v := range values {
		if len(v) < 2 {
			continue
		}
		if out == nil {
			out = make(map[string][]string)
		}
		out[k] = append([]string(nil), v[:min(len(v), maxRepeatedValues)]...)
	}
	return out
}

// firstValues flattens a query into the map[string]string the plugin
// Request carries, keeping the first value per key.
func firstValues(values map[string][]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
