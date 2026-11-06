# Installation and testing

Build the executable with `make build`. Create a private initial configuration
JSON file with the following documents.

| Field | Purpose |
| --- | --- |
| `system` | WACN, System ID, station callsign, radio directory, and talkgroups |
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

The package target assembles an ARM64 executable and the systemd units using
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
