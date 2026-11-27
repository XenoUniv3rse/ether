package studio

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaType follows the JSON contract, including anonymous presentation structs.
// Keeping this in the test makes new Go fields fail the published-schema gate.
func schemaType(typ reflect.Type) map[string]any {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}
	}
	if typ == reflect.TypeFor[plugins.KindLibraryShelfFilters]() {
		ruleSchema := schemaType(reflect.TypeFor[core.CollectionRule]())
		return map[string]any{
			"anyOf": []any{
				map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				schemaType(reflect.TypeFor[core.TabFilter]()),
				ruleSchema,
				map[string]any{"type": "array", "items": ruleSchema},
			},
		}
	}
	switch typ.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			props[name] = schemaType(f.Type)
		}
		switch typ {
		case reflect.TypeFor[plugins.MediaKindMeta]():
			props["media"] = map[string]any{"enum": []string{"video", "music", "audiobook", "book", "podcast", "photo"}}
		case reflect.TypeFor[plugins.KindLevel]():
			props["media"] = map[string]any{"enum": []string{"video", "music", "audiobook", "book", "podcast", "photo"}}
		case reflect.TypeFor[plugins.KindField]():
			props["type"] = map[string]any{"enum": []string{"string", "int", "float", "bool", "date", "duration", "enum", "strings", "people"}}
		case reflect.TypeFor[plugins.KindLibraryDecl]():
			props["card"] = map[string]any{"enum": []string{"poster", "landscape", "square"}}
			props["dateDisplay"] = map[string]any{"enum": []string{"year", "date"}}
			props["experience"] = map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9-]{0,31}$"}
		case reflect.TypeFor[plugins.KindLibraryTab]():
			props["kind"] = map[string]any{"enum": []string{"recommended", "library", "collections", "people", "view", "live"}}
			props["sort"] = map[string]any{"enum": []string{"title", "added", "released", "year", "rating", "index", "lastViewed"}}
			props["direction"] = map[string]any{"enum": []string{"asc", "desc"}}
			props["order"] = map[string]any{"enum": []string{"count", "name"}}
			props["card"] = map[string]any{"enum": []string{"poster", "landscape", "square"}}
			props["layout"] = map[string]any{"enum": []string{"grid", "list", "timeline"}}
		case reflect.TypeFor[plugins.KindLibraryShelf]():
			props["sort"] = map[string]any{"enum": []string{"title", "added", "released", "year", "rating", "index", "lastViewed"}}
			props["direction"] = map[string]any{"enum": []string{"asc", "desc"}}
		case reflect.TypeFor[plugins.KindSyncDecl]():
			props["undo"] = map[string]any{"enum": []string{"snapshots", "ids"}}
		case reflect.TypeFor[plugins.FrontendUploadPath]():
			props["path"] = map[string]any{"type": "string", "pattern": "^/", "maxLength": 256}
			props["maxBytes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 536870912}
			return map[string]any{"type": "object", "properties": props, "required": []string{"path", "maxBytes"}, "additionalProperties": false}
		case reflect.TypeFor[plugins.FrontendAdapterMeta]():
			if up, ok := props["uploadPaths"].(map[string]any); ok {
				up["description"] = "Adapter subpath globs that accept uploads streamed to disk. The smallest matching maxBytes applies to the entire wire body."
				up["maxItems"] = 16
			}
		}
		return map[string]any{"type": "object", "properties": props}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaType(typ.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaType(typ.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "integer"}
	}
}

func TestPublishedManifestSchema(t *testing.T) {
	const path = "../../../schemas/plugin-manifest.v1.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ether := doc["properties"].(map[string]any)["ether"].(map[string]any)["properties"].(map[string]any)
	// Extract every typed kind/capability constant from the authoritative contract.
	file, err := parser.ParseFile(token.NewFileSet(), "../contract.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	enums := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		typ, ok := spec.Type.(*ast.Ident)
		if !ok || (typ.Name != "Kind" && typ.Name != "Capability") {
			return true
		}
		for _, v := range spec.Values {
			if lit, ok := v.(*ast.BasicLit); ok {
				value, e := strconv.Unquote(lit.Value)
				if e == nil {
					enums[typ.Name] = append(enums[typ.Name], value)
				}
			}
		}
		return true
	})
	expected := map[string]any{}
	for key, value := range ether {
		expected[key] = value
	}
	expected["kind"] = map[string]any{"enum": enums["Kind"]}
	expected["capabilities"] = map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"enum": enums["Capability"]}}
	perms := ether["permissions"].(map[string]any)["properties"].(map[string]any)
	wantAI := map[string]any{
		"description": "Use the owner's configured language model for bounded completions in actions, jobs, sync, and other non-viewer routes (\"complete\"), or also on interactive search routes — Stremio catalog search, kinds search, and frontend adapter search routes — with a hard 20 s bound and per-query caching (\"complete+search\"). Every other viewer/client path stays refused.",
		"enum":        []string{"none", "complete", "complete+search"},
	}
	wantMedia := map[string]any{
		"description": "Probe container metadata ({durationSec, container, width, height, sizeBytes}) for a PlayableSource via media.probe from jobs, actions, background sync, and other non-viewer routes (\"probe\"). Reuses Ether's durable release_probes cache, is rate-limited per plugin, and is bounded at 10 s per probe. Every viewer or client request path is refused.",
		"enum":        []string{"none", "probe"},
	}
	wantSources := map[string]any{
		"description": "Ask Ether to search indexers and inspect or acquire debrid/Usenet releases via sources.resolve (\"search\", \"search+check\", \"acquire\", or \"acquire+check\"). The \"+check\" variants also permit background usenet_check calls in jobs and schedules within the owner's per-plugin daily allowance. Requires the kind_resolver capability.",
		"enum":        []string{"none", "search", "search+check", "acquire", "acquire+check"},
	}
	contrib := ether["contributes"].(map[string]any)["properties"].(map[string]any)
	kind := schemaType(reflect.TypeFor[plugins.MediaKindMeta]())
	adapter := schemaType(reflect.TypeFor[plugins.FrontendAdapterMeta]())
	liveFeed := schemaType(reflect.TypeFor[plugins.LiveFeedMeta]())
	screen := schemaType(reflect.TypeFor[plugins.ScreenMeta]())
	wantKinds := map[string]any{"type": "array", "items": kind}
	wantAdapters := map[string]any{"type": "array", "items": adapter}
	wantLiveFeeds := map[string]any{"type": "array", "items": liveFeed}
	wantScreens := map[string]any{"type": "array", "items": screen}
	equal := func(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return string(x) == string(y) }
	changed := !equal(ether["kind"], expected["kind"]) || !equal(ether["capabilities"], expected["capabilities"]) || !equal(perms["ai"], wantAI) || !equal(perms["media"], wantMedia) || !equal(perms["sources"], wantSources) || !equal(contrib["mediaKinds"], wantKinds) || !equal(contrib["frontendAdapters"], wantAdapters) || !equal(contrib["liveFeeds"], wantLiveFeeds) || !equal(contrib["screens"], wantScreens)
	if changed {
		if os.Getenv("UPDATE_MANIFEST_SCHEMA") != "1" {
			t.Fatal("manifest schema drift: run UPDATE_MANIFEST_SCHEMA=1 go test ./internal/plugins/studio -run TestPublishedManifestSchema")
		}
		ether["kind"] = expected["kind"]
		ether["capabilities"] = expected["capabilities"]
		perms["ai"] = wantAI
		perms["media"] = wantMedia
		perms["sources"] = wantSources
		contrib["mediaKinds"] = wantKinds
		contrib["frontendAdapters"] = wantAdapters
		contrib["liveFeeds"] = wantLiveFeeds
		contrib["screens"] = wantScreens
		raw, err = json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	normalized, _ := json.Marshal(doc)
	json.Unmarshal(normalized, &doc)
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource("manifest.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tpl := range Templates() {
		t.Run(tpl.ID, func(t *testing.T) {
			var manifest any
			if err := json.Unmarshal([]byte(tpl.Files["manifest.json"]), &manifest); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(manifest); err != nil {
				t.Fatal(err)
			}
		})
	}
}
