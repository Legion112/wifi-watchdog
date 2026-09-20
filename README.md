# wifi-watchdog

Keeps a NetworkManager-managed Wi-Fi link alive on a headless Linux box, and
—more importantly—refuses to touch it when it cannot prove the link is broken.

A Go rewrite of a shell watchdog that had been quietly making things worse.

## What it does

Every interval it checks three things:

1. NetworkManager reports the interface as `connected`
2. the interface carries an IPv4 default route
3. that route's **nexthop** answers an ICMP echo request

When the link is genuinely broken it escalates as gently as possible:
reactivate the connection profile (`nmcli connection up`), re-check, and only if
that was not enough restart NetworkManager. The first step that works wins.

## Why it was rewritten

The shell version hardcoded its probe target:

```sh
GATEWAY="${WIFI_GATEWAY:-192.168.1.1}"
```

The network it ran on was `192.168.8.0/24`. That address had not been routable
for a long time, so the reachability check could **never** pass. Every 60
seconds the watchdog therefore concluded a perfectly healthy link was dead,
bounced `wlan0`, re-checked, failed again, and restarted NetworkManager — about
two outages a minute, indefinitely. It was manufacturing the very instability it
then reported.

That is not a typo; it is a design flaw with three separate causes, and this
rewrite addresses each of them:

| Failure | Fix |
|---|---|
| A hardcoded probe target goes stale when the LAN is renumbered | The gateway is **derived** from the interface's actual default route. `--gateway` exists as an override and defaults to empty. |
| "I could not check" was indistinguishable from "it is broken" | Three-way verdict: `healthy` / `unhealthy` / **`unknown`**. Recovery only ever runs on `unhealthy`. Missing privileges, an absent interface or an unreachable NetworkManager all yield `unknown` and **no action**. |
| One bad check triggered an immediate bounce, forever | `--failure-threshold` requires N consecutive failures, and `--cooldown` bounds how often recovery may run at all. |

The derived gateway has a second benefit: it stays correct when the default
route legitimately changes. On the host this was written for, the default route
now points at a policy-routing gateway rather than the ISP router, and the check
followed it with no reconfiguration.

## Install

```bash
make build
sudo make install         # /usr/local/bin + /etc/systemd/system
sudo systemctl daemon-reload
sudo systemctl enable --now wifi-watchdog
```

Cross-compiling for a Raspberry Pi or similar:

```bash
make build-arm64          # -> bin/wifi-watchdog-linux-arm64
```

## Usage

```
wifi-watchdog run        watch the link and recover when it breaks
wifi-watchdog check      diagnose once; read-only, never acts
wifi-watchdog recover    diagnose once and recover if needed
```

`check` is safe to run at any time and is the quickest way to see what the
watchdog sees:

```console
# wifi-watchdog check
healthy: wlan0 connected, default via 192.168.8.162, gateway answers
  device    wlan0 state=connected connection=MTS_GPON_2F34
  route     default via 192.168.8.162 proto static metric 600
  gateway   192.168.8.162
```

Its exit code distinguishes all three verdicts, so it composes with other tools:

| Exit | Meaning |
|------|---------|
| 0 | healthy |
| 1 | unhealthy |
| 2 | unknown — could not determine, so draw no conclusion |

### Flags

| Flag | Default | Notes |
|------|---------|-------|
| `--iface` | `wlan0` | interface to watch |
| `--connection` | *(auto)* | profile to reactivate; defaults to whatever is active on `--iface` |
| `--gateway` | *(derived)* | override the probe target. Prefer leaving it empty — see above |
| `--interval` | `1m` | how often to check (`run`) |
| `--failure-threshold` | `2` | consecutive failures before acting |
| `--cooldown` | `5m` | minimum gap between recovery attempts |
| `--allow-restart` | `true` | permit the NetworkManager restart step |
| `--ping-count` | `2` | echo requests per probe |
| `--ping-timeout` | `2s` | per-request timeout |
| `--verbose` | `false` | log healthy checks too |

## Privileges

Needs root (or `CAP_NET_RAW` plus `CAP_NET_ADMIN`) for the raw ICMP socket,
`nmcli` device operations and restarting NetworkManager. Without the ICMP
capability the verdict is `unknown` and the watchdog deliberately does nothing —
it will not "fix" a permissions problem by restarting your network.

## A service, not a timer

The shell version ran from a systemd timer with `OnBootSec=2min` and
`OnUnitActiveSec=1min`. That combination has a trap: if the timer is ever
re-enabled *after* the `OnBootSec` window has passed, `OnUnitActiveSec` has
nothing to anchor to, `NextElapseUSecMonotonic` becomes `infinity`, and the
timer silently never fires again for the rest of the boot. It reads as `active`
the whole time.

This runs as a long-lived service with `Restart=always` instead, which has no
such anchoring behaviour and keeps the failure-threshold and cooldown state in
memory rather than on disk.

## Development

```bash
make check     # gofmt + vet + tests
```

Go 1.27, no external dependencies. Everything that touches the system goes
through a `Runner` interface and the ICMP prober is an interface too, so the
health logic and the whole recovery ladder are tested without a Wi-Fi card:

```bash
go test ./...
```
