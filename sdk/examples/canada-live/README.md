# Canada Live

Adds CPAC with English/French audio and five public Global News feeds to Live TV. The broadcasters control availability and URLs. These news and public-affairs feeds do not provide a programme guide through this plugin.

## Use

Created and verified through Studio in the 0.3.3 lab. These examples do not auto-install or change server configuration on upgrade.

From the repository root:

```sh
go run ./cmd/ether plugin check sdk/examples/canada-live
go run ./cmd/ether plugin pack -o /tmp/canada-live.etherpkg sdk/examples/canada-live
```

Install the package in Server → Plugins, or copy these three files into a Studio draft. Review permissions and configure the installed plugin in Setup. The source and English strings are editable in Studio.
