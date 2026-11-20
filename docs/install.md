# Installing repath

repath is one binary over SQLite. Building it needs Go 1.27+ **and a C
toolchain**: tree-sitter runs through cgo (PLAN §3 decision 15), so there is no
pure-Go build. Linux amd64 and arm64 are the release targets.

The first tagged release is **v1.3.0** (2026-11-20): Linux amd64/arm64
archives, SHA-256 checksums, and a source archive on the
[GitHub releases page](https://github.com/Goutam-11/repath/releases/tag/v1.3.0).
`repath --version` prints the tag (`v1.3.0`).

## From a release

Download the archive and checksums from the [GitHub releases page](https://github.com/Goutam-11/repath/releases):

```sh
version=1.3.0    # release version WITHOUT the leading v (GoReleaser .Version)
tag="v${version}"
arch=amd64       # amd64 or arm64
base="https://github.com/Goutam-11/repath/releases/download/${tag}"

curl -LO "${base}/repath_${version}_linux_${arch}.tar.gz"
curl -LO "${base}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
tar -xzf "repath_${version}_linux_${arch}.tar.gz"
sudo install -m 0755 repath /usr/local/bin/repath
repath --version          # v1.3.0
```

`checksums.txt` is SHA-256 over every archive and the source archive. The binary
is dynamically linked (cgo); a minimal container needs glibc.

## From source

The release archives are the supported install path; building from source from
a checkout (or a future `v*`-tagged commit) works the same way.

```sh
git clone https://github.com/Goutam-11/repath
cd repath
make build                    # bin/repath
./bin/repath --version
```

`make build` injects the version from `git describe --tags --always --dirty`
through the existing `-X .../internal/cli.version` ldflag. A plain
`go build ./cmd/repath` works too, but reports `dev` unless you pass the ldflag.

Requirements:

- Go 1.27+ (see `go.mod`);
- a C compiler and libc headers (`gcc`, `clang`, or a cross toolchain);
- on Debian/Ubuntu: `build-essential`.

## Cross-compiling

`GOOS=linux GOARCH=arm64 go build` alone is **not** enough: cgo needs an arm64 C
toolchain. On an amd64 Debian/Ubuntu host:

```sh
sudo apt-get install gcc-aarch64-linux-gnu
CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
  go build -trimpath -o repath-arm64 ./cmd/repath
```

The release workflow does the same with GoReleaser; the arm64 override is in
`.goreleaser.yaml`. There is no static pure-Go build to fall back to.

## Install the agent skill

repath ships one [Agent Skill](https://agentskills.io) at
[`skills/repath/SKILL.md`](../skills/repath/SKILL.md), embedded into the binary.
`repath install` copies it into the user-scope skill directory of one agent, or
all of them; no repo checkout, network, or config file is involved:

```sh
repath install --platform claude
repath install --all
repath install --all --force        # replace a file you edited
```

| `--platform` | Writes | Read by |
| --- | --- | --- |
| `claude` | `~/.claude/skills/repath/SKILL.md` | Claude Code personal skills |
| `codex` | `~/.agents/skills/repath/SKILL.md` | Codex user skills (`$HOME/.agents/skills`), also the cross-agent location |
| `cursor` | `~/.cursor/skills/repath/SKILL.md` | Cursor user skills |
| `gemini` | `~/.gemini/skills/repath/SKILL.md` | Gemini CLI user skills |
| `vscode` | `~/.copilot/skills/repath/SKILL.md` | GitHub Copilot user skills |

The command defaults to `claude`. A second run with the same content is a no-op;
an existing file with different content is reported and left alone unless
`--force` is passed. `repath uninstall` (same `--platform`/`--all` flags) removes
only `.../repath/SKILL.md` and the now-empty `repath/` directory; a missing file
is success. `repath uninstall --purge` additionally deletes `.repath/` under
`--root` (default: the current repo). It is never implied.

Paths follow each vendor's documented user-scope skill directory (checked
2026-11-20). An unknown platform is a usage error (exit `2`) that prints the
supported list. The tests run against a temporary `HOME` and never touch a real
agent directory; `--json` on either command returns the exact paths and actions.

## Runtime

```sh
cd /path/to/repo
repath status --json              # builds .repath/index.db on first use
repath impact <symbol>            # a structural answer
```

- One SQLite file lives at `<repo>/.repath/index.db`; add `.repath/` to your own
  `.gitignore`.
- No daemon, no LLM, no network calls. `repath serve` is an optional
  **read-only** HTTP surface; it binds loopback by default and has no auth.
- Artifacts (`index --artifact`, `pull`) are opt-in files, not a service.
- Languages: TypeScript/JavaScript, Python, Rust.

## Release engineering

- `.goreleaser.yaml` builds Linux amd64/arm64, writes `checksums.txt` and a
  source archive, and injects `internal/cli.version`.
- `.github/workflows/release.yml` runs only on a `v*` tag push. It installs the
  arm64 C toolchain, runs GoReleaser, and creates a **draft** GitHub release; a
  human publishes it.
- Local checks:

  ```sh
  goreleaser check
  make release-snapshot          # current platform, dist/repath_linux_amd64_v1/repath
  ./dist/repath_linux_amd64_v1/repath --version
  ```

  A full multi-arch snapshot needs the arm64 cross compiler; `make
  release-snapshot` intentionally builds only the current target so it works on
  a bare developer machine.
- No tag or release is created by a normal PR. Tagging is an explicit,
  separate decision.
