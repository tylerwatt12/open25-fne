# DVMHost integration

These patches target [DVMHost](https://github.com/DVMProject/dvmhost) at
`435ff45b367887ffa94c89cb1a2779628e0f7bfb`. See the upstream build instructions
for its compiler and dependencies. The license text for the patched code is
included in this directory.

Apply the patches in numeric order from a clean upstream checkout. Patch 0012
is a separate timing experiment. The integrated stack uses 0001–0011 and
0013–0080.

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
