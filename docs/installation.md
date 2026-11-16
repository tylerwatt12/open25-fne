# Installation and testing

Build the executable with `make build`. Create a private initial configuration
JSON file with the following documents.

| Field | Purpose |
| --- | --- |
| `system` | WACN, System ID, radio directory, and talkgroups |
| `zone` | RFSS ID, channel identifiers, and site registry |
| `sites` | Initial site's modem, channel, and station-identification settings |
| `application` | Application Server name and upstream connection |
| `administrator` | Administrator `username` and bcrypt `password_hash` |

The configuration structures are defined in
[`internal/framework/initial_config.go`](../internal/framework/initial_config.go).
Keep the seed and initialized state outside the source checkout. From the source
checkout, use a new state directory and a seed readable only by your account.

```sh
state_root="$HOME/.local/state/open25-fne"
chmod 600 /path/to/initial-configuration.json
./build/open25-fne init --config /path/to/initial-configuration.json --root "$state_root"

# Run each command in a separate terminal.
./build/open25-fne system --config "$state_root/system/service.json"
./build/open25-fne zone --config "$state_root/zone/service.json"
./build/open25-fne site --config "$state_root/site/service.json"
./build/open25-fne application --config "$state_root/application/service.json"
```

The web listeners default to loopback ports 5000–5003. Run
`./build/open25-fne init --help` for HTTPS and private-network listener options.
Use HTTPS for web access across a network and keep the internal HTTP, SIP and
RTP connections on the private service network. Set the Site's native runtime
paths and provision its modem and channel assignments before starting RF.

Each Site owns its NAC, station callsign, digital BSI settings, modem assignments
and control-channel selection policy. Station identification is digital BSI;
the former CWID/Morse controls are removed. The Site's optional automatic-start
setting defaults off. Saving programming never starts RF; an enabled automatic
start applies only when that Site service starts.

System owns the shared history archive and retention policy. Other services
keep durable delivery outboxes and forward accepted events to System. Their
administration pages link to System for historical views; live observations
remain with their existing service owners.

Open25 blocks operation with a home WACN in `BEE00`–`BEEFF` to reduce the risk
of unintended subscriber roaming. Start Site reports the configured WACN and
the reason before modem access or native process launch. Direct native host,
bridge and patch startup enforce the same policy. Saved programming is retained
exactly; the software never substitutes a different network identity. This is
an operating precaution, not a claim that the values are reserved by P25 or
that other identities guarantee safe RF operation. Coordinate system identities
and frequencies before operating. See the [native integration policy](../patches/dvmhost/README.md).

The package target requires Python 3.9 or newer to collect dependency notices.
It assembles an ARM64 executable and the systemd units using
separately prepared and checksummed native and speech payloads. See
[`tools/framework/package.sh`](../tools/framework/package.sh) for its inputs.

## Browser tests

Install Playwright and Chromium from the checkout root, then run the UI suite.

```sh
npm install --no-save --package-lock=false playwright
npx playwright install chromium
make ui-test
make ui-browser-test
```

`TRUNKSYSTEM_PLAYWRIGHT` can point to an existing Playwright module. Go tests
that need a reviewed native payload require `TRUNKSYSTEM_NATIVE_DIR`.
