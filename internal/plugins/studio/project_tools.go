package studio

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/agent/tools"
)

// ProjectTools use the same draft service as human editing. Drafts are
// sandboxed, so writes apply directly; nothing installs or touches viewers.
func ProjectTools(s *Studio) []tools.Tool {
	if s == nil {
		return nil
	}
	read := tools.Tool{Name: "studio_project_read", Title: "Read Studio draft", Description: "Read a saved Studio project and its source files, or list projects when id is omitted. Native handlers read saved non-secret values with synchronous ether.settings.get(); controls require a manifest ether.settings $ref and a settings.schema.json fields array. KV get returns string or null, put accepts a string, list accepts an optional prefix. See the starter files before changing contracts. Files larger than 6 KB are left out of the overview; read one with file (and offset for the next part).", ReadOnly: true, Audience: tools.AudienceOwner, Owner: "builtin", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"file":{"type":"string"},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`)}
	read.Execute = func(ctx context.Context, _ tools.Principal, input map[string]any) (tools.Result, error) {
		id, err := optString(input, "id", 160)
		if err != nil {
			return tools.Result{}, err
		}
		file, err := optString(input, "file", 200)
		if err != nil {
			return tools.Result{}, err
		}
		if id == "" && file != "" {
			return tools.Result{}, tools.Userf("Send the draft id together with file")
		}
		if id == "" {
			return tools.Result{Data: map[string]any{"projects": s.ListProjects(ctx)}}, nil
		}
		p, err := s.GetProject(ctx, id)
		if err != nil {
			return tools.Result{}, tools.Userf("Draft not found")
		}
		if file != "" {
			src, ok := p.Files[file]
			if !ok {
				return tools.Result{}, tools.Userf("The draft has no file %s", file)
			}
			offset, err := optInt(input, "offset", 0, 0, len(src))
			if err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Data: fileWindow(p, file, src, offset)}, nil
		}
		return tools.Result{Data: readableProject(p)}, nil
	}
	create := tools.Tool{Name: "studio_project_create", Title: "Create Studio draft", Description: "Create a saved plugin draft from template channels (Live TV), video_source (playable sources for catalogue titles), list (Home rows and collections), local_files, notification, ranking, tool, stremio_remix or page (console page; only when asked). Drafts are sandboxed and never installed, so this applies immediately. Read the saved source before editing it.", Audience: tools.AudienceOwner, Owner: "builtin", InputSchema: json.RawMessage(`{"type":"object","required":["template","name"],"properties":{"template":{"type":"string"},"name":{"type":"string"},"pluginId":{"type":"string"}},"additionalProperties":false}`)}
	create.Execute = func(ctx context.Context, _ tools.Principal, input map[string]any) (tools.Result, error) {
		template, err := reqString(input, "template", 64)
		if err != nil {
			return tools.Result{}, err
		}
		name, err := reqString(input, "name", 120)
		if err != nil {
			return tools.Result{}, err
		}
		id, err := optString(input, "pluginId", 120)
		if err != nil {
			return tools.Result{}, err
		}
		if _, ok := LookupTemplate(template); !ok {
			return tools.Result{}, tools.Userf("Unknown Studio template")
		}
		opts := CreateProjectOptions{Template: template, Name: name, PluginID: id}
		p, err := s.CreateProject(ctx, opts)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Data: map[string]any{"project": compactProject(p)}}, nil
	}
	save := tools.Tool{Name: "studio_project_save", Title: "Save Studio source", Description: "Save source files to an existing Studio draft and return real compiler diagnostics. Send whole files in files, or change part of a file with edits: an edit either replaces one exact, unique piece of text (find) with replace, or applies JSON Patch operations (patch: [{op: remove|replace|add, path: \"/catalogs/2\", value}]) to a JSON file such as manifest.json. Requires the exact updatedAt from studio_project_read; a newer human edit makes a stale save fail, so read again and retry. Applies immediately; does not install the plugin. \"$t:key\" text resolves in manifest fields and ViewSpec literals only, never in view data values. Fix every diagnostic, preview problem and installCheck error it returns before finishing; installed pages need $t: keys or { \"$\": \"field\" } data bindings for visible text, never literal strings.", Audience: tools.AudienceOwner, Owner: "builtin", InputSchema: json.RawMessage(`{"type":"object","required":["id","updatedAt"],"properties":{"id":{"type":"string"},"updatedAt":{"type":"string"},"files":{"type":"object","additionalProperties":{"type":"string"}},"edits":{"type":"array","maxItems":32,"items":{"type":"object","required":["file"],"properties":{"file":{"type":"string"},"find":{"type":"string"},"replace":{"type":"string"},"patch":{"type":"array","maxItems":64,"items":{"type":"object","required":["op","path"],"properties":{"op":{"type":"string","enum":["remove","replace","add"]},"path":{"type":"string"},"value":{}},"additionalProperties":false}}},"additionalProperties":false}}},"additionalProperties":false}`)}
	save.Execute = func(ctx context.Context, _ tools.Principal, input map[string]any) (tools.Result, error) {
		id, err := reqString(input, "id", 160)
		if err != nil {
			return tools.Result{}, err
		}
		version, err := reqString(input, "updatedAt", 64)
		if err != nil {
			return tools.Result{}, err
		}
		files := map[string]string{}
		if input["files"] != nil {
			raw, err := json.Marshal(input["files"])
			if err != nil || len(raw) > 256*1024 {
				return tools.Result{}, tools.Userf("Draft update exceeds 256 KiB")
			}
			if json.Unmarshal(raw, &files) != nil || len(files) > 32 {
				return tools.Result{}, tools.Userf("Provide at most 32 source files")
			}
		}
		if input["edits"] != nil {
			raw, _ := json.Marshal(input["edits"])
			var edits []fileEdit
			if json.Unmarshal(raw, &edits) != nil || len(edits) > 32 {
				return tools.Result{}, tools.Userf("Provide at most 32 edits")
			}
			cur, err := s.GetProject(ctx, id)
			if err != nil {
				return tools.Result{}, tools.Userf("Draft not found")
			}
			if err := applyEdits(cur.Files, files, edits); err != nil {
				return tools.Result{}, err
			}
		}
		if len(files) == 0 {
			return tools.Result{}, tools.Userf("Send files or edits")
		}
		p, comp, err := s.saveFilesVersion(ctx, id, files, version)
		if err != nil {
			return tools.Result{}, err
		}
		data := map[string]any{"project": compactProject(p), "compile": comp}
		if comp.OK && p.DefaultPage != "" {
			data["preview"] = renderedPreview(ctx, s, p)
		}
		// Run the real install checks too, so a draft that previews is also
		// one the owner can install.
		if comp.OK && s.mgr != nil {
			if _, err := s.PreviewInstallProject(ctx, p.ID); err != nil {
				data["installCheck"] = err.Error()
			} else {
				data["installCheck"] = "ok"
			}
		}
		return tools.Result{Data: data}, nil
	}
	return []tools.Tool{read, create, save}
}

// renderedPreview runs the saved page the way the owner sees it, so the
// model can catch output that compiles but renders wrong: literal
// "${...}" text from a quoted template, or "$t:" keys in view data.
func renderedPreview(ctx context.Context, s *Studio, p *Project) map[string]any {
	prev, err := s.PreviewProject(ctx, p.ID, p.DefaultPage, "", nil)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	out := map[string]any{"ok": prev.Valid && len(prev.Errors) == 0, "page": prev.Page}
	if len(prev.Errors) > 0 {
		out["errors"] = prev.Errors
	}
	var problems []string
	if strings.Contains(string(prev.Spec), "${") || strings.Contains(string(prev.Data), "${") {
		problems = append(problems, "Visible text contains a literal ${...}: build that string with a backtick template literal or concatenation.")
	}
	if strings.Contains(string(prev.Data), `"$t:`) {
		problems = append(problems, "View data contains a $t: key, which never resolves there: put plain text in data or move the key into a ViewSpec literal.")
	}
	if len(problems) > 0 {
		out["ok"] = false
		out["problems"] = problems
	}
	spec, data := string(prev.Spec), string(prev.Data)
	if len(spec) > 6<<10 {
		spec = spec[:6<<10] + "…"
	}
	if len(data) > 4<<10 {
		data = data[:4<<10] + "…"
	}
	out["spec"], out["data"] = spec, data
	return out
}

// compactProject is a draft without its source, for write results: the
// model already has the files it sent, and repeating them every step burns
// the turn's token budget. studio_project_read returns the full source.
func compactProject(p *Project) map[string]any {
	files := make([]string, 0, len(p.Files))
	for name := range p.Files {
		files = append(files, name)
	}
	sort.Strings(files)
	return map[string]any{
		"id": p.ID, "name": p.Name, "template": p.Template, "files": files,
		"updatedAt": p.UpdatedAt, "diagnostics": p.Diagnostics, "defaultPage": p.DefaultPage,
	}
}

// overviewFileLimit keeps a project overview inside one tool result.
const overviewFileLimit = 6 << 10

// fileWindowSize is how much of one file a read returns at a time.
const fileWindowSize = 12 << 10

// readableProject is a draft for the model: its version first, then each
// file inline, or its size when it is too large to send whole.
func readableProject(p *Project) map[string]any {
	files := map[string]string{}
	var large []string
	for name, src := range p.Files {
		if len(src) > overviewFileLimit {
			large = append(large, name)
			files[name] = fmt.Sprintf("[%d bytes: read it with file %q]", len(src), name)
			continue
		}
		files[name] = src
	}
	sort.Strings(large)
	out := map[string]any{
		"id": p.ID, "updatedAt": p.UpdatedAt, "name": p.Name, "template": p.Template,
		"pluginId": p.PluginID, "diagnostics": p.Diagnostics, "files": files,
	}
	if len(large) > 0 {
		out["largeFiles"] = large
	}
	if raw := p.Files["manifest.json"]; len(raw) > overviewFileLimit {
		out["manifestOutline"] = manifestOutline(raw)
	}
	return out
}

// manifestOutline names what a large manifest lists, with the JSON Patch
// path of each catalogue, so a model can change it without reading it all.
func manifestOutline(raw string) map[string]any {
	var doc struct {
		Types     []string          `json:"types"`
		Resources []json.RawMessage `json:"resources"`
		Catalogs  []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"catalogs"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil
	}
	cats := make([]string, 0, len(doc.Catalogs))
	for i, c := range doc.Catalogs {
		cats = append(cats, fmt.Sprintf("/catalogs/%d: %s/%s %q", i, c.Type, c.ID, c.Name))
	}
	res := make([]string, 0, len(doc.Resources))
	for _, r := range doc.Resources {
		var name string
		if json.Unmarshal(r, &name) != nil {
			var obj struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(r, &obj)
			name = obj.Name
		}
		res = append(res, name)
	}
	return map[string]any{"types": doc.Types, "resources": res, "catalogs": cats}
}

func fileWindow(p *Project, name, src string, offset int) map[string]any {
	end := min(len(src), offset+fileWindowSize)
	for end < len(src) && end > offset && src[end-1] != '\n' {
		end--
	}
	if end == offset {
		end = min(len(src), offset+fileWindowSize)
	}
	out := map[string]any{
		"id": p.ID, "updatedAt": p.UpdatedAt, "file": name, "size": len(src),
		"offset": offset, "text": src[offset:end],
	}
	if end < len(src) {
		out["nextOffset"] = end
	}
	return out
}

type fileEdit struct {
	File    string      `json:"file"`
	Find    string      `json:"find"`
	Replace string      `json:"replace"`
	Patch   []jsonPatch `json:"patch"`
}

// applyEdits replaces exact, unique text in the draft's files; files sent
// whole in the same save are edited after they are replaced.
func applyEdits(current, out map[string]string, edits []fileEdit) error {
	for _, e := range edits {
		src, ok := out[e.File]
		if !ok {
			src, ok = current[e.File]
		}
		if !ok {
			return tools.Userf("The draft has no file %s", e.File)
		}
		if len(e.Patch) > 0 {
			next, err := applyJSONPatch(src, e.Patch)
			if err != nil {
				return tools.Userf("%s: %v", e.File, err)
			}
			out[e.File] = next
			continue
		}
		if e.Find == "" {
			return tools.Userf("An edit to %s needs find text or a patch", e.File)
		}
		switch n := strings.Count(src, e.Find); n {
		case 0:
			return tools.Userf("The find text was not found in %s; read the file again", e.File)
		case 1:
		default:
			return tools.Userf("The find text appears %d times in %s; include more surrounding text", n, e.File)
		}
		out[e.File] = strings.Replace(src, e.Find, e.Replace, 1)
	}
	return nil
}

type jsonPatch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// applyJSONPatch applies remove, replace and add operations (RFC 6902) to
// a JSON document and returns it formatted like Studio's other files.
func applyJSONPatch(src string, ops []jsonPatch) (string, error) {
	var doc any
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		return "", fmt.Errorf("is not valid JSON: %w", err)
	}
	for _, op := range ops {
		var val any
		if op.Op != "remove" {
			if len(op.Value) == 0 {
				return "", fmt.Errorf("%s %s needs a value", op.Op, op.Path)
			}
			if err := json.Unmarshal(op.Value, &val); err != nil {
				return "", fmt.Errorf("%s %s: %w", op.Op, op.Path, err)
			}
		}
		next, err := patchAt(doc, pointerTokens(op.Path), op.Op, val)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", op.Op, op.Path, err)
		}
		doc = next
	}
	return prettyJSON(doc), nil
}

func pointerTokens(path string) []string {
	if path == "" || path == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts
}

func patchAt(node any, toks []string, op string, val any) (any, error) {
	if len(toks) == 0 {
		if op == "remove" {
			return nil, fmt.Errorf("cannot remove the whole document")
		}
		return val, nil
	}
	key, rest := toks[0], toks[1:]
	switch n := node.(type) {
	case map[string]any:
		if len(rest) == 0 {
			_, ok := n[key]
			switch {
			case op == "remove" && !ok, op == "replace" && !ok:
				return nil, fmt.Errorf("no member %q", key)
			case op == "remove":
				delete(n, key)
			default:
				n[key] = val
			}
			return n, nil
		}
		child, ok := n[key]
		if !ok {
			return nil, fmt.Errorf("no member %q", key)
		}
		next, err := patchAt(child, rest, op, val)
		if err != nil {
			return nil, err
		}
		n[key] = next
		return n, nil
	case []any:
		if len(rest) == 0 && op == "add" && key == "-" {
			return append(n, val), nil
		}
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i > len(n) || (i == len(n) && !(len(rest) == 0 && op == "add")) {
			return nil, fmt.Errorf("no index %s", key)
		}
		if len(rest) == 0 {
			switch op {
			case "remove":
				return append(n[:i:i], n[i+1:]...), nil
			case "replace":
				n[i] = val
				return n, nil
			default:
				out := append(n[:i:i], val)
				return append(out, n[i:]...), nil
			}
		}
		next, err := patchAt(n[i], rest, op, val)
		if err != nil {
			return nil, err
		}
		n[i] = next
		return n, nil
	}
	return nil, fmt.Errorf("%q is not inside an object or list", key)
}
