# toolkit

Shared Go libraries for the trick77 apps that are not base plumbing: the pieces through which an app talks to
the outside world, one package each. Base functionality (HTTP server, auth, SQLite, logging, SSE) lives in
[ship's kit](https://github.com/trick77/ship/tree/master/kit); a library only one app needs stays in that app.

```sh
go get github.com/trick77/toolkit@latest
```

One module, one version line. v0.x: a minor release may break callers, its release note says how.

## Packages

| Package | What |
|---|---|
| [`applemusic`](applemusic) | Apple Music API the way the music.apple.com web player reads it: the player's developer token (scraped from its bundle), the user's media-user-token for library calls. Catalog (songs by ISRC, search, album tracks) and the user's library, history and recommendations. Read-only, unofficial. |
| [`llmwire`](llmwire) | OpenAI-compatible inference (and Claude over Anthropic's protocol): chat, tools, vision, embeddings, each model's quirks kept as data in `profiles.yaml`. See [its README](llmwire/README.md). Was `github.com/trick77/llmwire`. |
| [`mcpserver`](mcpserver) | Stateless [MCP](https://modelcontextprotocol.io) server over streamable HTTP, JSON responses only. Answers the 2026-07-28 revision and the initialize-era ones (2025-11-25, 2025-06-18, 2025-03-26). |
| [`webfetch`](webfetch) | In-process Go port of the reference `mcp-server-fetch`: fetch a URL, extract the main content as Markdown, honour robots.txt, SSRF-guarded. See [its README](webfetch/README.md). Was `github.com/trick77/webfetch`. |

## mcpserver

Describe the endpoint and its tools in a `Server`, mount the handler. The protocol, the caps and the result shape
are the package's; the tools are yours.

```go
import "github.com/trick77/toolkit/mcpserver"

h := mcpserver.New(mcpserver.Server{
	Name:    "example",
	Version: version,
	Instructions: "Echoes text back.",
	Tools: []mcpserver.Tool{{
		Name: "echo", Title: "Echo", Description: "Returns the text it was given.", ReadOnly: true,
		Schema: mcpserver.ObjectSchema(map[string]any{"text": map[string]any{"type": "string"}}, "text"),
		Run: func(ctx context.Context, args json.RawMessage) mcpserver.Result {
			var a struct {
				Text string `json:"text"`
			}
			if err := mcpserver.DecodeArgs(args, &a); err != nil {
				return mcpserver.ErrorResult(err.Error()) // the model reads it and explains
			}
			return mcpserver.Result{Text: a.Text}
		},
	}},
})
mux.Handle("/mcp", h)
```

What `Server` covers beyond the tools:

- **Per-request text**: `InstructionsFunc` and a tool's `DescriptionFunc` are built per request, e.g. a hint naming
  the user's own data; `ToolsTTL` tells clients how long to cache the list (default 1 h).
- **Origin and auth**: a request with an `Origin` header is refused unless it matches `AllowedOrigin`; `Auth` wraps
  the handler after that check, so a foreign page learns nothing about the token.
- **Caller identity**: `UserHeader` names a trusted header whose value tools read with `mcpserver.User(ctx)`.
- **Result shape**: text capped at a line (`MaxTextBytes`, default 24 KiB), citable `Sources` in
  `structuredContent`, `Files` as embedded resources within a 3 MiB budget.
- **Retired names**: `Aliases` keep old tool names answering without listing them.

Not covered: sessions, the GET stream, server-initiated requests, resources and prompts. Tools only, stateless.

## Development

The gate is [ship](https://github.com/trick77/ship): `ship check` locally, and the pre-push hook runs it on every
push. MIT licensed.
