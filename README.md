# EuroKVM Agent

Lightweight fleet agent for KVM-over-IP devices. Connects your [PiKVM](https://pikvm.org/), [JetKVM](https://jetkvm.com/), or TinyPilot to the [EuroKVM Fleet](https://eurokvm.io) management platform.

## What it does

- Enrolls with the platform using a single-use token
- Opens an outbound WebSocket tunnel (works behind NAT, CGNAT, firewalls — no inbound ports)
- Reports health metrics (temperature, uptime, agent version)
- Proxies the device's KVM web UI through the tunnel (HTTP-over-WS multiplex)
- On JetKVM: reverse-proxies to `http://127.0.0.1:80` (JetKVM's built-in UI)
- On PiKVM: reverse-proxies to `https://127.0.0.1/` (kvmd)

## Install

SSH into your KVM device and run:

```bash
curl -sSL https://app.eurokvm.io/install | sh -s -- --token <your-enrollment-token>
```

Get a token from the [EuroKVM dashboard](https://app.eurokvm.io) → Fleet → + Add device.

The script detects your device type (JetKVM/PiKVM/generic Linux), downloads the right binary, sets up a service, and enrolls automatically.

## Supported devices

| Device | Architecture | Agent binary | Install path |
|---|---|---|---|
| JetKVM | ARM Cortex-A7 (armv7l) | `eurokvm-agent.linux-arm` | `/userdata/eurokvm/agent` |
| PiKVM v3/v4 | ARM Cortex-A72 (aarch64) | `eurokvm-agent.linux-arm64` | `/usr/local/bin/eurokvm-agent` |
| Generic Linux | x86_64 | `eurokvm-agent.linux-amd64` | `/usr/local/bin/eurokvm-agent` |

## Build from source

```bash
# Native build
go build -trimpath -o eurokvm-agent ./

# Cross-compile for PiKVM (arm64)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o eurokvm-agent.linux-arm64 ./

# Cross-compile for JetKVM (armv7)
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -o eurokvm-agent.linux-arm ./
```

## Configuration

All configuration via environment variables or flags:

| Env var | Flag | Default | Description |
|---|---|---|---|
| `EUROKVM_API` | `--api` | `http://localhost:8000` | Platform URL |
| `EUROKVM_TOKEN_FILE` | `--token-file` | — | Path to file containing enrollment token |
| `EUROKVM_STATE` | `--state` | `/var/lib/eurokvm/state.json` | Persistent state file |
| `EUROKVM_DEVICE_NAME` | `--name` | hostname | Device name in dashboard |
| `EUROKVM_DEVICE_TAGS` | `--tags` | — | Comma-separated tags |
| `EUROKVM_HW_KIND` | `--hw-kind` | `pikvm-v4` | Hardware type identifier |
| `EUROKVM_KVMD_URL` | `--kvmd-url` | auto-detected | URL of local KVM web UI to proxy |
| `EUROKVM_CONSOLE_ADDR` | `--console-addr` | `:8080` | Local HTTP server bind address (`off` to disable) |

## How it works

```
KVM device (your network, behind NAT)
    │
    │  eurokvm-agent opens outbound WSS connection
    │  (no inbound ports needed)
    │
    ▼
EuroKVM Platform (app.eurokvm.io)
    │
    │  HTTP-over-WS multiplex: platform sends
    │  http.request frames through the tunnel,
    │  agent serves responses from local KVM UI
    │
    ▼
Operator browser (anywhere)
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
