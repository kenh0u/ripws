# RIPWS (Raw IP over WebSocket) Protocol Specification

## 1. 概要 (Abstract)

本プロトコルは、WebSocket (RFC 6455) 通信をデータリンク層（L2）の仮想的な土管として利用し、生パケット（L3: IPv4/IPv6）を透過的にトンネリングするための極小仕様である。
Cloudflare等のリバースプロキシや厳しいファイアウォールを透過することを前提とし、暗号化・再送制御はトランスポート層（WSS: TLS over TCP）に完全委譲する。エンドポイントにおいては、伝送されたIPパケットがカーネルのネットワークインターフェース（TUNデバイス等）、あるいはユーザー空間のネットワークスタック（Userspace Networking Mode）へと引き渡され、シームレスにルーティングされることを要件とする。

## 2. ハンドシェイクと認証 (Handshake & Authentication)

経路上のDPI（Deep Packet Inspection）やプロキシによる検知を回避するため、ハンドシェイクは一般的なWebアプリケーションやAPIのトラフィックに完全に擬態しなければならない (MUST)。独自のHTTPヘッダは一切使用しない。

### 2.1. エンドポイントURIと難読化

トンネル確立のためのHTTP GETリクエストのパスは、固定文字列ではなく、クライアントごとに一意に発行された **UUIDv4 (RFC 4122)** を使用しなければならない (MUST)。これはエンドポイントの難読化と、サーバー側でのクライアント識別を兼ねる。

### 2.2. Pre-Shared Key (PSK)

認証には標準的な `Authorization: Bearer` ヘッダを使用する (MUST)。
トークン（PSK）は、十分なエントロピーを持たせるため、32バイト（256ビット）の乱数をBase64エンコードした文字列（44文字）を使用しなければならない (MUST)。

### 2.3. クライアントリクエスト例

```http
GET /f47ac10b-58cc-4372-a567-0e02b2c3d479 HTTP/1.1
Host: tunnel.example.com
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==
Sec-WebSocket-Version: 13
Authorization: Bearer 8x/P9...<32-byte-base64-encoded-key>...=

```

### 2.4. スキャン対策 (Silent Drop)

サーバーは、指定されたUUIDが存在しない場合、あるいは `Authorization` ヘッダのトークンが一致しない・欠落しているなどの認証失敗を検知した場合、HTTPステータスコード（`401 Unauthorized` や `403 Forbidden`、`404 Not Found` など）を**返してはならない (MUST NOT)**。
ポートスキャンや探索的アクセスを無効化するため、サーバーは直ちにTCPコネクションを強制切断（TCP RST ）しなければならない (MUST)。

## 3. データプレーン (Data Plane)

アップグレード完了後、双方向の通信はすべてWebSocketの **Binary Frame (Opcode `0x02`)** を使用する。独自のVPNヘッダやカプセル化オーバーヘッドは一切付与しない。

### 3.1. ペイロードの構造

フレームのペイロードは、**L3の生IPパケットそのもの**である。
フレームを受信した側は、ペイロードの先頭4ビット（Version Nibble）を検査し、IPバージョンを識別する。

* `0x4`: IPv4パケット
* `0x6`: IPv6パケット

### 3.2. ネットワークスタックへの統合要件

受信したIPパケットの処理は、エンドポイントの環境に応じて以下のいずれかの方法でルーティングされなければならない (MUST)。

* **Kernel Mode:** TUNデバイス等を通じてホストOSのカーネルネットワークスタックに注入し、OS標準のパケット転送機能を利用する。
* **Userspace Mode:** gVisorの `netstack` やLwIP等のユーザー空間ネットワークスタックに直接引き渡し、管理者権限（root）やカーネル機能に依存せずにL3ルーティングを完結させる。

## 4. KeepAliveと再接続制御 (KeepAlive & Reconnection)

NATやリバースプロキシによるアイドルタイムアウトを防ぐため、WebSocket標準のコントロールフレームを用いたKeepAliveを実装しなければならない (MUST)。

### 4.1. 動的なPing送信間隔

IoTデバイス等における消費電力低減（eDRX等のスリープサイクル）を考慮し、Ping送信の間隔は**クライアント側が 30秒から300秒 の範囲で任意に決定・変動させてよい (MAY)**。

* **Ping送信 (Client -> Server):** クライアントは自身のスリープスケジュールや通信環境に合わせて、上記範囲内で WebSocket Ping Frame (`Opcode 0x09`) を送信する。
* **Pong応答 (Server -> Client):** サーバーはPingを受信した場合、直ちに Pong Frame (`Opcode 0x0A`) を返さなければならない (MUST)。

### 4.2. コネクション切断の判断

クライアントは以下のいずれかの条件を満たした場合、直ちにソケットを破棄し、バックオフを伴う再接続手順に移行しなければならない (MUST)。

1. TCPレイヤーでの切断（RST/FIN）、または WebSocket Close Frame (`Opcode 0x08`) を検知した。
2. Pingフレーム送信後、十分な猶予時間（例: 15秒）を経過してもサーバーからPongフレームを受信できなかった。
