# hubhq

Personal Go CLI for a local IKEA DIRIGERA hub.

## What it does

- Discovers a DIRIGERA hub on the local network with mDNS.
- Falls back to a bounded local subnet scan when mDNS discovery fails.
- Pairs interactively and stores the access token in a dedicated local token file.
- Lists, fetches, updates, renames, and deletes devices.
- Integrates configured DukaOne ventilation devices into the same device inventory and control flow.
- Lists, fetches, triggers, renames, and deletes scenes.
- Streams hub update events.
- Executes non-destructive natural-language actions with a hosted LLM.
- Generates reviewable automation specs from natural language and stores them locally.
- Can apply a supported subset of auto specs as DIRIGERA scenes, including event-triggered speaker sounds.
- Supports speakers from the local machine, DIRIGERA hub, Google Cast devices, and AirPlay/RAOP speakers on the local network.
- Keeps a local cache of devices and scenes for fast human-friendly lookup.
- Prints human-readable output by default and JSON with `--json`.

## Current assumptions

This CLI is built around the local DIRIGERA API behavior described by reverse-engineered community clients:

- mDNS service type `_ihsp._tcp`
- HTTPS API on port `8443`
- PKCE pairing through `/v1/oauth/authorize` and `/v1/oauth/token`
- REST resources under `/v1/devices`, `/v1/scenes`, and `/v1/home`
- WebSocket updates on one of a few candidate paths

The REST surface is isolated in [`internal/client/client.go`](./internal/client/client.go), so if your hub firmware differs, the adjustments should stay localized.

## Config location

- macOS:
  - `~/Library/Application Support/dirigera-cli/config.json`
  - `~/Library/Application Support/dirigera-cli/tokens.json`
  - `~/Library/Application Support/dirigera-cli/cache.json`
- Linux:
  - `${XDG_CONFIG_HOME:-~/.config}/dirigera-cli/config.json`
  - `${XDG_CONFIG_HOME:-~/.config}/dirigera-cli/tokens.json`
  - `${XDG_CONFIG_HOME:-~/.config}/dirigera-cli/cache.json`

## Commands

```text
dirigera auth
dirigera auth --host 192.168.1.25
dirigera hub list
dirigera hub discover
dirigera hub probe 192.168.1.25
dirigera hub import-token 192.168.1.25 <token>
dirigera config get llm.api_key
dirigera config set llm.api_key sk-...
dirigera config set apple.raop_password secret
dirigera config set apple.airplay_credentials <credentials>
dirigera config set dukaone.password 1111
dirigera config set dukaone.python python3
dirigera dukaone discover
dirigera dukaone discover 192.168.1.255
dirigera dukaone add
dirigera dukaone list
dirigera dukaone add ABC12345 "Bedroom Vent" 192.168.1.50 Bedroom
dirigera dukaone remove "Bedroom Vent" --yes
dirigera speaker list
dirigera speaker play sound-0
dirigera speaker play sound-3 "Library Speaker"
dirigera speaker play ./clip.wav
dirigera speaker play https://example.com/clip.mp3 "Library Speaker"
dirigera ask "turn on kitchen lights"
dirigera --show-script ask "turn off all lights in the Library"
dirigera ask "what is on right now"
dirigera ask "pause the Library Speaker"
dirigera ask "set Library Speaker volume to 25"
dirigera auto "turn on hallway at sunset and off at 11pm"
dirigera --show-script auto "turn on porch light at sunset"
dirigera auto "when the kitchen door opens play a sound in the Library Speaker"
dirigera auto list
dirigera auto run
dirigera auto test <id>
dirigera auto show <id>
dirigera auto apply <id>
dirigera auto delete <id>
dirigera refresh
dirigera dump --json

dirigera devices list
dirigera devices get "Kitchen Lamp"
dirigera devices set "Kitchen Lamp" isOn=true lightLevel=50
dirigera devices rename "Kitchen Lamp" "Kitchen Main Lamp"
dirigera devices delete "Old Outlet" --yes

dirigera scenes list
dirigera scenes get "Movie Time"
dirigera scenes trigger "Movie Time"
dirigera scenes rename "Movie Time" "Movie"
dirigera scenes delete "Old Scene" --yes

dirigera watch
dirigera watch --json
```

## Build

```bash
go mod tidy
go build -o hubhq ./cmd/hubhq
```

## Linux quick start

Install Go 1.23+ and one local audio playback tool:

```bash
sudo apt install golang pulseaudio-utils
```

Build and run:

```bash
go build -o hubhq ./cmd/hubhq
./hubhq help
./hubhq auth
```

On Linux, local `speaker play sound-0` through `speaker play sound-5` and local `speaker play <file-or-url>` use a system audio tool such as `paplay` or `aplay`.

## DukaOne ventilation

This CLI can integrate DukaOne S6W ventilation devices as first-class `TYPE=ventilation` devices.

Install the Python SDK used by the local bridge:

```bash
python3 -m pip install dukaonesdk==1.0.5
```

Optional config:

```bash
./dirigera config set dukaone.python python3
./dirigera config set dukaone.password 1111
./dirigera config set dukaone.backend native
```

Register a device and refresh the unified cache:

```bash
./dirigera dukaone discover
./dirigera dukaone discover 192.168.1.255
./dirigera dukaone add
./dirigera dukaone add <device-id> "Bedroom Vent" 192.168.1.50 Bedroom
./dirigera refresh
./dirigera devices list
```

DukaOne commands:

```bash
./dirigera dukaone discover [broadcast-address]
./dirigera dukaone add
./dirigera dukaone list
./dirigera dukaone add <device-id> "Bedroom Vent" [ip-address] [room]
./dirigera dukaone rename <name-or-id> "Bedroom Vent"
./dirigera dukaone room <name-or-id> Bedroom
./dirigera dukaone remove <name-or-id> --yes
```

Once registered, DukaOne devices can be targeted through `devices get`, `devices set`, `ask`, `auto`, and the script runtime like other devices.

Examples:

```bash
./dirigera devices get "Bedroom Vent"
./dirigera devices set "Bedroom Vent" isOn=true
./dirigera devices set "Bedroom Vent" mode=inout preset=medium
./dirigera devices set "Bedroom Vent" manualSpeed=120
./dirigera devices set "Bedroom Vent" resetFilter=true
./dirigera ask "set the Bedroom Vent to medium"
./dirigera auto "at 22:00 turn the Bedroom Vent off"
```

Supported DukaOne write attributes:

- `isOn=true|false`
- `mode=in|out|inout`
- `preset=off|low|medium|high|manual`
- `manualSpeed=0..255`
- `resetFilter=true`

For the in-progress native Go backend for discovery and state reads:

```bash
./dirigera config set dukaone.backend native
./dirigera dukaone discover
./dirigera dukaone list
```

Native backend support now includes:

- discovery
- state reads
- `devices set`
- `ask`
- `auto` / script runtime control

The Python bridge remains available as a fallback when `dukaone.backend` is not set to `native`.

## LLM setup

Set these before using `ask` or `auto`:

```bash
export DIRIGERA_LLM_API_KEY=...
export DIRIGERA_LLM_MODEL=gpt-4.1-mini
export DIRIGERA_LLM_BASE_URL=https://api.openai.com/v1
export DIRIGERA_LLM_PROVIDER=openai-compatible
```

## Script workflow

`ask` and `auto` now generate JavaScript for the built-in automation runtime.

Useful commands:

```bash
./dirigera --show-script ask "turn off all lights in the Library"
./dirigera --show-script auto "when the kitchen door opens turn on the hallway light"
./dirigera auto apply <id>
./dirigera auto test <id>
./dirigera --debug auto run
```

Location config for `sun_event` automations:

```bash
./dirigera config set location.latitude 42.3601
./dirigera config set location.longitude -71.0589
./dirigera config set location.time_zone America/New_York
```

## Notes

- Ambiguous names fail instead of picking an arbitrary match.
- Destructive actions require `--yes`.
- `ask` only auto-executes non-destructive actions. Destructive plans are blocked pending confirmation support.
- `auto` stores pending specs locally in the app config directory.
- `auto apply` currently supports a subset of hub-native automations by compiling them into DIRIGERA scenes.
- Supported subset includes event-triggered scenes and built-in speaker audio clips for open/close style notifications.
- Built-in speaker clips are limited to `ikea://notify/03_device_open` and `ikea://notify/04_device_close`.
- `speaker list` shows the local computer speaker plus compatible speakers discovered from the hub and local network.
- `speaker play sound-0` through `speaker play sound-5` without a name use the local computer speaker.
- `speaker play sound-0 "<name>"` through `speaker play sound-5 "<name>"` route to the matching speaker without exposing backend-specific commands in the CLI.
- `speaker play sound-0 speaker-all` plays the built-in sound on all discovered speakers.
- `speaker play sound-0 "Kitchen, Library Speaker"` plays the built-in sound on each matching speaker in the comma-separated list.
- `speaker play <filename-or-url>` plays media on the local computer speaker by default.
- `speaker play <filename-or-url> "<name>"` plays media on the matching speaker when that backend supports arbitrary media playback.
- Local `speaker play` currently supports WAV files/URLs and uses a system audio tool on the local machine.
- AirPlay/RAOP speakers discovered in `speaker list` support `speaker play sound-0 "<name>"` through `speaker play sound-5 "<name>"` and `speaker play ... "<name>"` through classic AirPlay playback.
- `config.json` stores hub metadata and CLI settings, `tokens.json` stores hub access tokens, and `cache.json` stores the local device/scene cache.
- `config get` / `config set` currently support: `llm.provider`, `llm.base_url`, `llm.model`, `llm.api_key`, `apple.airplay_credentials`, `apple.airplay_password`, `apple.raop_credentials`, `apple.raop_password`, `dukaone.backend`, `dukaone.python`, `dukaone.password`, `location.latitude`, `location.longitude`, `location.time_zone`.
- If `hub list` does not discover your hub, pair directly with `dirigera auth --host <hub-ip>`.
- `dirigera hub probe <hub-ip>` checks whether port `8443` is reachable before pairing.
- Discovery uses platform tools:
  - macOS: `dns-sd`
  - Linux: `avahi-browse`
- If mDNS returns nothing, discovery falls back to scanning active private IPv4 subnets and probing HTTPS on port `8443`.
- The scan is intentionally bounded to local private `/24` ranges per active interface to avoid broad network sweeps.
