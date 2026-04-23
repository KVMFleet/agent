# EuroKVM Agent

Lightweight fleet agent for KVM-over-IP devices. Connects any KVM device with a web interface to the [EuroKVM Fleet](https://eurokvm.io) management platform.

## What it does

- Enrolls with the platform using a single-use token
- Opens an outbound WebSocket tunnel (works behind NAT, CGNAT, firewalls — no inbound ports)
- Reports health metrics (temperature, uptime, agent version)
- Reverse-proxies the device's web UI through the tunnel (HTTP-over-WS multiplex)
- Works with any KVM device that has a local web interface — no vendor lock-in

## Install

SSH into your KVM device and run:

```bash
curl -sSL https://app.eurokvm.io/install | sh -s -- --token <your-enrollment-token>
```

Get a token from the [EuroKVM dashboard](https://app.eurokvm.io) → Fleet → + Add device.

The script detects your device type and architecture, downloads the right binary, sets up a service, and enrolls automatically.

## Supported architectures

| Architecture | Binary | Example devices |
|---|---|---|
| ARMv7 (armv7l) | `eurokvm-agent.linux-arm` | JetKVM, NanoKVM, Luckfox-based KVMs |
| ARM64 (aarch64) | `eurokvm-agent.linux-arm64` | PiKVM, Raspberry Pi-based KVMs |
| x86_64 | `eurokvm-agent.linux-amd64` | Generic Linux, VMs, any x86 KVM appliance |

Single static binary, ~5 MB, no dependencies.

## Works with any KVM web UI

The agent is device-agnostic. It reverse-proxies whatever local web interface you point it at:

```bash
# Auto-detected for known devices, or set manually:
EUROKVM_KVMD_URL=http://127.0.0.1/       # most devices
EUROKVM_KVMD_URL=https://127.0.0.1/      # devices with self-signed TLS
EUROKVM_KVMD_URL=http://192.168.1.50/    # proxy a device on the local network
```

If your KVM has a web interface, the agent can proxy it through the EuroKVM platform.

## Build from source

```bash
# Native build
go build -trimpath -o eurokvm-agent ./

# Cross-compile for ARM64
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o eurokvm-agent.linux-arm64 ./

# Cross-compile for ARMv7
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
| `EUROKVM_HW_KIND` | `--hw-kind` | auto | Hardware type identifier |
| `EUROKVM_KVMD_URL` | `--kvmd-url` | auto-detected | URL of local web UI to proxy |
| `EUROKVM_CONSOLE_ADDR` | `--console-addr` | `:8080` | Local HTTP server bind (`off` to disable) |

## How it works

```
KVM device (your network, behind NAT)
    │
    │  Agent opens outbound WSS connection
    │  (no inbound ports needed)
    │
    ▼
EuroKVM Platform (app.eurokvm.io)
    │
    │  HTTP-over-WS multiplex: platform sends
    │  http.request frames through the tunnel,
    │  agent serves responses from local web UI
    │
    ▼
Operator browser (anywhere)
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
