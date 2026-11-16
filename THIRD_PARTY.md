# Third-party software and attribution

Original Open25 FNE files use the [MIT license](LICENSE). Third-party software
retains its own copyright, license, and notices. The MIT license does not
relicense the native DVM components or the DVM-derived patch set.

## DVMProject native backend

Open25 FNE builds on [DVMProject](https://github.com/DVMProject/dvmhost).
DVMProject's authors and contributors provide the underlying DVMHost, DVMFNE,
and DVMBridge native components. Open25's control services and web interfaces
use modified versions of these components. The modifications are maintained
for Open25 and are not official DVMProject releases. Thank you to the upstream
authors and contributors for their engineering, testing, and maintenance.

The [integration instructions](patches/dvmhost/README.md) select an exact reviewed
commit from the [Open25-maintained native fork](https://github.com/tylerwatt12/dvmhost).
That fork retains upstream history and owns subsequent public native changes.
The patch files here are an immutable historical import snapshot, not a second
implementation to edit. Native files retain their applicable upstream licenses;
neither the fork nor the historical patches replace those licenses.
The native tree's `OPEN25-LICENSING.md` owns its component-license inventory.
The collector copies that document unchanged into each payload, together with
`LICENSE-CLARIFICATION.json`, which records the upstream header corrections and
Open25-owned changes. This summary must be refreshed when the selected native
source's licensing inventory changes.

### Upstream licensing clarification — November 16, 2026

[DVMProject's maintainer clarified the intended grants](https://github.com/DVMProject/dvmhost/issues/137#issuecomment-6926724008)
and corrected the declarations on its `r05a06_dev` branch in
[commit b819cda22f7d22c87f344492a626d8d562b9ea01](https://github.com/DVMProject/dvmhost/commit/b819cda22f7d22c87f344492a626d8d562b9ea01).
The third-party vocoder grants and original notices remain unchanged.

| Component | Applicable licensing |
| --- | --- |
| DVMHost, DVMFNE, Patch and the other non-vocoder DVMProject executables | GPL-2.0-only in their applicable source files; Host and FNE do not link the vocoder library |
| DVMProject-owned common library, bridge and vocoder-wrapper code | GPL-2.0-or-later; the later-version grant permits their use under GPLv3 in the combined bridge |
| Pavel Yazev's IMBE and OP25-derived encoder material, including MBEEncoder.cpp | GPL-3.0-or-later |
| mbelib-derived sources | Retained ISC notices |
| Combined vocoder library and DVMBridge executable | GPL-3.0-or-later |

The corrected later-version grants address the earlier conflicting declarations;
the combined bridge does not rely on a separate linking exception. Preserve
every applicable source-file notice and dated downstream modification notice.
Open25's MIT license applies only to its original files and does not replace
these native terms.

Native distributions must include the complete corresponding source used to
produce their binaries and the applicable build/install materials and notices.
The maintainer also reiterated that source availability requirement in
[the follow-up comment](https://github.com/DVMProject/dvmhost/issues/137#issuecomment-6926811752).
The selected native revision `a3513e8e` retains that clarification and adds the
reviewed features through the running `0092` baseline. It passes a seven-target Linux ARM64
Release build and the selected offline native regressions in a Debian 13 ARM64 VM without
source overlays. Collection verifies its complete source archive, authoritative
licensing documents, provenance and relocated runtime-library closure. This
revision has not been deployed or executed on the physical Pi; no RF or
subscriber-roaming qualification follows from these software checks.

The fork's `LICENSE-CLARIFICATION.json` is an immutable receipt for the earlier
header corrections, rather than a current source-file manifest. Its
`OPEN25-RUNNING-BASELINE.json` records the later functional import. Original
notices, per-file terms and downstream modification dates remain in the source.

The [native collector](tools/framework/collect-native.sh) includes:

- `native/licenses/DVM-GPL-2.0.txt`, the upstream `COPYING` file when present,
  and `native/licenses/DVM-GPL-3.0-or-later.txt`;
- `native/licenses/Open25-native-components.md` and
  `native/licenses/LICENSE-CLARIFICATION.json`, copied from the selected native
  source's authoritative inventory and clarification record;
- `native/source/dvmhost.tar.gz`, copied from the supplied patched source tree;
- `native/source/asio.tar.gz` and its Boost license/COPYING notices;
- notices for bundled OpenSSL, GCC runtime, zlib, and zstd libraries, together
  with the corresponding license texts collected from the build host.

The collector receives source and build directories from the release builder;
the builder must supply the source that produced those binaries. It writes
`native/source-provenance.json` with the source archive hash, Git commit/tree
and worktree state when available, collected binary hashes, and available
CMake metadata hashes. Source exports without Git metadata remain supported.
This evidence records the supplied inputs; it does not independently prove
source/binary equivalence. Preserve the matching build qualification with a
release and provide the source to its recipients under the applicable terms.

## Go executable dependencies

`go.mod` and `go.sum` select the Go dependencies. Their upstream license files
remain authoritative. [collect-go-notices.py](tools/framework/collect-go-notices.py)
reads the actual Linux ARM64 package graph for `cmd/open25-fne`, then copies
the Go runtime and selected dependency license/notice texts unchanged into
`licenses/Go-dependencies.txt` in every complete package.

The selected runtime graph currently includes these dependency families:

| Component | Upstream license / notices |
| --- | --- |
| Coder WebSocket | ISC |
| SIPgo | BSD-2-Clause; [retained notice](third_party/sipgo-LICENSE) |
| gobwas HTTP/head, pool, and WebSocket packages | MIT; [retained notices](third_party/sipgo-dependency-LICENSES) |
| Pion WebRTC, DTLS, ICE, SCTP, SDP, RTP/RTCP, SRTP, STUN/TURN, transport, and supporting modules | MIT |
| go-humanize and icholy/digest | MIT |
| Google UUID, bigfft, and wlynxg/anet | BSD-3-Clause |
| Go runtime and golang.org/x crypto, net, sync, sys, and time packages | BSD-3-Clause, with any additional upstream notices |
| modernc SQLite, libc, mathutil, and memory | BSD-3-Clause plus their inherited third-party notices, including MIT and SQLite public-domain notices |

The generated file lists exact module versions and reproduces the selected
modules' root and imported-package license/notice files. Inherited notice files
may also describe material used only in an upstream source tree or test suite.
It is a notice collection, not a replacement license for those components.

This dependency inventory is derived from `go.mod`, `go.sum`, and the selected
package graph. Refresh this table when dependencies change. Complete packages
regenerate the notice collection during every build; do not reuse a collection
from an earlier dependency graph. To inspect the current collection separately:

```sh
python3 tools/framework/collect-go-notices.py --output build/Go-dependencies.txt
```

## Interface icons

The interface includes Lucide icons and their Feather attribution. Their MIT
and ISC notices are retained in
[internal/frameworkui/LICENSE.lucide](internal/frameworkui/LICENSE.lucide)
and copied to `LICENSE.lucide` in complete packages.

## Speech runtime

The separately prepared eSpeak NG runtime includes its source archive and
GPL-3.0, Apache-2.0, BSD-2-Clause, Unicode, and other applicable notices under
`speech/source/` and `speech/licenses/`. See
[collect-speech.sh](tools/framework/collect-speech.sh). Its licenses are not
replaced by Open25's MIT license.

## Package contents

[package.sh](tools/framework/package.sh) includes `LICENSE`, this file, the
Lucide and existing SIPgo notices, the generated Go dependency notices, and
the supplied native and speech source/license payloads. Packaging requires
Python 3.9 or newer in addition to the Go toolchain. Standalone `make build`
and `make arm64` produce an executable only; use the complete package target
when distributing its collected notices and runtime payloads together.
