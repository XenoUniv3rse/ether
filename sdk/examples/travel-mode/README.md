# Travel Mode

A native ranking step with a 1, 2, 4 or 8 GB target. Add it near the top of a quality profile’s ranking order; the default target is 4 GB. Its scorecard uses the same rank_key function with example releases. It favours cached, device-compatible files at 1080p or below, within the selected size target.

## Use

Created and verified through Studio in the 0.3.3 lab. These examples do not auto-install or change server configuration on upgrade.

From the repository root:

```sh
go run ./cmd/ether plugin check sdk/examples/travel-mode
go run ./cmd/ether plugin pack -o /tmp/travel-mode.etherpkg sdk/examples/travel-mode
```

Install the package in Server → Plugins, or copy these three files into a Studio draft. Review permissions and configure the installed plugin in Setup. The source and English strings are editable in Studio.
