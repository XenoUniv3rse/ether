# Movie Night

A native page that saves a rotating shortlist from actual catalogue title matches. Set title search words and a shortlist size in Setup, then use Pick the next shortlist. Also contributes a read-only Ask Ether tool. It does not check playback availability or perform semantic mood searches.

## Use

Created and verified through Studio in the 0.3.3 lab. These examples do not auto-install or change server configuration on upgrade.

From the repository root:

```sh
go run ./cmd/ether plugin check sdk/examples/movie-night
go run ./cmd/ether plugin pack -o /tmp/movie-night.etherpkg sdk/examples/movie-night
```

Install the package in Server → Plugins, or copy these three files into a Studio draft. Review permissions and configure the installed plugin in Setup. The source and English strings are editable in Studio.
