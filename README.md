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

The current source includes Site-owned digital BSI and control-channel policy,
optional Site startup, managed neighbor announcements, requested-Site routing,
and the shared System history archive with durable source outboxes. Saved radio
programming remains separate from explicit operating actions. Station
identification uses digital BSI; CWID/Morse scheduling is retired.

The public runtime is synchronized with the running lab's application and native
feature baseline, retaining the later licensing corrections and the explicit
BEE00–BEEFF operating restriction. Private operational state, configurations,
build output, captures and research material are not part of this source release.
Publishing source does not update an existing installation or qualify RF use.

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
licenses and notices. DVMHost and DVMFNE remain GPL-2.0-only; the combined
vocoder library and DVMBridge use GPL-3.0-or-later under the
[upstream licensing clarification](https://github.com/DVMProject/dvmhost/issues/137#issuecomment-6926724008).
Other dependencies keep their own licenses. [THIRD_PARTY.md](THIRD_PARTY.md)
describes the per-component terms, corrected source declarations and complete
corresponding source and notices required with native distributions.
