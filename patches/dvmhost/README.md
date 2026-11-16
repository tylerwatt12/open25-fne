# DVMHost integration

These patches target [DVMHost](https://github.com/DVMProject/dvmhost) at
`435ff45b367887ffa94c89cb1a2779628e0f7bfb`. See the upstream build instructions
for its compiler and dependencies. The license text for the patched code is
included in this directory.

Apply the patches in numeric order from a clean upstream checkout. Patch 0012
is a separate timing experiment. The integrated stack uses 0001–0011 and
0013–0081.

```sh
git clone https://github.com/DVMProject/dvmhost.git
git -C dvmhost checkout 435ff45b367887ffa94c89cb1a2779628e0f7bfb
for patch in /path/to/open25-fne/patches/dvmhost/*.patch; do
    case "$patch" in */0012-*) continue;; esac
    git -C dvmhost apply "$patch" || exit 1
done
```

Build `dvmhost`, `dvmfne` and `dvmbridge` together from the patched tree. The
[collection script](../../tools/framework/collect-native.sh) prepares the
Linux ARM64 payload used by the Open25 FNE package target.

## WACN operating policy

Open25 refuses operation with WACN **BEE00–BEEFF** to reduce unintended
subscriber roaming risk. This is an operating policy, separate from the
protocol's numeric address range. It does not reserve those identifiers in the
P25 standard or establish that an identity outside this range is safe to use.
RF frequencies and network identities still require appropriate coordination.

Patch 0041 replaces upstream's numbered exits and silent special cases with a
clear startup refusal. The native `P25Utils::isWACNBlocked` policy is used by
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
python3 patches/dvmhost/tests/wacn-startup.py \
    --bin-dir /path/to/build --work-dir /path/to/internal/test-scratch
```

The startup regression uses the real Host, Bridge and Patch executables. All
256 blocked identities must return the explicit policy error before later
configuration stages; five allowed identities must reach a deliberate later
software-only error. The fixtures configure no modem, audio device or network
connection, and verify that their saved configuration bytes remain unchanged.
This is offline startup qualification, not an RF or roaming test.

Patch 0081 adds dated downstream modification notices to the remaining native
files changed by this patch series. Upstream copyright and license notices are
preserved. The notice date records the current modified-version update; it does
not claim that all earlier changes were first written on that date.
