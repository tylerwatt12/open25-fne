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
The root DVM [license text](patches/dvmhost/LICENSE) is GPLv2, and core files
such as [Host.cpp](https://github.com/DVMProject/dvmhost/blob/435ff45b367887ffa94c89cb1a2779628e0f7bfb/src/host/Host.cpp)
declare **GPL-2.0-only**. Embedded components have additional declarations:

- Pavel Yazev's [IMBE encoder/decoder](https://github.com/DVMProject/dvmhost/blob/435ff45b367887ffa94c89cb1a2779628e0f7bfb/src/vocoder/imbe/imbe_vocoder.h)
  declares **GPL-3.0-or-later**.
- [MBEEncoder.cpp](https://github.com/DVMProject/dvmhost/blob/435ff45b367887ffa94c89cb1a2779628e0f7bfb/src/vocoder/MBEEncoder.cpp)
  carries both a GPL-2.0-only SPDX declaration and a retained GPL-3.0-or-later
  declaration for OP25-derived AMBE encoder material.

Preserve every applicable upstream copyright and license notice and identify
changes and their dates in modified native source files.

### Unresolved native license compatibility

The pinned build includes the IMBE sources in its static vocoder library and
links that library into DVMBridge alongside GPL-2.0-only components. The reviewed
upstream `LICENSE`, `COPYING`, README, source and documentation do not provide
a linking exception or additional permission resolving the GPL version mismatch.
The [GNU compatibility guidance](https://www.gnu.org/licenses/gpl-faq.html#v2v3Compatibility)
states that GPL-2.0-only and GPLv3 code cannot be combined under their ordinary
terms; GPL-2.0-or-later would be a different grant.

**Distribution of the combined native vocoder/backend remains an unresolved
licensing issue.** Copying both license texts, preserving source, or licensing
Open25's original files under MIT does not resolve it. Before distributing that
combined native binary, establish applicable additional permission or another
compatible implementation/build arrangement. This repository does not claim
that permission has been obtained or that the combined binary's licensing is
resolved. No upstream license declaration has been changed.

The clarification request is tracked in
[DVMProject/dvmhost issue 137](https://github.com/DVMProject/dvmhost/issues/137).

The [native collector](tools/framework/collect-native.sh) includes:

- `native/licenses/DVM-GPL-2.0.txt` and the upstream `COPYING` file when present;
- `native/source/dvmhost.tar.gz`, copied from the supplied patched source tree;
- `native/source/asio.tar.gz` and its Boost license/COPYING notices;
- notices for bundled OpenSSL, GCC runtime, zlib, and zstd libraries, together
  with the corresponding license texts collected from the build host.

For a distributed native binary, retain the corresponding modified source and
the scripts needed to build and install it under the applicable GPL terms.
The collector receives source and build directories from the release builder;
the builder must use the source that produced those binaries. A checksum alone
does not establish that relationship.

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
