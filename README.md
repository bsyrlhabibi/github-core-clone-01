# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/bsyrlhabibi/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/bsyrlhabibi/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge whose core is a single statically linked Go binary: git smart HTTP (protocol v2 and v0 receive-pack), the REST v3 API, a GraphQL v4 API and an MCP server — Go standard library only, no external dependencies, no git binary. A thin Node launcher (`main.ts`) starts the committed `forge-core` binary; the binary owns the whole lifecycle, from harness registration to shutdown.

## Run it

```sh
node main.ts
```

## Layout

- `main.ts` — Node launcher: copies `forge-core` to a temp file, marks it executable, spawns it.
- `forge-core` — the compiled forge (Go, `CGO_ENABLED=0`, statically linked).
- `*.go` — the complete forge source: object store and scenario seeding (`store.go`), git pack protocol (`gitpack.go`), REST (`rest.go`), GraphQL (`graphql.go`), MCP (`mcp.go`), HTTP wiring (`app.go`), lifecycle (`main.go`).

---

<sub>Entered on [make.fun](https://make.fun/?ref=hazel-beacon-85&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
