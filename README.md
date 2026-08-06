# RIPWS — Raw IP over WebSocket

L3の生IPパケットをWebSocketバイナリフレームでトンネリングする、Tailscale風味の極小VPN実装。仕様は [`SPEC.md`](SPEC.md) を参照。

- シングルバイナリ `server` / `client` / `gen-psk` / `gen-config` サブコマンド
- **Silent Drop** — 不正UUID・不正PSKは即座にTCP RST（`SO_LINGER=0`）、HTTPレスポンスなし
- 2つのL3バックエンドを `tunnel.Interface` で統一:
  - `kernel` — ホストTUNデバイス（`songgao/water`）、`CAP_NET_ADMIN` + `/dev/net/tun` 必要
  - `userspace` — gVisor netstackインプロセス、**権限不要・TUN不要**
- 双方向ゴルーチンプロキシパイプ（`TUN ↔ WS`）、`sync.Pool`バッファ、IPバージョンニブル検証
- WebSocket Ping/Pongキープアライブ（30–300秒、ジッター付き）、サーバー読み取りウィンドウ ≥ 最遅Ping
- 指数バックオフ + フルジッター再接続
- **Userspace NATゲートウェイ** — トンネル通過トランジットトラフィックをホスト側ネットワークへ透過的にNAT転送（`net.Dial`）、権限不要

---

## 目次

- [ビルド](#ビルド)
- [サブコマンド概要](#サブコマンド概要)
- [gen-psk](#gen-psk)
- [gen-config](#gen-config)
- [server](#server)
- [client](#client)
- [使用例](#使用例)
  - [1. カーネルモード（基本トンネル）](#1-カーネルモード基本トンネル)
  - [2. ユーザースペースモード（権限不要）](#2-ユーザースペースモード権限不要)
  - [3. ユーザースペース ICMP Ping](#3-ユーザースペース-icmp-ping)
  - [4. ユーザースペース TCP/UDP ポートフォワード](#4-ユーザースペース-tcpudp-ポートフォワード)
  - [5. ユーザースペース NATゲートウェイ](#5-ユーザースペース-natゲートウェイ)
  - [6. Cloudflare Tunnel経由 (wss://)](#6-cloudflare-tunnel経由-wss)
  - [7. 複数クライアント（JSON設定ファイル）](#7-複数クライアントjson設定ファイル)
  - [8. Echo プローブ（ユーザースペース疎通確認）](#8-echo-プローブユーザースペース疎通確認)
- [テストスクリプト](#テストスクリプト)
- [Dockerテスト](#dockerテスト)
- [プロジェクトレイアウト](#プロジェクトレイアウト)
- [依存ライブラリ](#依存ライブラリ)
- [テスト結果サマリ](#テスト結果サマリ)

---

## ビルド

### Nix flake（推奨）

```sh
nix build .#ripws          # -> result/bin/ripws (静的Goバイナリ, CGO無効)
nix develop                # go / gopls が使える開発シェル
```

`flake.nix` は `buildGoModule` を使用。gVisorバージョンは `go.mod` でピン留め。

> **注意:** Nix flakeはGit追跡済みファイルからビルドするため、新規ファイルは `git add` してから `nix build` すること。

### Goで直接ビルド

```sh
go build -o ripws .
```

---

## サブコマンド概要

```
ripws server     [flags]    RIPWSサーバーを起動
ripws client     [flags]    RIPWSクライアントを起動
ripws gen-psk               新規32バイトBase64 PSKを生成
ripws gen-config            新規UUIDに新規PSKを割り当てたJSONを生成
ripws help                  ヘルプを表示
```

---

## gen-psk

PSK（Pre-Shared Key）を生成。32バイトの乱数をBase64エンコードした44文字の文字列を出力。

```sh
ripws gen-psk
# 出力例: 8x/P9mZ3qF1bR6wN2vT4xK7jL0sH5aBcDeFgHiJk=
```

オプション: なし

---

## gen-config

UUIDv4を新規生成し、それに対応する新規PSKを割り当てたJSONを出力。サーバー設定ファイルの雛形として使用。

```sh
ripws gen-config
# 出力例:
# {
#   "f47ac10b-58cc-4372-a567-0e02b2c3d479": "8x/P9mZ3qF1bR6wN2vT4xK7jL0sH5aBcDeFgHiJk="
# }
```

オプション: なし

---

## server

RIPWSサーバーを起動。WebSocketアップグレード後にL3パケットのトンネル端点として動作。

### フラグ一覧

| フラグ | デフォルト | 説明 |
|--------|-----------|------|
| `--listen ADDR` | `:8080` | リッスンアドレス |
| `--config PATH` | (空) | UUID→PSKマッピングのJSONファイル（複数クライアント対応） |
| `--uuid UUID` | (空) | 単一クライアントのUUID（`--config`と併用可） |
| `--psk PSK` | (空) | 単一クライアントのPSK |
| `--mode kernel\|userspace` | `kernel` | L3バックエンド選択 |
| `--tun-name NAME` | (空) | kernel: TUNデバイス名のヒント（ベストエフォート） |
| `--us-addr IP[,IP...]` | (空) | userspace: netstack NICに割り当てる仮想IP（カンマ区切り複数可） |
| `--us-route CIDR[,CIDR..]` | `0.0.0.0/0,::/0` | userspace: トンネルNIC経由のルート（カンマ区切り複数可） |
| `--us-echo ADDR` | (空) | userspace: netstack内にTCP echoリスナーを起動（例: `10.8.0.1:7`） |
| `--ping-window D` | `330s` | クライアント生存判定の読み取りデッドライン（最遅Ping 300s以上） |

> `--config` と `--uuid`/`--psk` は併用可。両方指定した場合、フラグの認証情報が追加マージされる。最低1つの認証情報が必須。

### 認証ファイル形式

```json
{
  "client-a-uuid": "client-a-psk-base64",
  "client-b-uuid": "client-b-psk-base64"
}
```

---

## client

RIPWSクライアントを起動。WebSocketでサーバーに接続し、L3パケットを双方向にブリッジ。切断時は指数バックオフ + ジッターで再接続。

### フラグ一覧

| フラグ | デフォルト | 説明 |
|--------|-----------|------|
| `--url URL` | (必須) | `ws(s)://host:port/<uuid>` エンドポイント |
| `--psk PSK` | (必須) | Pre-Shared Key（Bearerトークン） |
| `--mode kernel\|userspace` | `kernel` | L3バックエンド選択 |
| `--tun-name NAME` | (空) | kernel: TUNデバイス名のヒント |
| `--tun-addr ADDR` | (空) | kernel: TUNに割り当てるアドレス（起動時に案内を表示。実際のIP割り当ては手動） |
| `--us-addr IP[,IP...]` | (空) | userspace: ローカル仮想IP（カンマ区切り複数可） |
| `--us-route CIDR[,CIDR..]` | `0.0.0.0/0,::/0` | userspace: トンネル経由ルート |
| `--us-dial ADDR` | (空) | userspace: ワンショットechoプローブ対象（例: `10.8.0.1:7`） |
| `--us-dial-interval D` | `5s` | echoプローブの繰り返し間隔 |
| `--us-ping IP` | (空) | userspace: ICMP ping送信先IP（例: `10.8.0.1`） |
| `--us-ping-count N` | `4` | pingプローブ回数 |
| `--us-ping-interval D` | `1s` | pingプローブ間隔 |
| `--us-forward L=R` | (空) | userspace: TCPポートフォワード `LOCAL=REMOTE`（例: `127.0.0.1:5201=10.8.0.1:5201`） |
| `--us-udp-forward L=R` | (空) | userspace: UDPポートフォワード `LOCAL=REMOTE` |
| `--us-nat` | `false` | userspace: NATゲートウェイ有効化（トンネル通過トランジットトラフィックをホストネットワークへプロキシ） |
| `--ping-interval D` | `60s` | WebSocket Ping送信間隔（30s〜300sの範囲） |
| `--ping-pong-timeout D` | `15s` | Pong待機時間（超過で切断判定） |
| `--backoff-base D` | `500ms` | 再接続の初期遅延 |
| `--backoff-max D` | `60s` | 再接続遅延の上限 |
| `--max-retries N` | `0` | 最大再接続回数（0 = 無制限） |

---

## 使用例

### 1. カーネルモード（基本トンネル）

最も基本的な構成。サーバー・クライアント両方でTUNデバイスを使用。`CAP_NET_ADMIN` と `/dev/net/tun` が必要。

```sh
# --- サーバー側 ---
sudo ripws server --listen :8080 --config creds.json --mode kernel
# 起動後、TUNデバイスにアドレスを割り当てる:
sudo ip addr add 10.7.0.1/24 dev tun0 && sudo ip link set up dev tun0

# --- クライアント側 ---
sudo ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode kernel \
    --ping-interval 30s
# 起動後、TUNデバイスにアドレスを割り当てる:
sudo ip addr add 10.7.0.2/24 dev tun0 && sudo ip link set up dev tun0

# 疎通確認
ping -c 4 10.7.0.1   # クライアント→サーバー
ping -c 4 10.7.0.2   # サーバー→クライアント
```

フラグで単一クライアントを直接指定することも可能:

```sh
sudo ripws server --listen :8080 --uuid <uuid> --psk <psk> --mode kernel
```

---

### 2. ユーザースペースモード（権限不要）

gVisor netstackを使用。root権限・TUNデバイス不要。サーバーとクライアント両方で利用可能。

```sh
# --- サーバー側（ユーザースペース + echoリスナー） ---
ripws server \
    --listen :8080 \
    --uuid <uuid> --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.1 \
    --us-echo 10.8.0.1:7

# --- クライアント側（ユーザースペース） ---
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24
```

> `--us-route` を `0.0.0.0/0`（デフォルト）にすると全トラフィックがトンネル経由になる。特定サブネットのみトンネルする場合は `--us-route 10.8.0.0/24` のように指定。

---

### 3. ユーザースペース ICMP Ping

gVisorのICMPエンドポイントを使用して、トンネル経由でpingを送信。権限不要。

```sh
# サーバー（kernel TUN 10.8.0.1/24 で稼働中と仮定）

# クライアント
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24 \
    --us-ping 10.8.0.1 \
    --us-ping-count 10 \
    --us-ping-interval 1s
```

出力例:
```
client: PING 10.8.0.1 (10 probes, 1s interval)
client: ping seq=0 rtt=689.324µs
client: ping seq=1 rtt=451.212µs
...
client: ping 10.8.0.1: 10 sent, 10 recv, 0% loss, min=312µs avg=690µs max=1.08ms
```

---

### 4. ユーザースペース TCP/UDP ポートフォワード

ローカルのリスナーポートを、トンネル先のアドレスにポートフォワード。iperf3等のツールをトンネル経由で利用可能。

```sh
# サーバー（kernel TUN 10.8.0.1/24, iperf3 -s -B 10.8.0.1 で稼働中と仮定）

# クライアント — TCP + UDP フォワード
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24 \
    --us-forward 127.0.0.1:5201=10.8.0.1:5201 \
    --us-udp-forward 127.0.0.1:5201=10.8.0.1:5201 \
    --us-ping 10.8.0.1 --us-ping-count 10

# 別ターミナルから iperf3
iperf3 -c 127.0.0.1 -p 5201 -t 10          # TCP c2s
iperf3 -c 127.0.0.1 -p 5201 -t 10 -R       # TCP s2c (reverse)
iperf3 -c 127.0.0.1 -p 5201 -t 10 -P 8     # TCP 8並列
iperf3 -c 127.0.0.1 -p 5201 -t 10 -u -b 200M   # UDP 200Mbps
```

---

### 5. ユーザースペース NATゲートウェイ

クライアントをNATゲートウェイとして動作させ、サーバーからのトランジットトラフィックをクライアント経由でインターネットへ転送。クライアント側は権限不要。

```
サーバー (kernel TUN 10.7.0.1/24)
    │  ip route add 1.1.1.1/32 8.8.8.8/32 via 10.7.0.2
    │  WebSocket ws://server:8080/<uuid>
クライアント (userspace netstack 10.7.0.2, --us-nat)
    │  gVisor TCP/UDP Forwarder → net.Dial → インターネット
インターネット (1.1.1.1, 8.8.8.8, ...)
```

```sh
# --- クライアント（NATゲートウェイ、権限不要） ---
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.7.0.2 \
    --us-route 10.7.0.0/24 \
    --us-nat

# --- サーバー側（kernel TUN 10.7.0.1/24） ---
# 外部IPへのルートをクライアント経由に設定
sudo ip route add 1.1.1.1/32 via 10.7.0.2
sudo ip route add 8.8.8.8/32 via 10.7.0.2

# サーバーからインターネットへ（トンネル→クライアントNAT→インターネット）
curl http://1.1.1.1/                          # TCP 80
curl https://1.1.1.1/                         # TCP 443 (TLS)
dig @8.8.8.8 example.com                      # UDP 53 (DNS)
curl http://example.com/                      # TCP (DNS + HTTP)
```

> NATゲートウェイはgVisorのTCP/UDP Forwarderで受信したパケットを `net.Dial` でホストのカーネルネットワークへブリッジする。クライアントに `CAP_NET_ADMIN` は不要。

---

### 6. Cloudflare Tunnel経由 (wss://)

Cloudflare Tunnel (`cloudflared`) をリバースプロキシとして使用。`wss://` でTLS暗号化 + CDN透過。

```sh
# --- サーバー（Dockerコンテナ、kernel TUN 10.7.0.1/24） ---
# localhost:8080 をCloudflare Tunnelがリバースプロキシする構成
sudo ripws server --listen :8080 --config creds.json --mode kernel
sudo ip addr add 10.7.0.1/24 dev tun0 && sudo ip link set up dev tun0
# iperf3をトンネルIPにバインド
iperf3 -s -p 5201 -B 10.7.0.1

# --- cloudflared（ホスト上で実行） ---
cloudflared tunnel --no-autoupdate --protocol http2 run --token <CF_TOKEN>
# ※ --protocol http2 必須（QUIC datagram handlerが一部環境で失敗するため）

# --- クライアント（ホスト上、ユーザースペースモード） ---
sudo ripws client \
    --url "wss://ripws-test.example.com/<uuid>" \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.7.0.2 \
    --us-route 10.7.0.0/24 \
    --us-ping 10.7.0.1 --us-ping-count 100 --us-ping-interval 0.1s \
    --us-forward 127.0.0.1:5201=10.7.0.1:5201 \
    --us-udp-forward 127.0.0.1:5201=10.7.0.1:5201 \
    --ping-interval 30s

# iperf3（Cloudflare経由）
iperf3 -c 127.0.0.1 -p 5201 -t 15 -P 8       # TCP 8並列
iperf3 -c 127.0.0.1 -p 5201 -t 15 -u -b 500M  # UDP 500Mbps
```

カーネルモードでのCloudflare経由も可能:

```sh
sudo ripws client \
    --url "wss://ripws-test.example.com/<uuid>" \
    --psk <psk> \
    --mode kernel \
    --ping-interval 30s
sudo ip addr add 10.7.0.2/24 dev tun0 && sudo ip link set up dev tun0
iperf3 -c 10.7.0.1 -p 5201 -B 10.7.0.2
```

> **Silent Drop + Cloudflare:** 不正UUID/PSKのプローブはオリジンでRSTされるが、CloudflareはこれをHTTP 502としてクライアントに返す。オリジン自体はHTTPレスポンスを返さない。

---

### 7. 複数クライアント（JSON設定ファイル）

```sh
# 複数のUUID-PSKペアをJSONファイルで管理
ripws gen-config > creds.json
# creds.json に手動でエントリを追加:
# {
#   "uuid-a": "psk-a",
#   "uuid-b": "psk-b",
#   "uuid-c": "psk-c"
# }

# サーバーは1つで複数クライアントを受け入れる
sudo ripws server --listen :8080 --config creds.json --mode kernel

# 各クライアントは自身のUUID/PSKで接続
sudo ripws client --url ws://SERVER:8080/uuid-a --psk psk-a --mode kernel
sudo ripws client --url ws://SERVER:8080/uuid-b --psk psk-b --mode kernel
```

`--config` と `--uuid`/`--psk` の併用も可能:

```sh
# JSONファイル + フラグ追加
sudo ripws server --config creds.json --uuid <extra-uuid> --psk <extra-psk>
```

---

### 8. Echo プローブ（ユーザースペース疎通確認）

サーバー側にgVisor内のTCP echoリスナーを起動し、クライアントから定期的にプローブ:

```sh
# サーバー（userspace, echoリスナー付き）
ripws server \
    --listen :8080 \
    --uuid <uuid> --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.1 \
    --us-echo 10.8.0.1:7

# クライアント（userspace, 定期プローブ）
ripws client \
    --url ws://SERVER:8080/<uuid> \
    --psk <psk> \
    --mode userspace \
    --us-addr 10.8.0.2 \
    --us-route 10.8.0.0/24 \
    --us-dial 10.8.0.1:7 \
    --us-dial-interval 2s
```

出力例:
```
client: probe 10.8.0.1:7 OK rtt=1.2ms
client: probe 10.8.0.1:7 OK rtt=0.9ms
```

---

## テストスクリプト

| スクリプト | 内容 |
|-----------|------|
| `scripts/test.sh` | カーネルモード L3 ping + Silent Drop（Docker 2コンテナ） |
| `scripts/test-userspace.sh` | ユーザースペース echo プーブ（両側gVisor） |
| `scripts/test-us-direct.sh` | ユーザースペース直接接続ベンチマーク（client userspace + server kernel、iperf3 + ping） |
| `scripts/test-nat.sh` | NATゲートウェイテスト（server→tunnel→client NAT→internet、TCP/UDP/DNS） |
| `scripts/cloudflare-load-test.sh` | Cloudflare経由カーネルモード負荷テスト（iperf3 + 1000 ping） |
| `scripts/cloudflare-us-test.sh` | Cloudflare経由ユーザースペーステスト（iperf3 + 100 ping） |

---

## Dockerテスト

### カーネルモード（L3 ping, Silent Drop）

```
ripws-client (tun0 10.7.0.2/24)  --ws-->  ripws-server (tun0 10.7.0.1/24)
```

両コンテナとも `--cap-add=NET_ADMIN --device=/dev/net/tun` で実行。`scripts/entrypoint.sh` が `ripws` 起動後に `tun0` にアドレスを割り当てる。

```sh
bash scripts/test.sh
```

期待結果: `ping -c 4 10.7.0.1`（クライアント→サーバー）と `ping -c 4 10.7.0.2`（サーバー→クライアント）が成功。不正UUID/PSKのプローブは `Connection reset by peer`（HTTPステータス行なし）。

### ユーザースペース直接ベンチマーク

```
ripws-client (userspace 10.8.0.2)  --ws-->  ripws-server (tun0 10.8.0.1/24, iperf3 -B 10.8.0.1)
```

```sh
bash scripts/test-us-direct.sh
```

### NATゲートウェイ

```
ripws-server (tun0 10.7.0.1/24) → tunnel → ripws-client (userspace 10.7.0.2, --us-nat) → internet
```

```sh
bash scripts/test-nat.sh
```

### Cloudflare経由

```sh
CF_TUNNEL_TOKEN=<token> bash scripts/cloudflare-load-test.sh    # kernel mode
CF_TUNNEL_TOKEN=<token> bash scripts/cloudflare-us-test.sh      # userspace mode
```

### 手動Docker操作

```sh
nix build .#ripws
mkdir -p bin && cp -L result/bin/ripws bin/ripws
sudo docker compose build

# 認証情報生成
CRED="$(nix run .#ripws -- gen-config)"
UUID="$(printf '%s' "$CRED" | jq -r 'keys[0]')"
PSK="$(printf '%s' "$CRED" | jq -r ".[\"$UUID\"]")"
mkdir -p config && printf '%s\n' "$CRED" > config/server.json

export RIPWS_UUID="$UUID" RIPWS_PSK="$PSK"
sudo -E docker compose up -d server client

# 検証
sudo docker exec ripws-client ping -c 3 10.7.0.1
sudo docker exec ripws-server ping -c 3 10.7.0.2

# Silent Drop確認（RST, ステータス行なし）
sudo docker exec ripws-client curl -i --max-time 5 \
  http://server:8080/00000000-0000-0000-0000-000000000000 \
  -H 'Authorization: Bearer bad'
```

---

## プロジェクトレイアウト

```
main.go                      flagベース server/client/gen-* ディスパッチ
flake.nix                    buildGoModule（静的バイナリ）
internal/
  config/                    JSON + flag認証情報、PSKバリデーション (SPEC 2.2)
  proto/                     IPバージョンニブル、キープアライブ、指数バックオフ
  tunnel/
    tunnel.go                Interface 抽象契約
    tun_linux.go             kernel TUN バックエンド (songgao/water)
    netstack.go              gVisor userspace バックエンド (netstack)
    ping.go                  userspace ICMP ping (gVisor ICMP endpoint)
    forward.go               userspace TCP/UDP ポートフォワーダ
    nat.go                   userspace NATゲートウェイ (gVisor Forwarder → net.Dial)
  pipe/                      双方向 TUN↔WS プロキシ + バッファプール
  server/                    Silent Drop (hijack + SO_LINGER=0) + WS upgrade
  client/                    dial + reconnect + jittered ping
scripts/                     entrypoint.sh, test.sh, test-*.sh, cloudflare-*.sh
Dockerfile, docker-compose*.yml
```

---

## 依存ライブラリ

- `github.com/gorilla/websocket` — WebSocket（`http.Hijacker`アクセスでSilent Dropが自然に実装可能）
- `github.com/songgao/water` — Linux TUN
- `gvisor.dev/gvisor` — userspace netstack（ピン留め、`go.mod` + Nix vendor hash）
- `github.com/google/uuid` — UUIDv4生成・検証

---

## テスト結果サマリ

全テスト結果は [`REPORT.md`](REPORT.md) を参照。

| カテゴリ | 結果 |
|----------|------|
| L3接続性（kernel） | PASS |
| Silent Drop セキュリティ | PASS |
| Userspace (gVisor) 基本 | PASS |
| iperf3スループット（kernel） | PASS（1.6+ Gbps） |
| マルチポート同時接続 | PASS（32/32） |
| 再接続・KeepAlive | PASS |
| レース検出器 | PASS |
| Cloudflare tunnel（kernel） | PASS（wss://, 双方向, RTT ~232ms） |
| Userspace ping + iperf（直接） | PASS（0% loss, 2.0 Gbps -P8） |
| Userspace NATゲートウェイ | PASS（TCP/UDP/DNS, tunnel → internet） |
| Cloudflare tunnel（userspace） | PASS（wss://, 0% ping loss, UDP 499 Mbps 0% loss） |
