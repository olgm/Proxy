# Contributing

## Build and test

```sh
go build ./...
go vet ./...
gofmt -l .        # must print nothing
go test ./...
```

`go run ./cmd/proxyctl config` expands a topology file and changes nothing on
any node, which is the fast way to check a topology change compiles into what
you expect.

## Design notes

Read `agents/AGENTS.md` first. It lists the design docs, the shape of the
repo, and the rules a change must not break. The docs it points to (protocol
notes, the tunnel, the control plane) are the background for anything beyond
a small fix.

## Rules a PR must not break

- `proxyd` and `probed` stay stdlib-only. Check with:

  ```sh
  go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/proxyd ./cmd/probed
  ```

  Every line it prints must be inside this module
  (`github.com/olgm/proxy/...`). If it prints anything else, a change added a
  third-party dependency to one of these binaries; that is not allowed.

- Roles are emergent. A listener with a `minecraft` block is an ingress
  because of what it does, not because something labeled it one. Do not add a
  role field.

- Never add a pass-through mode for the status exchange. A status ping is
  answered on the ingress and never forwarded; there is no config for
  changing that, on purpose.

- One version for the whole repo, in `internal/version`. Do not add a
  per-binary version.

## Commit style

One topic per commit. Add a line under `## Unreleased` in `CHANGELOG.md` for
anything a user or operator would notice.

## Portability

Nothing in a deploy script may assume GNU coreutils. The fleet is not
uniform — some nodes run uutils coreutils, whose `install` cannot overwrite
an existing file from `/dev/stdin` the way GNU's does. Prefer forms that work
on both: remove a file before writing it rather than relying on overwrite.
