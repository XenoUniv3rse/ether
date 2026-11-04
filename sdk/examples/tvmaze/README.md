# TVmaze Native Catalog

Independent TVmaze show metadata, an acclaimed-series list and a Canadian-airing list. The page publishes a 30-show collection by default. Data is attributed to TVmaze under CC BY-SA; Ether matches external IDs to its canonical catalogue.

## Use

Created and verified through Studio in the 0.3.3 lab. These examples do not auto-install or change server configuration on upgrade.

From the repository root:

```sh
go run ./cmd/ether plugin check sdk/examples/tvmaze
go run ./cmd/ether plugin pack -o /tmp/tvmaze.etherpkg sdk/examples/tvmaze
```

Install the package in Server → Plugins, or copy these three files into a Studio draft. Review permissions and configure the installed plugin in Setup. The source and English strings are editable in Studio.
