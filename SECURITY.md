# Security

Report security issues through GitHub's private vulnerability reporting for
this repository. Do not open a public issue containing exploit details,
credentials, private logs, or sensitive review material.

You should receive an initial response within three business days. Fixes
target the latest release.

## Trust Model

roast is local software that drives your own review CLI. At runtime it reads
local Git metadata, diffs, and tracked files. The review snapshot is built
from the reviewed commit's tree and blob objects, so ignored files and archive
attributes cannot inject or hide content; dirty-mode reviews snapshot the
working tree because that is the change under review.

Review content does leave your machine — that is the product. The diff, the
tracked-file snapshot, and any project documents selected as context are
handed to the Codex or Claude CLI you chose, which sends them to its model
provider under your existing account. roast adds no network calls, telemetry,
or credential handling of its own; engine CLIs run with your login, with
project configuration, hooks, MCP servers, and skills disabled, and with read
access scoped to the snapshot.

Before the review engine runs, TruffleHog scans the complete pre- and
post-change content. Its verification step may contact credential providers'
endpoints with detected candidates to confirm they are live — detection is
local, verification is not. roast fails closed on any verified secret.
Environment files and credential stores are excluded from review bundles
entirely; a change that touches one stops the review. Every failure path in
the pipeline fails closed — a missing review can never approve a change.

`install.sh` is the installer's only network touchpoint: it downloads release
archives and checksums from GitHub (or an explicit mirror), verifies SHA-256,
and installs one binary without sudo. The skill installer writes only under
existing Claude Code and Codex skill directories.

Reports are most useful when they include the affected version, concrete
impact, and a minimal reproduction. Scanner output without an impact path is
less useful, but uncertain reports are still welcome through the private
channel.
