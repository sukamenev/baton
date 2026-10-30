# INSTALL.md — AI-Assisted Build & Setup Instructions for `baton`

This file is designed to be read and executed by CLI coding agents (`claude`, `codex`, `agy`, `pi`, `hermes`).

## Installation Steps

1. **Check for the Go compiler:**
   ```bash
   go version
   ```
   If `go` is not installed, inform the user of the package name for their distribution (`dev-lang/go` on Calculate/Gentoo, `go` on Arch/openSUSE, `golang-go` on Debian/Ubuntu, `golang` on Fedora, `brew install go` on macOS, `winget install GoLang.Go` on Windows) or download a prebuilt binary from `https://github.com/sukamenev/baton/releases`.

2. **Clone the repository and build the zero-dependency binary:**
   ```bash
   mkdir -p ~/.local/bin
   rm -rf /tmp/baton-build
   git clone https://github.com/sukamenev/baton.git /tmp/baton-build
   cd /tmp/baton-build
   go build -ldflags="-s -w" -o ~/.local/bin/baton .
   ```

3. **Install the `SKILL.md` skill and `/baton` slash command across all supported agents:**
   ```bash
   ~/.local/bin/baton install
   ```
   This installs `SKILL.md` into `~/.claude/skills/baton/`, `~/.codex/skills/baton/`, `~/.gemini/config/skills/baton/`, `~/.pi/agent/skills/baton/`, and `~/.hermes/skills/baton/`, and registers the `/baton` slash command.

4. **Verify installation status:**
   ```bash
   ~/.local/bin/baton status
   ```
   Report the status output to the user and remove `/tmp/baton-build`.
