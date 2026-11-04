# AGENTS.md

- What lives here: shared libraries that are not base plumbing (integrations with the outside: mcpserver, llmwire,
  webfetch; Apple Music once reworm's and loom's copies drift). Base functionality goes to ship's kit; a library for
  one app stays in that app. User: kit "is more about common base functionality"; a repo per library "is totally
  overblown". llmwire and webfetch were own repos (archived after the move); their rules are their dir's AGENTS.md.
- One module, one version line, a package per library. No nested go.mod: every new module costs release wiring.
  A library's dependencies land in every consumer's module graph (go.sum lines, not builds): keep them few, pure Go.
- Consumers are the trick77 apps; a v0.x minor may break them. Breaking change = minor, say so in the release note.
- mcpserver: protocol only. App concepts stay in the app (loom's X-Loom-User is loom's `internal/loomuser`, named via
  `Server.UserHeader`; peeq's channel hint rides `InstructionsFunc`/`DescriptionFunc`).

## Git & CI

- Gate: `ship check` (`ship` is the `ship()` from ship's README).
- Fresh clone: `ship hooks` before the first push (check, conform and init install it too); without the pre-push hook a push skips the gate.
- Branch + PR, never push to `master`.
- Release by hand (`gh workflow run Release -f bump=auto`), except a merged Dependabot go.mod/go.sum bump: that one
  releases itself (release.yaml, a ship.yaml `scaffold` allow). User: dependabot "auto-releases", the rest stays manual.
  A Dependabot release also ships whatever was merged since the last tag, at the bump its `Release:` lines ask for.
- Commit subjects + PR titles: Go style `area: what` (`mcpserver: …`, `llmwire: …`, `webfetch: …`), gated by ship; release notes list them by area.
- `.yaml`, never `.yml`.
