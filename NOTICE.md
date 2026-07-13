# Notice, attribution & disclaimers

## What this is

This repository is an **unofficial, personal fork** of the SFTPGo project
([drakkan/sftpgo](https://github.com/drakkan/sftpgo), copyright © Nicola Murino
and the SFTPGo contributors). It exists to add three additive, backward-compatible
features to the end-user WebClient file browser — drag-and-drop move, recursive
search, and photo/video thumbnails with a grid view. See
[FORK_FEATURES.md](FORK_FEATURES.md).

It is **not affiliated with, endorsed by, sponsored by, or supported by** the
SFTPGo project, its maintainer, or SFTPGo S.r.l. For official releases, security
updates, commercial support, and the Enterprise edition, please use the upstream
project and <https://sftpgo.com>.

## License

SFTPGo is licensed under the **GNU Affero General Public License v3.0 only
(AGPL-3.0-only)**, and this fork is distributed under the **same license**. The
full text is in [LICENSE](LICENSE). All original copyright notices and license
headers are preserved unchanged.

The modifications and additions in this fork (see the git history and
`FORK_FEATURES.md`) are contributed under the same AGPL-3.0-only license.

Under the AGPL, if you run a modified version of this software and let users
interact with it over a network, you must offer those users access to the
corresponding source code. Publishing this repository satisfies that requirement
for this fork; if you deploy your own modified build, keep your source available
to your users as well.

## Third-party components

- **Web interface templates (KeenThemes).** The WebAdmin and WebClient user
  interfaces use Bootstrap templates from [KeenThemes](https://keenthemes.com/),
  provided to the SFTPGo project under a **custom license granted specifically to
  SFTPGo** (see the notice at the top of `templates/common/base.html`). That
  license permits use of the templates **only within the SFTPGo product** and
  does **not** grant rights to reuse them in other/derivative products. This fork
  is SFTPGo (it does not rebrand or repackage the UI as a different product) and
  changes only content within those SFTPGo templates; it does **not** relicense
  them. Anyone using or further distributing this fork must comply with **both**
  the AGPL-3.0 and the KeenThemes terms, and must not extract the KeenThemes
  templates for use outside SFTPGo.
- **Go module dependencies** retain their respective upstream licenses (see
  `go.mod` / `go.sum` and each module's repository). The thumbnail feature adds a
  direct dependency on `golang.org/x/image` (BSD-3-Clause).
- **ffmpeg** (optional, used at runtime for video thumbnails) is a separate
  program that is *invoked*, not linked or redistributed by this project. If you
  ship an image that bundles ffmpeg, comply with ffmpeg's own licensing
  (LGPL/GPL depending on build).

## Trademarks

"SFTPGo" and related names, logos, and marks are trademarks of their respective
owner. Use of these names in this repository is **nominative** — to identify the
upstream project this is forked from — and does **not** imply any affiliation or
endorsement.

## No warranty

This software is provided **"as is", without warranty of any kind**, express or
implied, to the maximum extent permitted by law, as set out in sections 15 and 16
of the GNU AGPL-3.0. You run it at your own risk. It has not been independently
security-audited; review it yourself before exposing it to untrusted networks or
using it for sensitive data.
