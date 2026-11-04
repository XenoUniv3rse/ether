# October After Dark

Publishes 17 Halloween films as a collection and Home row with an annual October-only schedule. The owner management page remains available year-round. Publishing creates or updates only this plugin’s own collection.

## Use

Created and verified through Studio in the 0.3.3 lab. These examples do not auto-install or change server configuration on upgrade.

From the repository root:

```sh
go run ./cmd/ether plugin check sdk/examples/october-after-dark
go run ./cmd/ether plugin pack -o /tmp/october-after-dark.etherpkg sdk/examples/october-after-dark
```

Install the package in Server → Plugins, or copy these three files into a Studio draft. Review permissions and configure the installed plugin in Setup. The source and English strings are editable in Studio.
