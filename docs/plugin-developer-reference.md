# Plugin developer reference

The console reference lives at `/ether/plugins/reference/getting-started`.
The Plugins header, Studio header, and Code home all link to the same reference.
Every topic has a stable `/plugins/reference/:topic` URL. Search is local across
all guides and generated declarations, including collapsed source, with Cmd/Ctrl K
or `/` to focus and Escape to clear. Search queries are URL-backed (`?q=`).

`web/src/pages/plugins/reference-guides.json` contains the authored chapters,
service explanations, and copyable examples. Contract sections are generated:

```sh
go generate ./internal/plugins/devreference
go test ./internal/plugins/devreference -run TestReferenceMatchesContract
```

The drift test runs with the ordinary Go CI package gate; it compares the entire
checked-in generated JSON with current source. Regenerate after changing manifest
validation, schema, types, host services, SDKs, conformance kit, or examples.
The generator uses Go AST declarations, preserving comments/signatures/errors,
adds field tables with wire presence, and recursively flattens schema properties
with requiredness and constraints. Schema requiredness and Go wire presence are
kept distinct: omitempty does not prove a capability field is optional.
SDK and example source discovery includes new files automatically. New runtime
bindings or contract source files outside these roots must be added to the source
list in reference.go. The generated corpus is imported only by the lazy reference
route; opening Plugins or Studio does not download or index it.

Developer prose, chapter names, schema tables, API declarations and examples stay
in English to preserve exact API vocabulary and make snippets searchable against
the code. UI controls (search, results, copy, navigation controls) use the console
translation catalogue. No runtime translation or network search is performed.

Hosted SDK imports must be bundled into the entry artifact before packaging;
bare imports do not resolve inside the hosted virtual filesystem. Remote and
hosted APIs differ: folder/secret services are hosted only. Test paid accounts
through the owner's connection test after offline conformance.

Private network addresses are blocked by default even when a host is listed in
`permissions.network`, unless the owner allows that exact `host` or `host:port`
under "On your network" in the plugin's settings (loopback stays blocked unless
the owner explicitly grants `localhost` or `127.0.0.1`). Exact local names in
`permissions.network` (no dot, `.local`, `.lan`, `.home.arpa`, or a private IP)
are highlighted on the install permission sheet and pre-granted on install.

Large data services are generated from `host_data.go` and the TypeScript, Go, and
Python SDK clients: `runtime.limits`, `http.fetchToCache`, `cache.read`, `cache.stat`, `archive.list`, `archive.open`, `archive.read`, and `archive.extract`.
See `plugin-kinds.md` for the wire shapes, grant rules, archive safety bounds, and
why a 10 MiB direct fetch can still exceed a 32 MiB guest's available heap.

Bounded concurrent HTTP batching (`http.fetchMany`) is generated from
`host_fetch_many.go`, `contract.go`, `hosted.go`, `wasm/js.go`, `wasm/wasm.go`,
and the SDK clients. Hosted plugins can run up to 64 outbound requests in
parallel on the host (clamped to 8 in-flight per batch, 6 per origin across
Ether, 10 MiB total response bodies, 100 units per request) with per-item
allowlist, redirect credential confinement, timeout, egress accounting, and
prompt cancellation when the parent request ends.

The owner language model host service (`ai.complete`, `permissions.ai: "complete"`
or `"complete+search"`) is generated from `host_ai.go`, `contract.go`,
`remote/host.go`, and the SDK clients. It reuses the owner's configured language
model from Integrations, validates optional JSON Schemas on the host, applies
fixed per-plugin rate/size/timeout bounds, and hides provider credentials and
raw upstream errors from plugins. Viewer/client request paths remain refused
except opted-in interactive search routes (Stremio catalog search with a query,
kinds search, and declared frontend-adapter search routes) under
`permissions.ai: "complete+search"`, which run within Ether's 20 s search wait
bound with per-query caching and a separate per-viewer rate limit.

Agents can read `/ether/plugins/reference/llms.txt` for the entry guide and topic
index, or `llms-full.txt` for every authored chapter and generated declaration.
The Reference header's “Copy for your AI assistant” button fetches the full text
only when clicked. Both files are static Vite public assets, included in the
embedded console build; no server index or background work is involved.
`go generate ./internal/plugins/devreference` updates all three artifacts. The
same `TestReferenceMatchesContract` CI gate checks both text files for drift,
including changes to authored guides and `sdk/AGENTS.md`.
