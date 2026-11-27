# Local folders

This hosted source scans the folders you grant to the plugin, matches movies and shows against Ether's catalogue, and returns local files as direct releases. Movie folders use `Title (Year)`; shows use `Show/Season 1/S01E02` filenames. Files remain in their original folders.

## Setup

Run `go run ./cmd/ether plugin check sdk/examples/local-folders` and `go run ./cmd/ether plugin pack -o /tmp/local-folders.etherpkg sdk/examples/local-folders`. Install the package in Server > Plugins and pick the folders to grant to the plugin. Open the Local folders plugin page and choose Scan folders. Use the title picker on unmatched folders to assign a catalogue title. The scheduled scan repeats hourly. Run offline tests with `node --test sdk/examples/local-folders/tests/plugin.test.ts` and `go test ./sdk/examples`.

Screenshot placeholder: the Local folders page with a scan button and an unmatched folder card containing a title search picker. A second screenshot shows the folder grants in the plugin drawer.

## Limits

Only files under the granted roots are indexed. The scanner recognizes MKV, MP4, M4V, AVI, MOV, TS and WebM, and series episodes named with `S01E02`. A folder whose name carries a provider ID tag, as Radarr, Sonarr, Jellyfin and Plex write them (`{tmdb-603}`, `[tmdbid-603]`, `{tvdb-81189}`, `[imdbid-tt0133093]`), is matched by that ID. Otherwise automatic matching requires the catalogue title, compared by letters and digits only (so `Mr. Robot` matches a folder named `Mr Robot` or `Mr.Robot`), and, when present, the year. The scanner follows the folder API cursor to read directories larger than one page. Scans are subject to Ether’s host-call and time budgets; very large trees can exceed them. Scans stop at ten directory levels. Changes are reflected after the next scan, and removing a manual match currently requires removing the plugin's stored data. Ether's existing player support determines which files can be played.
