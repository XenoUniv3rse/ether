# EasyDebrid

A native debrid provider using the debrid_provider capability. Add a bearer API token in Setup and run Test. Credentials stay in the secret vault. It checks cached torrents and generates playback links; it does not queue or remove downloads. Paid playback needs an active account and was not tested in the lab.

## Use

Created and verified through Studio in the 0.3.3 lab. These examples do not auto-install or change server configuration on upgrade.

From the repository root:

```sh
go run ./cmd/ether plugin check sdk/examples/easydebrid
go run ./cmd/ether plugin pack -o /tmp/easydebrid.etherpkg sdk/examples/easydebrid
```

Install the package in Server → Plugins, or copy these three files into a Studio draft. Review permissions and configure the installed plugin in Setup. The source and English strings are editable in Studio.
