# wg6pn

Join a Fly.io organization's private network (6PN) from a sprite, using a sprites connector so no sprite ever holds a Fly token.

Sprites get no Machine identity features and no Flycast. They cannot reach `.internal` names, and Machines cannot reach them privately. wg6pn makes the sprite a named WireGuard peer of the org:

- **Inbound:** Machines connect to `<sprite>._peer.internal`, and configured ports are forwarded to `127.0.0.1` in the sprite.
- **Outbound:** a loopback SOCKS5 proxy dials through the tunnel. Use `socks5h://` so `.internal` names resolve on the 6PN.
- **Status:** `GET http://127.0.0.1:9180/` returns JSON, and `/healthz` returns 503 while the tunnel is down.

WireGuard runs in userspace over a websocket on 443, so it needs no TUN device and no root.

## How it uses the connector

1. On first boot the sprite generates a WireGuard key.
2. It calls Fly's `addWireGuardPeer` GraphQL mutation at the connector's URL, sending only its public key and its name.
3. The sprites gateway identifies the sprite, checks the connector's access policy, adds the token, and forwards the request to `api.fly.io`.
4. The sprite saves the returned peer details next to its key. Every later connection uses only the key, so the token is never needed again.

## Connector setup

Create a custom-API connector in the sprites org:

| Field | Value |
|---|---|
| Base URL | `https://api.fly.io` |
| Auth header prefix | `FlyV1` |
| Token | output of `fly tokens create wireguard --org my-org --expiry 720h` |
| Access policy | a sprite label such as `6pn`, or `allow_all` |

A WireGuard token can manage the org's WireGuard peers and read org metadata, so admit only sprites you would trust on the org network.

## Identity

The saved peer is bound to the sprite's identity: `FLY_COMPUTE_ID`, else `SPRITE_NAME`, else the hostname. If wg6pn starts with state saved under a different identity, it generates a new key and registers itself under its own name. The peer name comes from `WG6PN_PEER_NAME`, else `SPRITE_NAME`, else the hostname.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `WG6PN_ORG` | required | org slug, not `personal` |
| `WG6PN_REGISTER_URL` | required | the connector's GraphQL URL |
| `WG6PN_PEER_NAME` | sprite name | WireGuard peer name |
| `WG6PN_REGION` | API default | gateway region |
| `WG6PN_NETWORK` | org default | custom 6PN name |
| `WG6PN_KEY_FILE` | `~/.config/wg6pn/key` | WireGuard private key, mode 600 |
| `WG6PN_STATE_FILE` | next to the key | registered peer |
| `WG6PN_FORWARD` | none | `8080`, `8080:3000`; separate with `,` or `;` |
| `WG6PN_PROXY` | `127.0.0.1:1080` | SOCKS5 listener, loopback only, or `off` |
| `WG6PN_STATUS` | `127.0.0.1:9180` | status listener, loopback only, or `off` |

## Build and deploy

```bash
nix build              # static binary in result/bin/wg6pn
go test -race ./...

SPRITE=my-sprite ORG=my-org CONNECTOR=<connection-id> ./deploy.sh
```

`deploy.sh` also takes `CREATE=1`, `FORWARD`, `NETWORK` and `SPRITES_ORG`.

## Using it

```bash
# from the sprite
curl --proxy socks5h://127.0.0.1:1080 http://my-app.internal:8080/

# from a Machine in the org
curl http://my-sprite._peer.internal:8080/
```

## Known gaps

- **Peers outlive sprites.** Remove a destroyed sprite's peer with
  `fly wireguard remove`.
- **No caller identity.** Anything on the 6PN can reach forwarded ports.
- **Sleep.** The tunnel only exists while the sprite runs.

