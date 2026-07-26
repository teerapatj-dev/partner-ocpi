# mock-ocpi-partner

Mock OCPI 2.2.1 partner server สำหรับทดสอบ roaming ของ Evolt ครบ loop — codebase เดียว รันเป็น partner 3 เจ้า (แยก config + แยก database):

| Partner | party | local port | admin key (local) |
|---|---|---|---|
| PlugSiam | `TH/PLG` | 9101 | `plg-admin-key` |
| VoltCity | `TH/VCT` | 9102 | `vct-admin-key` |
| ChargeX | `TH/CHX` | 9103 | `chx-admin-key` |

แต่ละ partner ทำตัวเป็นทั้ง **CPO** (มี locations/tariffs ของตัวเองให้ Evolt pull) และ **eMSP** (รับ push locations/tariffs จาก Evolt) พร้อม seed data คนละชุด

## รัน local

```bash
make up          # 3 partners + Postgres (3 databases)
curl -s localhost:9101/health
```

## เส้นทาง OCPI (ต่อ partner)

ทุกเส้นต้องมี `Authorization: Token base64(<token>)` · ตอบ OCPI envelope (`status_code` เป็น int) · functional error ตอบ HTTP 200 + code ใน body (ยกเว้น 401/400/405)

| Method | Path | ใช้ตอน |
|---|---|---|
| GET | `/ocpi/versions` · `/ocpi/2.2.1` | handshake (Token A หรือ C) |
| POST/GET/PUT/DELETE | `/ocpi/2.2.1/credentials` | handshake (POST ใช้ Token A) |
| GET | `/ocpi/cpo/2.2.1/locations[/{id}[/{evse_uid}[/{connector_id}]]]` | Evolt Roaming-Out pull (paginated: `X-Total-Count`/`X-Limit`/`Link rel="next"`, default 50 max 100) |
| GET | `/ocpi/cpo/2.2.1/tariffs` | Evolt Roaming-Out pull |
| PUT/PATCH/GET | `/ocpi/emsp/2.2.1/locations/{cc}/{pid}/{location_id}[/{evse_uid}[/{connector_id}]]` | Evolt Roaming-In push (PATCH ต้องมี `last_updated`, object ไม่มี → `2003`) |
| PUT/DELETE/GET | `/ocpi/emsp/2.2.1/tariffs/{cc}/{pid}/{tariff_id}` | Evolt tariff push (DELETE ของที่ไม่มี → `2003` = converge) |
| GET | `/ocpi/cpo/2.2.1/{sessions,cdrs}` · POST `/ocpi/cpo/2.2.1/commands/{cmd}` · PUT/PATCH `/ocpi/emsp/2.2.1/{sessions,cdrs}/...` | stub เผื่อ phase หน้า |

## Admin API (สำหรับ web-demo / curl) — header `X-API-Key`, มี CORS

| Method | Path | ทำอะไร |
|---|---|---|
| POST | `/admin/tokens` | ออก Token A ให้ Evolt เริ่ม handshake มาหา partner นี้ (คู่กับ `POST /ocpi/partner/outbound` ฝั่ง Evolt) — ถ้า REGISTERED อยู่แล้วจะตอบ 409 กันเผลอ reset ต้องใส่ `?force=true` |
| POST | `/admin/handshake` | partner เป็นฝ่ายเริ่ม handshake ไปหา Evolt — body `{"evolt_versions_url": "...", "token_a": "<จาก /ocpi/partner/initial ของ Evolt>"}` |
| POST | `/admin/push/evse-status` | push PATCH สถานะ EVSE ไปหา Evolt — body `{"location_id","evse_uid","status"}` |
| POST / DELETE | `/admin/push/tariff` · `/admin/push/tariff/{id}` | PUT/DELETE tariff ของ partner ไปหา Evolt |
| GET | `/admin/state` | สรุปทุกอย่าง (จุดเดียวที่ UI poll) |
| GET | `/admin/registrations` | credentials ที่ลงทะเบียน (token ถูก mask) |
| GET | `/admin/own/{locations,tariffs}` | ข้อมูล CPO ของ partner |
| GET | `/admin/received/{locations,tariffs}` | ของที่ Evolt push เข้ามา |
| POST | `/admin/received/locations` | inject location ตั้งต้น (Evolt วันนี้ PATCH สถานะอย่างเดียว ไม่เคย PUT เต็มก้อน — ต้องมี baseline ก่อน PATCH ถึงจะไม่ `2003`) |
| GET | `/admin/requests?limit=50` | request log ล่าสุด (ไม่เก็บค่า token) |
| POST | `/admin/seed/reset` | ล้าง data + seed ใหม่ (คง registration ไว้) |

## ทดสอบกับ Evolt

**ทิศ Evolt → partner (Evolt เริ่ม):**
1. `POST /admin/tokens` ที่ partner → ได้ Token A
2. ฝั่ง Evolt: `POST /ocpi/partner/outbound` (X-API-Key) body `{partner_name, token_a, partner_versions_url: "<partner>/ocpi/versions"}`
3. เช็คผล: `GET /admin/registrations` ต้องเป็น REGISTERED

> ⚠️ ทิศนี้ partner จะ push กลับหา Evolt ได้ก็ต่อเมื่อ callback ตอนรับ handshake สำเร็จ (ได้ endpoints ของ Evolt มาเก็บ) — ถ้า Evolt อยู่หลัง network ที่ partner ยิงไม่ถึง callback จะถูกข้าม (`STRICT_CALLBACK=false`) แล้ว push จะตอบ error "no RECEIVER endpoint" ให้ใช้ทิศ partner → Evolt แทน

**ทิศ partner → Evolt (partner เริ่ม):**
1. ฝั่ง Evolt: `POST /ocpi/partner/initial` → ได้ Token A ของ Evolt
2. `POST /admin/handshake` ที่ partner พร้อม URL versions ของ Evolt (ผ่าน bff: `https://<evolt>/api/ocpi/versions`) — ถ้า Evolt อยู่หลัง network ส่วนตัวและ partner อยู่ cloud ต้องใช้ URL ที่ partner ยิงถึงได้ (เช่น ngrok)

**Roaming-Out pull:** ยิงผ่าน adapter ของ Evolt: `POST {adapter}/ocpi/locations/pull` / `/ocpi/tariffs/pull` body `{url: "<partner>/ocpi/cpo/2.2.1/locations", token: "<token ที่ partner ออกให้>", limit: 100}` (loop cron ฝั่ง Evolt ยังไม่ implement — mock รองรับ `Link rel="next"` พร้อมแล้ว)

**Roaming-In push:** trigger จาก flow จริง (Kafka `tariff_update` / `evse_status`) แล้วดูผลที่ `GET /admin/received/*` + `GET /admin/requests`

## Web demo (แชร์ลิงก์ให้ทีมเล่นครบ loop)

`cmd/demo` คือเว็บ + orchestrator ตัวเดียว: UI หน้าเดียว (embed), API `/api/demo/*` ที่ถือ secret ทั้งหมดฝั่ง server (browser ไม่เห็น token ใดๆ), และ reverse-proxy `/ocpi/*` → PlugSiam เพื่อให้ Evolt callback เข้า hostname เดียวกับที่คนเล่นใช้

**เงื่อนไข network:** เครื่องที่รันต้องต่อ VPN ถึง Evolt dev (Evolt internal ยิงจาก cloud ไม่ได้) — คนอื่นเข้าผ่าน Cloudflare named tunnel

**ตั้งครั้งเดียว:**
1. Cloudflare dashboard → Zero Trust → Networks → Tunnels → **Create tunnel** (Cloudflared)
2. รันคำสั่ง install ที่หน้านั้นโชว์ → cloudflared ขึ้นเป็น service บนเครื่อง (อยู่รอดข้ามรีบูต)
3. ใน tunnel เพิ่ม **Public hostname** เช่น `ocpi-demo.<your-domain>` → service **`http://localhost:9100`**
4. `cp .env.demo.example .env.demo` แล้วเติม URL/key ของ Evolt (ขอจากทีม) — ไฟล์นี้ถูก gitignore

ถ้าไม่อยากติดตั้งบนเครื่อง ใช้ cloudflared ใน compose แทนได้: ใส่ `TUNNEL_TOKEN` ใน `.env.demo`, ตั้ง public hostname เป็น `http://demo:8080` แล้วรันด้วย `--profile tunnel` — **ห้ามรันทั้งสองทางพร้อมกัน** เพราะ route มีชุดเดียวแต่ปลายทางคนละที่

**รัน:** `make demo-up` → เปิด `https://ocpi-demo.<your-domain>` (local: `http://localhost:9100`) · ปิด: `make demo-down` · log: `make demo-logs`

หมายเหตุ: หลัง tunnel เปลี่ยน hostname (หรือใช้ quick tunnel) ต้องกด handshake ใหม่ในหน้าเว็บ เพราะ Evolt จำ callback URL เดิม · ปุ่มฝั่ง Evolt ปิดตัวเองพร้อมเหตุผลเมื่อ dev ปิด (~20:00) หรือ Kafka ไม่ถึง

## Deploy ฟรี: Render + Neon

1. **Neon** (neon.tech): สร้าง project 1 อัน → สร้าง database 3 ก้อน: `partner_plg`, `partner_vct`, `partner_chx` → copy connection string ของแต่ละก้อน (มี `?sslmode=require` อยู่แล้ว)
2. **Render** (render.com): New → Blueprint → ชี้ repo นี้ (ต้อง push ขึ้น GitHub/GitLab ก่อน) → Render อ่าน `render.yaml` สร้าง 3 services → ตอน prompt ให้วาง `DATABASE_URL` ของแต่ละ partner
3. `BASE_URL` ไม่ต้องตั้ง (ใช้ `RENDER_EXTERNAL_URL` อัตโนมัติ) · `ADMIN_API_KEY` ถูก generate — ดูได้ในหน้า Environment ของแต่ละ service
4. ⚠️ **Free tier หลับหลัง idle ~15 นาที** (ตื่น ~30–50s) และ Evolt timeout 30s ไม่มี retry — **รัน `./scripts/prewarm.sh <url1> <url2> <url3>` ก่อนทดสอบ/เดโมทุกครั้ง** · อย่าตั้ง keep-alive ping (ชั่วโมงฟรี 750 ชม./เดือนไม่พอ 3 ตัว)

## Env vars

| ตัวแปร | ความหมาย | default |
|---|---|---|
| `PARTNER_NAME` / `PARTNER_PARTY_ID` / `PARTNER_COUNTRY_CODE` / `PARTNER_CURRENCY` | ตัวตน partner | – / – / `TH` / `THB` |
| `DATABASE_URL` | Postgres ของ partner นี้ | – (required) |
| `BASE_URL` | URL สาธารณะ ใช้ใน endpoints/Link headers | `RENDER_EXTERNAL_URL` หรือ `http://localhost:<port>` |
| `PORT` / `HTTP_PORT` | พอร์ต | `8080` |
| `ADMIN_API_KEY` | key ของ `/admin/*` (fail-closed: ไม่ตั้ง = ปฏิเสธหมด) | – |
| `CORS_ORIGINS` | origins ของ web-demo (comma) | `*` |
| `STRICT_CALLBACK` | `true` = callback หา initiator ไม่ได้ให้ fail handshake (`3001`) | `false` |
| `OUTBOUND_TIMEOUT_SECONDS` | timeout ยิงออกหา Evolt | `10` |
