# sshd - Lightweight SSH Server

Minimal SSH server with dual-protocol support. Single static binary, no dependencies.

## Features

- SSH exec mode (non-interactive commands)
- Interactive PTY with window resize
- SFTP subsystem (file transfer)
- WebSocket transport (same port, auto-detect)
- Runtime RSA key generation (no key files needed)
- Password auth from environment variable

## Usage

```bash
# Start (password and port from env)
PW=$SECRET PORT=2222 ./sshd-linux-amd64
```

## Connect

```bash
# Direct SSH
ssh -p 2222 root@host

# Via WebSocket (cloudflared proxy)
ssh -o ProxyCommand="cloudflared access ssh --hostname xxx.trycloudflare.com" -p 2222 root@host

# SFTP
scp -P 2222 file.txt root@host:/tmp/
```

## Protocol Detection

Same port serves both protocols:
- First byte `S` (SSH-2.0 banner) -> SSH handler
- Otherwise -> HTTP/WebSocket upgrade -> SSH over WS

## Build

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o sshd-linux-amd64 .
upx --best --lzma sshd-linux-amd64  # ~2MB
```

## Environment

| Var | Default | Description |
|-----|---------|-------------|
| `PORT` | (required) | Listen port |
| `PW` | (required) | Auth password |

## Download

https://github.com/hhsw2015/sshd/releases
