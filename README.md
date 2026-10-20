# Browser plugin

The BranchKit side of browser control. It pairs with the BranchKit Browser
extension (`branchkit-extension` in the app workspace), turns what the
extension reports about the page (hint badges, tabs, find, caret, media) into
collections and commands the platform matcher can use, and routes the matched
actions back to the extension. An ordinary Go plugin (`id: browser`) under the
same sandbox and grants as any other.

Developed in the BranchKit app workspace at `plugins/browser/`.

## How the pieces connect

1. The plugin declares one listen socket (`requires.sockets.listen`, id
   `extension`). The platform binds it and hands it over, so the plugin works
   inside its sandbox on every OS; `network` is `localhost`.
2. The extension finds the plugin through the native messaging host
   `com.branchkit.pairing` (`provides.native_messaging`). The host binary is
   built from the app repo's `native-host/` crate and returns the plugin's
   port and a per-run pairing token.
3. Everything after pairing is HTTP on that socket behind the token: requests
   from the extension, plus an event stream opened with a one-time ticket so
   the token never appears in a URL (`bridge.go`).
4. Commands come from several sources, each pushed as its own group with
   `commands.push` (`commands_register.go`): hints, references, tab-to-desk,
   escape, and the scroll, find and navigation phrases the extension
   contributes on connect (`contributed`, received by `contribute.go`).

When several browsers or profiles are connected, only the one the OS reports in
front may write the live hint grammar (`PluginState` in `main.go`, the batch
handler in `batch.go`).

## The wire protocol

The extension protocol is declared once, in Go, in `src/wire/` (routes,
request and reply types, stream events). The extension's `src/wire/gen.ts` is
generated from it, and both sides compare a wire version at pairing:

```bash
cd src && go run ./cmd/wiregen -o ../../../branchkit-extension/src/wire/gen.ts
```

`just check-browser-wire` in the app repo regenerates and diffs; it is part of
`just push`. Change the protocol here, regenerate, and commit both repos.

## What it declares

- Action types: `click`, `set_value`, `hide_hints`, `history_back`,
  `history_forward`, `refresh`, `tab_to_desk`, `tab_to_window`, `noop`.
- Collections: `plugin.browser.*` (hints and per-prefix hint sets, tabs,
  caret, find, query field, media, video mode, auto-scroll, palette rows,
  references, config).
- A `palette` selection surface and the `hint_pair` capture macro used to
  speak two-letter hints.
- Settings tabs: Hints, and Backup (a read-only view of the copy of the
  extension's settings the plugin keeps for a reinstall).

`plugin.json` is the source of truth; `src/actions_gen.go` and
`src/collections_gen.go` are generated from it (`just gen-plugins` in the app
repo).

## Build and test

From the app workspace root:

```bash
just rebuild browser               # rebuild and restart in the running app
cd plugins && go test ./browser/src/...
just check-browser-wire            # wire file in sync with the extension
```

The build recipe is `dev.build` in `plugin.json`: `templ generate`, then
`go build` into `../browser-plugin`. Logs: `branchkit-cli dev plog browser`
(extension log lines land in the same file).

## License

MIT. See [LICENSE](LICENSE).
