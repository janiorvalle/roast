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
provider under your existing account. During reviews, roast adds no telemetry
or network calls beyond those named here. For Codex, roast reads `auth.json`
and stages a refresh-token-free copy in a temporary home; when the cached access
token is expiring, Codex's native `account/read` flow may refresh the source
login before staging. Engine CLIs otherwise run with your login, with project
configuration, hooks, MCP servers, and skills disabled, and with read access
scoped to the snapshot.

Before the review engine runs, TruffleHog scans the complete pre- and
post-change content. Its verification step may contact credential providers'
endpoints with detected candidates to confirm they are live — detection is
local, verification is not. roast fails closed on any verified secret.
Environment files and credential stores are excluded from review bundles
entirely; a change that touches one stops the review. Every failure path in
the pipeline fails closed — a missing review can never approve a change.

Release installation has two explicit network paths. `install.sh` downloads a
release archive and `checksums.txt` from GitHub or an explicit mirror. `roast
upgrade` queries `api.github.com/repos/janiorvalle/roast/releases/latest`, then
downloads the named archive and `checksums.txt` from that release's GitHub asset
URLs. Both paths verify the archive's SHA-256 before replacing the binary, and
neither uses sudo. The skill installer writes only under existing Claude Code
and Codex skill directories.

Reports are most useful when they include the affected version, concrete
impact, and a minimal reproduction. Scanner output without an impact path is
less useful, but uncertain reports are still welcome through the private
channel.
