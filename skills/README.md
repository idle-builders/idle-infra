# Published infra skills

This directory is the plugin's explicit publication allowlist and the canonical
home of every consumer-facing skill. The matching entries under
`.claude/skills/` are symlinks back here so local repository discovery and the
plugin do not maintain two copies.

Generic workflow skills such as `concept`, `architecture`, `review`, and
`ship` belong to this repository's local agent pipeline and must not be added
here.
