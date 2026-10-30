package main

import _ "embed"

// Files installed into every supported agent by `baton install` / `baton update`.
// They are embedded into the binary at compile time, so the repository files below
// are the single source of truth.

//go:embed skills/baton/SKILL.md
var skillMarkdownContent string

//go:embed commands/baton.md
var slashCommandContent string
