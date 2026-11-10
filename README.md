# rpc-health

Lightweight health-check API สำหรับ EVM JSON-RPC node (Geth, Erigon, Nethermind, LUKSO ฯลฯ)
poll `eth_blockNumber` เป็นระยะ แล้วตอบ HTTP status ตามว่า block height ยังขยับอยู่หรือไม่ — เอาไปต่อกับ Uptime Kuma, Gatus, load balancer หรือ Kong upstream healthcheck ได้ทันที

- เขียนด้วย Go (stdlib ล้วน ไม่มี dependency)
- Docker image บน distroless, รันเป็น non-root
- ตั้งค่าทั้งหมดผ่าน environment variables

## How it works

```
┌────────────────┐   eth_blockNumber   ┌──────────┐
│   rpc-health   │ ──────────────────▶ │ RPC node │
│ poll ทุก        │ ◀────────────────── │          │
│ CHECK_INTERVAL │     "0x3e8a1f"      └──────────┘
└───────┬────────┘
        │ GET /health  →  200 / 502 / 503
        ▼
 Uptime Kuma / Gatus / LB
```

service จะจำเวลาที่ block เปลี่ยนครั้งล่าสุด แล้วตัดสินสถานะตามนี้

| status | เงื่อนไข | HTTP |
|---|---|---|
| `syncing` | block เปลี่ยนภายใน `STALL_THRESHOLD` | **200** |
| `stalled` | block ไม่ขยับเกิน `STALL_THRESHOLD` | **503** |
| `rpc_unreachable` | เรียก RPC ไม่สำเร็จเกิน `STALL_THRESHOLD` หรือยังไม่เคยสำเร็จเลย | **502** |
| `starting` | เพิ่งเริ่ม ยังไม่มีผลเช็คแรก | **503** |

## Quick start

```bash
git clone https://github.com/<your-username>/rpc-health.git
cd rpc-health
cp .env.example .env        # แก้ RPC_HOST / RPC_PORT
docker compose up -d --build
curl -i http://localhost:8080/health
```

ตัวอย่าง response

```json
{
  "status": "syncing",
  "rpc": "http://host.docker.internal:28545",
  "block_number": 4123456,
  "seconds_since_block_change": 3,
  "seconds_since_rpc_success": 3,
  "stall_threshold_seconds": 120,
  "last_check": "2026-11-10T12:51:37Z"
}
```

เมื่อเกิด error จะมีฟิลด์ `last_error` เพิ่มมาด้วย

## Endpoints

| Method | Path | คำอธิบาย |
|---|---|---|
| `GET` | `/health` | status code ตามตารางด้านบน — ใช้สำหรับ monitor |
| `GET` | `/` | เหมือน `/health` |
| `GET` | `/status` | JSON เดียวกัน แต่ตอบ **200 เสมอ** ใช้ดูข้อมูล และใช้เป็น Docker `HEALTHCHECK` ของตัว service เอง |

## Configuration

| Variable | Default | คำอธิบาย |
|---|---|---|
| `RPC_HOST` | `host.docker.internal` | IP หรือ hostname ของ node |
| `RPC_PORT` | `8545` | port ของ JSON-RPC |
| `RPC_SCHEME` | `http` | `http` หรือ `https` |
| `RPC_URL` | — | ถ้าตั้ง จะ override `RPC_SCHEME`/`RPC_HOST`/`RPC_PORT` ทั้งหมด |
| `CHECK_INTERVAL` | `15s` | ความถี่ในการ poll รับ `15`, `15s`, `1m` |
| `STALL_THRESHOLD` | `120s` | block ไม่ขยับนานเกินนี้ถือว่า stalled |
| `RPC_TIMEOUT` | `5s` | timeout ต่อ request |
| `LISTEN_PORT` | `8080` | port ภายใน container |
| `HOST_PORT` | `8080` | port ที่ publish บน host (ใช้ใน compose) |

> **เลือก `STALL_THRESHOLD` อย่างไร:** chain ที่ block time ~12s (Ethereum, LUKSO) ใช้ 120s ≈ 10 block กำลังดี ถ้า chain block เร็วกว่านี้ลดลงได้ แต่ควรมากกว่า `CHECK_INTERVAL` อย่างน้อย 3–4 เท่า

## Network setup

**Node อยู่บน host เดียวกัน**
ใช้ `RPC_HOST=host.docker.internal` (compose ตั้ง `host-gateway` ไว้ให้แล้ว) และ node ต้อง bind RPC ที่ `0.0.0.0` หรือ IP ของ docker bridge

ถ้า node bind แค่ `127.0.0.1` ให้แก้ compose เป็น host network

```yaml
services:
  rpc-health:
    network_mode: host
    # ลบ ports: และ extra_hosts: ออก
```

แล้วตั้ง `RPC_HOST=127.0.0.1`

**Node อยู่ใน compose network เดียวกัน**
ตั้ง `RPC_HOST` เป็นชื่อ service เช่น `RPC_HOST=geth`

**Node อยู่คนละเครื่อง**
ตั้ง `RPC_HOST` เป็น IP ของเครื่องนั้น หรือใช้ `RPC_URL=https://rpc.example.com`

## Monitoring

> ถ้า monitor รันใน container ใช้ `localhost` ไม่ได้ ต้องใช้ IP ของ host หรือให้อยู่ docker network เดียวกันแล้วเรียก `http://rpc-health:8080/health`

### Uptime Kuma

- **Monitor Type:** HTTP(s)
- **URL:** `http://<host>:8080/health`
- **Heartbeat Interval:** 60s
- **Accepted Status Codes:** `200-299`

ถ้าต้องการเช็คค่าใน JSON ด้วย ใช้ **HTTP(s) - Json Query** ตั้ง query เป็น `status` และ expected value เป็น `syncing`

### Gatus

```yaml
endpoints:
  - name: lukso-rpc
    group: blockchain
    url: "http://rpc-health:8080/health"
    interval: 60s
    conditions:
      - "[STATUS] == 200"
      - "[BODY].status == syncing"
      - "[RESPONSE_TIME] < 2000"
```

แนะนำให้ตั้ง alert เมื่อ fail 2–3 ครั้งติดกัน เพื่อกันการแจ้งเตือนผิดตอน block ช้าชั่วคราว

## Run without Docker

```bash
go build -o rpc-health .
RPC_HOST=127.0.0.1 RPC_PORT=28545 ./rpc-health
```

ต้องใช้ Go 1.22 ขึ้นไป

## Limitations

- ตรวจแค่ว่า block "ขยับ" หรือไม่ ไม่ได้เทียบกับ chain head จริง ถ้า node กำลังไล่ sync ตามหลังอยู่ก็จะได้ 200
- รองรับเฉพาะ JSON-RPC ผ่าน HTTP(S) ยังไม่รองรับ WebSocket หรือ RPC ที่ต้อง auth

## License

MIT
