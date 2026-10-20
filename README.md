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
request and reply types, stream events). The extension's `src/wire/gen.ts`
and this folder's `WIRE.md` are generated from it, and both sides compare a
wire version at pairing:

```bash
cd src && go run ./cmd/wiregen -o ../../../branchkit-extension/src/wire/gen.ts
```

`just check-browser-wire` in the app repo regenerates and diffs; it is part of
`just push`. Change the protocol here, regenerate, and commit both repos.

## Writing your own extension

An extension of your own can pair with this plugin and speak the protocol in
`WIRE.md`. The browser lets an extension start the pairing host only if the
host's manifest names it, and `plugin.json` names only the BranchKit
extension, so any other one is connected by the person who installed it.

1. **Connect it.** In BranchKit's Settings, open Plugins → Browser and, under
   "Other browser extensions", choose the browser family, paste the
   extension's id and press Connect. Chromium browsers show the id on
   `chrome://extensions` (developer mode on); Firefox on `about:debugging`
   (the add-on's `browser_specific_settings.gecko.id`). BranchKit asks you to
   confirm in its own window, then writes the id into the browsers'
   native-messaging host manifests. Switching it off or removing it there
   takes the id out and restarts the plugin, which changes its token and
   ends any connection the extension holds; an id another plugin also names
   pairs with neither.
2. **Ask the pairing host.** Declare the `nativeMessaging` permission and send
   one message: `chrome.runtime.sendNativeMessage("com.branchkit.pairing",
   {v: 1}, reply => …)` (`browser.runtime.sendNativeMessage` in Firefox). The
   reply is `{"v":1,"port":N,"token":"…"}`, or `{"v":1,"error":"…"}` when
   BranchKit or the plugin is not running. An extension that is not
   connected never reaches it: the browser refuses the call
   (`runtime.lastError`). The port and token change every run: ask again
   after a refused request.
3. **Talk to the plugin.** Requests go to `http://127.0.0.1:<port>` with
   `Authorization: Bearer <token>`. Buy a ticket (`POST /events/ticket`, with
   your `wire_version`), then open the event stream
   (`GET /events?ticket=…&conn_id=…`). `conn_id` is a nonce your extension
   picks for this connection and sends in every body that has one.

`WIRE.md` lists every route, event and type, with the current `WIRE_VERSION`;
it is generated from `src/wire/` (`cd src && go run ./cmd/wiregen -md -o
../WIRE.md`) and a test fails when it falls behind. Each change to the
protocol changes the version, and the ticket refuses an extension built
against another one.

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
