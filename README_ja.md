# RIPWS — Raw IP over WebSocket

L3の生IPパケットをWebSocketバイナリフレームでトンネリングする、Tailscale風味の極小VPN実装。仕様は [`SPEC.md`](SPEC.md) を参照。

- シングルバイナリ `up` / `down` / `genkey` / `genuuid` サブコマンド
- **wg-quick互換設定** — `ripws up ws0.conf` でTUN作成・アドレス割当・MTU・ルート・DNS・PreUp/PostUpフックを全自動設定
- **Silent Drop** — 不正UUID・不正PSKは即座にTCP RST（`SO_LINGER=0`）、HTTPレスポンスなし
- 2つのL3バックエンドを `tunnel.Interface` で統一:
  - `kernel` — ホストTUNデバイス（`songgao/water`）、`CAP_NET_ADMIN` + `/dev/net/tun` 必要（wg-quick互換）
  - `userspace` — gVisor netstackインプロセス、**権限不要・TUN不要**（Tailscale互換）
- **Userspace NATゲートウェイ** — トンネル通過トランジットトラフィックをホスト側ネットワークへ透過的にNAT転送（`net.Dial`）、権限不要
- **Split-Dataplane `[ProxyRule]`** — 指定宛先CIDRだけをカーネルTUNを迂回してインプロセスでL4終端し、HTTP `CONNECT` / SOCKS5プロキシへ送出。TLS SNI抽出で企業プロキシのドメインACL・監査ログとも共存。`Mode = kernel` の既存性能（SSH/ICMP/`10.x`）は一切壊さない
- 双方向ゴルーチンプロキシパイプ（`TUN ↔ WS`）、`sync.Pool`バッファ、IPバージョンニブル検証
- WebSocket Ping/Pongキープアライブ（30–300秒、ジッター付き）、サーバー読み取りウィンドウ ≥ 最遅Ping
- 指数バックオフ + フルジッター再接続

---

## 目次

- [ビルド](#ビルド)
- [サブコマンド概要](#サブコマンド概要)
- [クイックスタート](#クイックスタート)
- [設定ファイル形式](#設定ファイル形式)
- [NixOSモジュール](#nixosモジュール)
- [genkey](#genkey)
- [genuuid](#genuuid)
- [使用例](#使用例)
  - [1. カーネルモード（基本トンネル）](#1-カーネルモード基本トンネル)
  - [2. ユーザースペースモード（権限不要）](#2-ユーザースペースモード権限不要)
  - [3. ユーザースペース NATゲートウェイ](#3-ユーザースペース-natゲートウェイ)
  - [4. Cloudflare Tunnel経由 (wss://)](#4-cloudflare-tunnel経由-wss)
  - [5. 複数クライアント（個別PSK）](#5-複数クライアント個別psk)
  - [6. Split-Dataplane: 指定CIDRのみ社内プロキシ経由](#6-split-dataplane-指定cidrのみ社内プロキシ経由)
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

> **注意:** Nix flakeはGit追跡済みファイルからビルドするため、変更をコミットしてから `nix build` すること。

### Goで直接ビルド

```sh
CGO_ENABLED=0 go build -o ripws .
```

---

## サブコマンド概要

```
ripws up   <config.conf>  トンネルを起動（TUN/netstack作成・アドレス・MTU・ルート・DNS・フック全自動）
ripws down <iface|conf>   実行中のトンネルを停止（SIGTERM + クリーンアップ）
ripws genkey              新規32バイトBase64 PSKを生成（WireGuard風命名）
ripws genuuid             新規UUIDv4を生成（クライアント識別子）
ripws help                ヘルプを表示
```

---

## クイックスタート

`ripws up` / `ripws down` はwg-quick互換のINI設定ファイルを使って、インターフェース作成からアドレス割当・MTU設定・ルート追加・DNS注入・PreUp/PostUpフックまで全て自動で行います。

```sh
# 認証情報生成
UUID="$(ripws genuuid)"
PSK="$(ripws genkey)"

# --- サーバー設定 (/etc/ripws/ws0.conf) ---
cat > /tmp/ws0.conf << EOF
[Interface]
Address    = 10.7.0.1/24
ListenPort = 8080
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
AllowedIPs = 10.7.0.2/32
EOF

# --- クライアント設定 ---
cat > /tmp/ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 30
EOF

# 起動
sudo ripws up /tmp/ws0.conf        # インターフェース名はファイル名から自動決定 (ws0.conf → ws0)

# 停止
sudo ripws down /tmp/ws0.conf
```

> `/etc/ripws/ws0.conf` に置けば `ripws up ws0` / `ripws down ws0` と省略可能。

---

## 設定ファイル形式

INI形式で `[Interface]` セクションと複数の `[Peer]` セクションを記述します。

### [Interface] セクション

| フィールド | 必須 | 説明 |
|-----------|------|------|
| `Address` | ✓ | インターフェースに割り当てるCIDRアドレス（カンマ区切り複数可） |
| `ListenPort` | serverのみ | サーバーのリッスンポート。指定時はサーバーモードで動作 |
| `PrivateKey` | ✓ | デフォルトPSK（base64 44文字）。`PresharedKey` でpeer個別に上書き可 |
| `MTU` | (デフォルト1420) | インターフェースMTU |
| `Mode` | (デフォルト `kernel`) | `kernel`（TUN、CAP_NET_ADMIN必要）または `userspace`（gVisor netstack、権限不要） |
| `NAT` | (デフォルト `false`) | userspaceのみ: NATゲートウェイ有効化（トンネル通過トラフィックをホストネットワークへ転送） |
| `DNS` | (省略可) | `/etc/resolv.conf` に注入するDNSサーバー（カンマ区切り複数可） |
| `Table` | (デフォルト `auto`) | `off` でルート追加をスキップ |
| `PreUp` | (省略可) | インターフェース起動前に実行するシェルコマンド（複数行可） |
| `PostUp` | (省略可) | インターフェース起動後に実行するシェルコマンド（複数行可） |
| `PreDown` | (省略可) | インターフェース停止前に実行するシェルコマンド（複数行可） |
| `PostDown` | (省略可) | インターフェース停止後に実行するシェルコマンド（複数行可） |

### [Peer] セクション

| フィールド | 必須 | 説明 |
|-----------|------|------|
| `UUID` | ✓ | peerのUUIDv4 |
| `PresharedKey` | (省略可) | peer個別のPSK。指定時は `PrivateKey` を上書き |
| `AllowedIPs` | ✓ | このpeer経由のルート（CIDR、カンマ区切り複数可） |
| `Endpoint` | clientのみ | `ws(s)://host:port` サーバーURL（UUIDは自動でパスに追加） |
| `PersistentKeepalive` | (省略可) | WebSocket Ping間隔（秒、30〜300） |

### [ProxyRule] セクション（複数回記述可）

宛先IPが `CIDRs` に一致するパケットはカーネルTUNを一切経由せず、インプロセス（gVisor netstack）でL4終端され、上部プロキシへ新しいTCPコネクションとして転送されます。それ以外（`AllowedIPs` ルート、SSH、ICMP、`10.x` トランジット）は従来通りカーネル経路のまま一切壊れません。`Mode = kernel` / `userspace` 両方で動作します。

| フィールド | 必須 | 説明 |
|-----------|------|------|
| `CIDRs` | ✓ | 遮断対象の宛先CIDR（カンマ区切り、IPv4/IPv6） |
| `Proxy` | ✓ | `http://` `https://` `socks5://` `socks5h://` URL（`user:pass@` 可） |
| `SNI` | (デフォルト `false`) | TLS ClientHelloを先読みし `CONNECT <dstIP>` ではなく `CONNECT <sni>:<port>` を使用 |

```ini
[ProxyRule]
CIDRs = 20.20.32.0/19,20.190.128.0/18,40.126.0.0/18
Proxy = http://proxy.corp:8080
SNI   = true
```

ルールが複数の場合、**最長プレフィックス一致**が優先（同一長は先勝ち）。注意:

- 一致CIDR宛の **UDP** は即時拒否（HTTPプロキシはTCPのみ。QUICクライアントはハングせずTCPへフォールバック）。
- `SNI = true` でも、非TLS・SNIなし・先読みタイムアウト（500ms）は `CONNECT <dstIP>:<port>` へフォールバック。
- プロキシ自身のIPを `CIDRs` に入れてはいけません（自身へループする）。プロキシへの接続はホストの通常経路で行われます。

### WireGuardとの対応関係

| WireGuard | RIPWS | 備考 |
|-----------|-------|------|
| `PrivateKey` | PSK | 認証トークン（Bearer） |
| `PublicKey` | `UUID` | クライアント識別子（公開鍵ではなく秘匿対象、誤解を避けるためUUIDと命名） |
| `PresharedKey` | per-peer PSK | peer個別のPSK上書き |
| `Endpoint` | ws(s):// URL | WebSocketエンドポイント |
| `ListenPort` | listen port | サーバーモード判定 |
| `AllowedIPs` | routes | トンネル経由ルート |
| `PersistentKeepalive` | Ping間隔 | WebSocket Ping (30〜300秒) |
| `MTU`, `DNS`, `Table`, `PreUp/PostUp/PreDown/PostDown` | 同名同機能 | wg-quick互換 |

### サーバー・クライアント判定

- `ListenPort` が指定されている → **サーバー**（HTTPサーバーを起動）
- 指定されていない → **クライアント**（最初の `[Peer]` の `Endpoint` に接続）

### カーネル・ユーザースペースモード切替

- `Mode = kernel`（デフォルト）→ ホストTUNデバイスを使用、`CAP_NET_ADMIN` + `/dev/net/tun` 必要
- `Mode = userspace` → gVisor netstackをインプロセスで使用、権限不要

### インターフェース名の決定

設定ファイルのベース名から `.conf` を取り除いたものがインターフェース名になります:
- `ws0.conf` → `ws0`
- `/etc/ripws/tunnel1.conf` → `tunnel1`

---

## NixOSモジュール

`nix/module.nix` は `services.ripws.tunnels.<name>` による宣言的（wg-quick風）デプロイを提供します。attr名は systemd ユニット名と TUN インターフェース名の両方に使われます。トンネルごとに `ripws-<name>` フォアグラウンドサービス（`Type=simple`、`Restart=always`、SIGTERMでクリーンシャットダウン、`network-online.target` 待ち）が生成されます。

```nix
# flake inputs: ripws.url = "github:kenh0u/ripws";
{ inputs, ... }: {
  imports = [ inputs.ripws.nixosModules.default ];

  services.ripws.tunnels.server = {
    address        = [ "10.7.0.1/24" ];
    listenPort     = 8080;                       # 設定するとサーバー側
    privateKeyFile = "/run/secrets/ripws.key";   # 44文字Base64 PSK
    peers = [
      { uuid = "…client-uuid…"; allowedIPs = [ "10.7.0.2/32" ]; }
    ];
  };
}
```

クライアント側（`listenPort` なし、peerにendpoint）:

```nix
services.ripws.tunnels.client = {
  address        = [ "10.7.0.2/24" ];
  privateKeyFile = "/run/secrets/ripws.key";
  peers = [
    {
      uuid       = "…same-uuid…";
      endpoint   = "ws://server.example:8080";   # wss://も可
      allowedIPs = [ "0.0.0.0/0" ];              # 全トラフィック経由
    }
  ];
  # 企業NAT/プロキシ越え（環境変数駆動）:
  proxyEnvironment = { ALL_PROXY = "socks5h://proxy.corp:1080"; };
};
```

### オプション一覧

| オプション | 既定値 | 設定ファイルのキー |
|---|---|---|
| `mode` | `"kernel"` | `Mode` |
| `address` | 必須 | `Address` |
| `listenPort` | `null`（クライアント） | `ListenPort` |
| `privateKey` / `privateKeyFile` | `null` | `PrivateKey` |
| `mtu` | ripws既定（1420） | `MTU` |
| `dns` | `[ ]` | `DNS` |
| `table` | `"auto"` | `Table` |
| `nat` | `false` | `NAT`（userspace） |
| `peers[].uuid` | 必須 | `UUID` |
| `peers[].presharedKey` / `…File` | `null` | `PresharedKey` |
| `peers[].allowedIPs` | `[ ]` | `AllowedIPs` |
| `peers[].endpoint` | `null` | `Endpoint` |
| `peers[].persistentKeepalive` | `30` | `PersistentKeepalive` |
| `preStart` / `postStart` | `""` | systemdフック |
| `autoStart` | `true` | ブート時有効化 |
| `proxyEnvironment` | `{ }` | ユニットの `Environment` |
| `proxyRules[].cidrs` | — | `[ProxyRule]` の `CIDRs` |
| `proxyRules[].proxy` | — | `[ProxyRule]` の `Proxy` |
| `proxyRules[].sni` | `false` | `[ProxyRule]` の `SNI` |
| `extraPackages` | `[ ]` | サービスのPATH |

### 秘密情報の取り扱い

- `privateKeyFile` / `presharedKeyFile`: 中身は**Nixストアに一切書き込まれません**。起動スクリプトにはプレースホルダトークンだけが存在し、`ExecStart` 時に `/run/ripws/<name>.conf`（ディレクトリ0700・ファイル0600・rootのみ）へ置換されます。未解決のプレースホルダがあれば起動を中止します。
- `privateKey` / `presharedKey`（インライン）: 設定へ直接展開されます。利便性・テスト用のみ推奨で、値はストア内で全ユーザーから読める状態になります。

### 検証

- PSK（Base64・44文字）とUUIDv4の形式はオプション型で検査されます。
- アサーション: サーバーには鍵が必須、クライアントにはendpointを持つpeerが1つ以上必要、`privateKey` と `privateKeyFile` は排他。

### テスト

```sh
nix build .#checks.x86_64-linux.ripws-eval   # 純評価テスト: ユニット生成・秘密
                                            # 置換・アサーション（数秒）
nix build .#checks.x86_64-linux.ripws-vm     # NixOS VMテスト: kernel+userspace
                                            # 疎通・再起動・後始末（要/dev/kvm）
```

VMテスト（`nix/vm-test.nix`）は2台のNixOSゲストに6トンネル（kernel↔kernel、userspaceサーバー↔kernelクライアント、`proxyRules` 付きsplit-dataplaneクライアント）を起動し、`/run/ripws` の設定レンダリングと権限、双方向ping、モック社内プロキシ（ドメイン許可・IPリテラル拒否）に対するSNI CONNECTプロキシ経路、UDP遮断、カーネル経路の不変性、クライアント再起動時の再接続とインターフェース後始末、サーバー再起動後の回復を検証します。

---

## genkey

PSK（Pre-Shared Key）を生成。32バイトの乱数をBase64エンコードした44文字の文字列を出力。WireGuard風の命名（`wg genkey` → `ripws genkey`）。

```sh
ripws genkey
# 出力例: 8x/P9mZ3qF1bR6wN2vT4xK7jL0sH5aBcDeFgHiJk=
```

---

## genuuid

UUIDv4を新規生成。WebSocketのURLパスおよび `[Peer]` の `UUID` に使用するクライアント識別子。

```sh
ripws genuuid
# 出力例: f47ac10b-58cc-4372-a567-0e02b2c3d479
```

---

## 使用例

### 1. カーネルモード（基本トンネル）

最も基本的な構成。サーバー・クライアント両方でTUNデバイスを使用。`CAP_NET_ADMIN` と `/dev/net/tun` が必要。

```sh
# 認証情報生成
UUID="$(ripws genuuid)"
PSK="$(ripws genkey)"

# --- サーバー設定 (/etc/ripws/ws0.conf) ---
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.7.0.1/24
ListenPort = 8080
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
AllowedIPs = 10.7.0.2/32
EOF

# --- クライアント設定 ---
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 30
EOF

# 起動
sudo ripws up ws0.conf        # サーバー・クライアント両方で

# 疎通確認
ping -c 4 10.7.0.1   # クライアント→サーバー
ping -c 4 10.7.0.2   # サーバー→クライアント

# 停止
sudo ripws down ws0.conf
```

---

### 2. ユーザースペースモード（権限不要）

gVisor netstackを使用。root権限・TUNデバイス不要。

```sh
# --- サーバー設定（カーネルモード、TUN使用） ---
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.8.0.1/24
ListenPort = 8080
PrivateKey = $PSK
Mode       = kernel
MTU        = 1420

[Peer]
UUID  = $UUID
AllowedIPs = 10.8.0.2/32
EOF
sudo ripws up ws0.conf

# --- クライアント設定（ユーザースペース、権限不要） ---
cat > ws0.conf << EOF
[Interface]
Address    = 10.8.0.2/24
PrivateKey = $PSK
Mode       = userspace
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
AllowedIPs = 10.8.0.0/24
PersistentKeepalive = 30
EOF
ripws up ws0.conf          # sudo不要
```

> ユーザースペースモードでは `AllowedIPs` でgVisor netstack経由のCIDRを指定します。`0.0.0.0/0` で全トラフィック、`10.8.0.0/24` で特定サブネットのみトンネル。

---

### 3. ユーザースペース NATゲートウェイ

クライアントをNATゲートウェイとして動作させ、サーバーからのトランジットトラフィックをクライアント経由でインターネットへ転送。クライアント側は権限不要。

```
サーバー (kernel TUN 10.7.0.1/24)
    │  ip route add 1.1.1.1/32 8.8.8.8/32 via 10.7.0.2
    │  WebSocket ws://server:8080
クライアント (userspace netstack 10.7.0.2, NAT = true)
    │  gVisor TCP/UDP Forwarder → net.Dial → インターネット
インターネット (1.1.1.1, 8.8.8.8, ...)
```

```sh
# --- クライアント設定（NATゲートウェイ、権限不要） ---
cat > ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
Mode       = userspace
NAT        = true
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = ws://SERVER:8080
AllowedIPs = 10.7.0.0/24
PersistentKeepalive = 30
EOF
ripws up ws0.conf

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

### 4. Cloudflare Tunnel経由 (wss://)

Cloudflare Tunnel (`cloudflared`) をリバースプロキシとして使用。`wss://` でTLS暗号化 + CDN透過。

```sh
# --- サーバー（Dockerコンテナ、kernel TUN 10.7.0.1/24） ---
# localhost:8080 をCloudflare Tunnelがリバースプロキシする構成
sudo ripws up ws0.conf
# iperf3をトンネルIPにバインド
iperf3 -s -p 5201 -B 10.7.0.1

# --- cloudflared（ホスト上で実行） ---
cloudflared tunnel --no-autoupdate --protocol http2 run --token <CF_TOKEN>
# ※ --protocol http2 必須（QUIC datagram handlerが一部環境で失敗するため）

# --- クライアント（ホスト上、ユーザースペースモード） ---
cat > ws0.conf << EOF
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
Mode       = userspace
MTU        = 1420

[Peer]
UUID  = $UUID
Endpoint   = wss://ripws-test.example.com
AllowedIPs = 10.7.0.0/24
PersistentKeepalive = 30
EOF
ripws up ws0.conf

# iperf3（Cloudflare経由）
iperf3 -c 10.7.0.1 -p 5201 -t 15 -P 8       # TCP 8並列
iperf3 -c 10.7.0.1 -p 5201 -t 15 -u -b 500M  # UDP 500Mbps
```

カーネルモードでのCloudflare経由も可能 — クライアント設定で `Mode = kernel` を指定し、`ripws up` 後にTUNアドレスを手動割り当て。

> **Silent Drop + Cloudflare:** 不正UUID/PSKのプローブはオリジンでRSTされるが、CloudflareはこれをHTTP 502としてクライアントに返す。オリジン自体はHTTPレスポンスを返さない。

---

### 5. 複数クライアント（個別PSK）

サーバーの `PrivateKey`（PSK）が全peerのデフォルト。peer個別の `PresharedKey` で異なるPSKを指定可能。

```sh
# 複数のUUID-PSKペアを生成
UUID_A="$(ripws genuuid)"; PSK_A="$(ripws genkey)"
UUID_B="$(ripws genuuid)"; PSK_B="$(ripws genkey)"

# 複数peerを持つサーバー設定
cat > /etc/ripws/ws0.conf << EOF
[Interface]
Address    = 10.7.0.1/24
ListenPort = 8080
PrivateKey = $DEFAULT_PSK
MTU        = 1420

[Peer]
UUID  = $UUID_A
PresharedKey = $PSK_A
AllowedIPs = 10.7.0.2/32

[Peer]
UUID  = $UUID_B
PresharedKey = $PSK_B
AllowedIPs = 10.7.0.3/32
EOF

sudo ripws up ws0.conf

# 各クライアントは自身のUUID/PSKで接続
# クライアントA: Endpoint = ws://SERVER:8080, UUID = $UUID_A, PrivateKey = $PSK_A
# クライアントB: Endpoint = ws://SERVER:8080, UUID = $UUID_B, PrivateKey = $PSK_B
```

---

### 6. Split-Dataplane: 指定CIDRのみ社内プロキシ経由

送出制限の強い環境（例: 社内VPN終端がSquidプロキシ経由でしか外部に出られない）向けの定石構成。`ripws` クライアントは **kernel** モードのままSSH/ICMP/`10.x` トランジットのフルスピードを維持し、Microsoft 365系のグローバル帯だけインプロセスで遮断して `CONNECT` でプロキシへ流します。TLS SNI を抽出して実ホスト名で `CONNECT` するため、プロキシ側のドメインACL・監査ログもそのまま機能します。

```
サーバー (kernel TUN、MS帯をトンネルへルート)
   │  AllowedIPs = 10.7.0.2/32, 20.20.32.0/19, 40.126.0.0/18
   │  WebSocket ws://server:8080
クライアント (kernel TUN + split proxy rules)
   ├─ 10.x / SSH / ICMP ─────────────▶ カーネルTUN（従来通り）
   └─ 20.20.32.0/19, 40.126.0.0/18 ──▶ インプロセス netstack
                                         └─ TLS SNI 抽出 → CONNECT <host>:443
                                            → http://proxy.corp:8080 (Squid)
```

```ini
# クライアント ws0.conf
[Interface]
Address    = 10.7.0.2/24
PrivateKey = $PSK
Mode       = kernel

[Peer]
UUID       = $UUID
Endpoint   = ws://server:8080
AllowedIPs = 10.7.0.0/24          # カーネルルートは最小限: MS帯をここに入れないこと

[ProxyRule]
CIDRs = 20.20.32.0/19,20.190.128.0/18,20.231.128.0/19,40.126.0.0/18
Proxy = http://proxy.corp:8080
SNI   = true
```

クライアント側ではMS帯はどのカーネルルートにも現れません（遮断はカーネルが見る前に完了）。`AllowedIPs` が `10.7.0.0/24` のままなので、プロキシ自体はホストのデフォルトルートで到達可能で、ループしません。

NixOS:

```nix
services.ripws.tunnels.client.proxyRules = [
  {
    cidrs = [ "20.20.32.0/19" "20.190.128.0/18" "20.231.128.0/19" "40.126.0.0/18" ];
    proxy = "http://proxy.corp:8080";
    sni   = true;
  }
];
```

> **IPリテラル CONNECT:** プロキシによっては `CONNECT <IP>:443` を拒否します
> （実測: Squidは `SSL_ports` の443は許可、80番のCONNECTは403）。
> `SNI = true` でホスト名を送れば回避できます。SNIが解析できないフローは
> IPリテラルへフォールバックします。

---

## テストスクリプト

| スクリプト | 内容 |
|-----------|------|
| `scripts/test.sh` | カーネルモード L3 ping + Silent Drop（Docker 2コンテナ） |
| `scripts/test-nat.sh` | NATゲートウェイテスト（server→tunnel→client NAT→internet、TCP/UDP/DNS） |
| `scripts/load-test.sh` | iperf3スループットベンチマーク（カーネルモード、Docker） |
| `scripts/reconnect-test.sh` | 再接続 + KeepAliveロバスト性（サーバー再起動、ハードキル、アイドル） |
| `scripts/multiport-test.sh` | マルチポート同時接続テスト（32 iperf3ストリーム） |
| `scripts/cloudflare-test.sh` | Cloudflare経由カーネルモードE2E（ping + Silent Drop） |
| `scripts/cloudflare-load-test.sh` | Cloudflare経由カーネルモード負荷テスト（iperf3 + 1000 ping） |

---

## Dockerテスト

### カーネルモード（L3 ping, Silent Drop）

```
ripws-client (ws0 10.7.0.2/24)  --ws-->  ripws-server (ws0 10.7.0.1/24)
```

両コンテナとも `--cap-add=NET_ADMIN --device=/dev/net/tun` で実行。`scripts/entrypoint.sh` が環境変数からINI設定を生成し `ripws up` を実行。

```sh
bash scripts/test.sh
```

期待結果: `ping -c 4 10.7.0.1`（クライアント→サーバー）と `ping -c 4 10.7.0.2`（サーバー→クライアント）が成功。不正UUID/PSKのプローブは `Connection reset by peer`（HTTPステータス行なし）。

### NATゲートウェイ

```
ripws-server (ws0 10.7.0.1/24) → tunnel → ripws-client (userspace 10.7.0.2, NAT=true) → internet
```

```sh
bash scripts/test-nat.sh
```

### 手動Docker操作

```sh
nix build .#ripws
mkdir -p bin && cp -L result/bin/ripws bin/ripws
sudo docker compose build

# 認証情報生成
UUID="$(nix run .#ripws -- genuuid)"
PSK="$(nix run .#ripws -- genkey)"
mkdir -p config && printf 'UUID=%s\nPSK=%s\n' "$UUID" "$PSK" > config/creds.env

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
main.go                      up/down/genkey/genuuid ディスパッチ
flake.nix                    buildGoModule（静的バイナリ）、nixosModules、checks
nix/                         module.nix（NixOS）、package.nix、eval-test.nix、vm-test.nix
internal/
  config/                    認証情報、PSKバリデーション (SPEC 2.2)
    quickconf.go             wg-quick互換INIパーサー（Mode, NAT, [ProxyRule]対応）
  proto/                     IPバージョンニブル、キープアライブ、指数バックオフ
  tunnel/
    tunnel.go                Interface 抽象契約
    tun_linux.go             kernel TUN バックエンド (songgao/water)
    netstack.go              gVisor userspace バックエンド (netstack)
    setup_linux.go           wg-quick風 ifup/ifdown (addr/MTU/routes/DNS/hooks)
    nat.go                   userspace NATゲートウェイ (gVisor Forwarder → net.Dial)
    split.go                 split-dataplane: [ProxyRule] 遮断 + fan-in
    rules.go                 ProxyRule バリデーション + 最長プレフィックス一致
    proxy.go                 HTTP CONNECT / SOCKS5(h) プロキシダイアラ
    sni.go                   TLS ClientHello SNIパーサー（標準ライブラリのみ）
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
| L3接続性（カーネルモード） | PASS |
| Silent Drop セキュリティ | PASS |
| Userspace NATゲートウェイ | PASS（TCP/UDP/DNS, tunnel → internet） |
| iperf3スループット（kernel） | PASS（1.77 Gbps -P64） |
| マルチポート同時接続 | PASS（32/32） |
| 再接続・KeepAlive | PASS |
| レース検出器・ユニットテスト | PASS |
| Cloudflare tunnel（kernel） | PASS（wss://, 双方向, RTT ~232ms） |
