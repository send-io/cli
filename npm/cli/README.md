# @sendio/cli

The official [send.io](https://send.io) command-line interface, packaged for npm.

```sh
npm install -g @sendio/cli
sendio login
sendio emails send --from "Acme <hi@mail.example.com>" --to you@example.com \
  --subject "Hello" --text "It works"
```

Or run it without installing:

```sh
npx @sendio/cli --help
```

This package is a small Node shim. The native `sendio` binary comes from a
platform package that npm installs automatically as an optional dependency:

| Platform            | Package                      |
| ------------------- | ---------------------------- |
| macOS Apple silicon | `@sendio/cli-darwin-arm64`   |
| macOS Intel         | `@sendio/cli-darwin-x64`     |
| Linux x64           | `@sendio/cli-linux-x64`      |
| Linux arm64         | `@sendio/cli-linux-arm64`    |
| Windows x64         | `@sendio/cli-win32-x64`      |
| Windows arm64       | `@sendio/cli-win32-arm64`    |

If you install with `--omit=optional` / `--no-optional`, the binary is not
installed and `sendio` will tell you so. Set `SENDIO_BINARY_PATH` to point the
shim at a binary you installed some other way.

Looking for the Node.js SDK? That is the `sendio` package.

Other ways to install: `curl -fsSL https://app.send.io/install.sh | sh`,
Homebrew (`brew install --cask send-io/tap/sendio`), Scoop, `.deb`/`.rpm`, or the
archives on the GitHub releases page.
