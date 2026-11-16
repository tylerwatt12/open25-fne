# Open25 FNE

Open25 FNE is fixed network infrastructure for a P25 Phase 1 system. Its four
services can run together on one host or on separate hosts.

Open25 FNE builds on [DVMProject](https://github.com/DVMProject/dvmhost), using
modified versions of its DVMHost, DVMFNE, and DVMBridge components alongside
Open25's control services and web interfaces. Thank you to DVMProject's authors
and contributors for the underlying native backend and the years of engineering,
testing, and maintenance that made this work possible. The native modifications
are maintained for Open25; they are not official DVMProject releases.

| Service | Default web port | Responsibility |
| --- | --- | --- |
| System Administration | 5000 | System identity, radio directory, talkgroups, users, and profiles |
| Zone Controller | 5001 | RFSS identity, band plan, site registry, and wide-area routing |
| Site Controller | 5002 | Modems, RF channels, local trunking, and channel allocation |
| Application Server | 5003 | Dispatch and PTT testing pages, browser audio, and operator preferences |

Each service owns its configuration and SQLite databases. The executable
includes the web interfaces. Site RF and native vocoding require a compatible
[DVM runtime](patches/dvmhost/README.md). The Dispatch and PTT pages are development and testing interfaces.

Open25's public native modifications are maintained in the separate
[DVMHost fork](https://github.com/tylerwatt12/dvmhost), retaining DVMProject's
upstream history. The [native integration instructions](patches/dvmhost/README.md)
select the exact reviewed commit used by this source release.

## Build and test

Use Go 1.26.3 or newer. The JavaScript tests also use Node.js and Playwright with
Chromium. See [installation and testing](docs/installation.md) for details.

```sh
make build
go test -mod=readonly ./...
```

`make arm64` cross-builds the Go executable for Linux ARM64.

## License and third-party software

See [LICENSE](LICENSE) for the license of original Open25 files and its scope.
Native DVM components and DVM-derived patches retain their applicable upstream
licenses and notices, including GPL-2.0-only and embedded GPL-3.0-or-later
declarations. Other dependencies keep their own licenses.
[THIRD_PARTY.md](THIRD_PARTY.md) describes the components, packaged notices and
source, and the unresolved license compatibility issue in the combined native
vocoder/backend. The MIT license does not resolve that issue.
