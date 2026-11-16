# DVM native source integration

The [Open25-maintained DVMHost fork](https://github.com/tylerwatt12/dvmhost) is
the authoritative source for Open25's public native modifications. It retains
[DVMProject's](https://github.com/DVMProject/dvmhost) upstream history, authors,
copyrights and per-file license notices. Its modified components are maintained
for Open25 and are not official DVMProject releases.

This source release selects the exact reviewed native revision below. Clone it
directly; no additional patches from this directory are applied:

```sh
git clone https://github.com/tylerwatt12/dvmhost.git
git -C dvmhost checkout --detach a3513e8ec0c258198e80b2a0f49a250276a5b00e
```

Follow `OPEN25.md` in the checked-out native tree for build and test instructions.
Build `dvmhost`, `dvmfne` and `dvmbridge` together from this selected tree. The
[collection script](../../tools/framework/collect-native.sh) prepares the
Linux ARM64 payload used by the Open25 FNE package target. Supply the same
source that produced its binaries, including all applicable license notices.
Collection uses Python 3 for structured source/build provenance and preserves
source exports that do not contain Git metadata.

Make future native changes and their tests in the fork. Update the exact source
pin here only after review and appropriate qualification. The Go startup
preflight remains owned by Open25 FNE; it is the supported caller's early error
path and must stay synchronized with the native operating policy.

This revision includes the native feature changes through the running `0092`
baseline. The fork's `OPEN25-RUNNING-BASELINE.json` records their provenance.
The restored WACN restriction and clarified licensing are retained. The fork
also keeps Linux TUN/TAP compilation and the correct 460800-baud constant,
rather than two platform adjustments in the historical build archive. This
selects the running features with those reviewed differences; it does not claim
to reproduce the installed executable bytes. Later uninstalled candidates are
excluded.

## Historical patch snapshot

The patch files and startup fixture in this directory are frozen provenance for
the [initial remediation source snapshot](https://github.com/tylerwatt12/open25-fne/commit/a9334864d2345c4cafa61af2d0e5714bc65e9211).
The integrated import used 0001–0011 and 0013–0081 on upstream
`435ff45b367887ffa94c89cb1a2779628e0f7bfb`. Patch 0012 is a separate timing
experiment and was excluded. Each selected patch is now a separate native
commit; the fork's `open25-import.json` records the hashes and commit mapping.

Do not edit or refresh this historical export, or apply it again to the fork.
Subsequent native work belongs only in the fork. This preserves one current
source owner while keeping the original published patch snapshot reproducible.

## WACN operating policy

Open25 refuses operation with WACN **BEE00–BEEFF** to reduce unintended
subscriber roaming risk. This is an operating policy, separate from the
protocol's numeric address range. It does not reserve those identifiers in the
P25 standard or establish that an identity outside this range is safe to use.
RF frequencies and network identities still require appropriate coordination.

The imported patch 0041 replaced upstream's numbered exits and silent special
cases with a clear startup refusal. The native `P25Utils::isWACNBlocked` policy is used by
Host, Bridge and Patch configuration readers. Host checks it before modem
initialization when P25 is enabled. The Open25 Go radio-start interface checks
the same range before acquiring resources or launching a native child, including
when an older native payload is present. Keep both supported startup paths and
their exhaustive range tests synchronized when changing this fixed policy.
Administrator settings remain editable and saved exactly as entered; startup
refusal never substitutes a different WACN or rewrites programming.

The offline protocol tests can still encode valid numeric addresses and exercise
internal controller methods for this range. Those tests establish protocol
behavior; they do not authorize RF operation or qualify subscriber roaming.

After building the native targets with `ENABLE_TESTS=ON`, run:

```sh
/path/to/build/dvmtests '[wacn-operating-policy],[wacn-address]'
python3 dvmhost/tests/open25/wacn_startup.py \
    --bin-dir /path/to/build --work-dir /path/to/internal/test-scratch
```

The startup regression uses the real Host, Bridge and Patch executables. All
256 blocked identities must return the explicit policy error before later
configuration stages; five allowed identities must reach a deliberate later
software-only error. The fixtures configure no modem, audio device or network
connection, and verify that their saved configuration bytes remain unchanged.
This is offline startup qualification, not an RF or roaming test.

The upstream maintainer clarified the component licensing in
[issue 137](https://github.com/DVMProject/dvmhost/issues/137#issuecomment-6926724008)
and corrected its `r05a06_dev` source declarations in
[b819cda22f7d22c87f344492a626d8d562b9ea01](https://github.com/DVMProject/dvmhost/commit/b819cda22f7d22c87f344492a626d8d562b9ea01).
The native tree's `OPEN25-LICENSING.md` and `LICENSE-CLARIFICATION.json` record
the component terms and the exact corrections incorporated downstream; they
remain with the corresponding source and are copied into collected payloads.
The combined vocoder library and DVMBridge use GPL-3.0-or-later. Host and FNE
retain their GPL-2.0-only terms and do not link that vocoder library. See
[THIRD_PARTY.md](../../THIRD_PARTY.md) for the component summary.

The selected running-baseline native revision passes a fresh seven-target
Linux ARM64 Release build in a Debian 13 ARM64 VM without source overlays.
Its offline P25, EDAC/crypto, FNE and WACN suites pass, including all 783 real
Host/Bridge/Patch startup fixtures. The collected payload passes exact source,
licensing-document, provenance and relocated six-library dependency checks.
This qualifies the selected software build and collection on that VM. It does
not qualify physical Pi execution, RF, subscriber roaming or deployment; those
operations were not performed for this revision.

The imported patch 0081 added dated downstream modification notices to the
remaining native files changed by this patch series. Upstream copyright and license notices are
preserved. The notice date records the current modified-version update; it does
not claim that all earlier changes were first written on that date.
