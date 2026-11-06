# Open25 FNE

Open25 FNE is fixed network infrastructure for a P25 Phase 1 system. Its four
services can run together on one host or on separate hosts.

| Service | Default web port | Responsibility |
| --- | --- | --- |
| System Administration | 5000 | System identity, radio directory, talkgroups, users, and profiles |
| Zone Controller | 5001 | RFSS identity, band plan, site registry, and wide-area routing |
| Site Controller | 5002 | Modems, RF channels, local trunking, and channel allocation |
| Application Server | 5003 | Dispatch and PTT testing pages, browser audio, and operator preferences |

Each service owns its configuration and SQLite databases. The executable
includes the web interfaces. Site RF and native vocoding require a compatible
[DVM runtime](patches/dvmhost/README.md). The Dispatch and PTT pages are development and testing interfaces.

## Build and test

Use Go 1.26.3 or newer. The JavaScript tests also use Node.js and Playwright with
Chromium. See [installation and testing](docs/installation.md) for details.

```sh
make build
go test -mod=readonly ./...
```

`make arm64` cross-builds the Go executable for Linux ARM64.
