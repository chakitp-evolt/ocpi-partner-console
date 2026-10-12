# evolt-ocpi-partner-console

OCPI 2.2.1 partner mock สำหรับทดสอบ roaming กับ Evolt แบบ black-box: จำลอง partner ภายนอกได้ 3 แบบ ตั้งแต่ Versions/Credentials จนถึง Tokens, Commands, Sessions และ CDRs

| หน้า | Partner ที่จำลอง | Versions URL ที่ให้ Evolt |
|---|---|---|
| `/emsp` | eMSP — ทดสอบ Evolt ฝั่ง CPO | `<PUBLIC_BASE_URL>/ocpi/emsp/versions` |
| `/cpo` | CPO — ทดสอบ Evolt ฝั่ง eMSP | `<PUBLIC_BASE_URL>/ocpi/cpo/versions` |
| `/dual/` | CPO + eMSP ใน credentials ชุดเดียว | `<PUBLIC_BASE_URL>/dual/ocpi/versions` |

`/` redirect ไป `/emsp` · ทุกหน้ามีแผง Requests สำหรับเก็บและยิง request ที่เขียนเอง

## Deploy (สำหรับ DevOps)

| | |
|---|---|
| Image | `Dockerfile` ที่ root (multi-stage, debian-slim, non-root `evoltuser`) — หน้าเว็บ embed อยู่ใน binary |
| Port | `8080` (`PORT`) |
| Liveness | `GET /health` — ไม่แตะ DB |
| Readiness | `GET /readiness` — 503 เมื่อต่อ PostgreSQL ไม่ได้ |
| Replicas | **1** — state ของ CPO mock และ SSE อยู่ใน memory ของ process |
| Database | PostgreSQL 14+ ฐานเปล่า 1 ก้อน |

### Database

app **ไม่ migrate เอง** ต้องรัน script ด้วยมือครั้งเดียวก่อน deploy ครั้งแรก:

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/db/001_init.sql
```

ถ้ายังไม่ได้รัน app จะ boot ไม่ผ่าน (`relation ... does not exist`) · script รันใน transaction เดียว และหยุดทันทีถ้ามีตาราง `partner_console_*` อยู่แล้ว (ไม่แก้อะไร) จึงใช้ DB ร่วมกับ service อื่นได้โดยไม่ชนชื่อตาราง แต่แนะนำ database แยก

### Environment

| ตัวแปร | จำเป็น | ค่าเริ่มต้น | หมายเหตุ |
|---|---|---|---|
| `PORT` | | `8080` | |
| `DATABASE_URL` | ✓* | | หรือใช้ชุด `DATABASE_*` ด้านล่างแทน |
| `DATABASE_HOST` / `DATABASE_USER` / `DATABASE_NAME` | ✓* | | |
| `DATABASE_PASSWORD` | | | secret |
| `DATABASE_PORT` | | `5432` | |
| `DATABASE_SSLMODE` | | `disable` | |
| `ENCRYPTION_KEY` | ✓ | | secret — ใช้เข้ารหัส OCPI token ใน DB **ห้ามเปลี่ยนหลังมีข้อมูล** ไม่งั้น token เดิมถอดไม่ได้ |
| `PUBLIC_BASE_URL` | ✓ | `http://localhost:8080` | URL ที่ Evolt (adapter-ocpi) เรียกกลับมาหา mock ได้ |
| `EVOLT_VERSIONS_URL` | | `http://bff-external-service.dev.evtech.dev/api/ocpi/versions` | Versions URL ของ Evolt ที่ mock ไป register ด้วย |
| `SECRET_ENV_FILE` | | | path ไฟล์ secret เพิ่มเติม |

\* ต้องมี `DATABASE_URL` หรือ `DATABASE_HOST` + `DATABASE_USER` + `DATABASE_NAME`

### Vault

ให้ Vault Agent render secret เป็นไฟล์ dotenv (`KEY=value` ทีละบรรทัด) ไว้ที่ path ไหนก็ได้ในนี้ — app โหลด**ทุกไฟล์ที่มี** ตามลำดับความสำคัญ:

1. env var ของ pod (เช่น `envFrom` Secret) — ชนะทุกไฟล์
2. `SECRET_ENV_FILE` (ถ้าตั้ง)
3. `/vault/secrets/secret.env`
4. `/vault/secrets/config`
5. `/app/config/secret.env` — path เดียวกับ service อื่นของ Evolt (`configurer.LoadDotEnv("./config/secret.env")`)
6. `/app/.env` — สำหรับ local

key เดียวกันอยู่หลายที่ ใช้ค่าจากที่ลำดับสูงกว่า · log ตอน boot บอกว่าโหลดไฟล์ไหน (ไม่ log ค่า) · ถ้าขาด `ENCRYPTION_KEY` หรือค่า DB จะ boot ไม่ผ่านพร้อมบอกชื่อตัวแปร

### Network

- หน้าเว็บและ `/api/*` **ไม่มี auth** และ `/api/*/tokens` แสดง OCPI token ได้ — ต้องเปิดเฉพาะใน network ภายใน/VPN ห้าม expose public
- `/ocpi/*` และ `/dual/ocpi/*` ต้องให้ Evolt (adapter-ocpi) เรียกถึงผ่าน `PUBLIC_BASE_URL`
- mock ต้องเรียกออกไปหา `EVOLT_VERSIONS_URL` และ endpoint ที่ Evolt ประกาศได้

## Local

```bash
cp .env.example .env
make db-up      # Postgres เปล่าที่ :55432 (local เท่านั้น)
psql "postgres://partner_console:partner_console@localhost:55432/partner_console" -v ON_ERROR_STOP=1 -f scripts/db/001_init.sql
make run        # http://localhost:8080
```

หรือ `make up` เพื่อรันทั้ง Postgres และ console ใน Docker — ทั้งสองแบบไม่สร้างตารางให้ ต้องรัน script ก่อนเสมอ

| Make | |
|---|---|
| `make build` | build เป็น `bin/evolt-ocpi-partner-console` |
| `make test` | unit test |
| `make test-integration` | ต้องมี DB ชื่อ `partner_console_test` (`TEST_DATABASE_URL`) — test จะล้าง schema ทิ้ง |
| `make lint` | `go vet` |
| `make docker-build` | build image `evolt-ocpi-partner-console:local` |

## โครงสร้าง

```
cmd/server/          entrypoint
internal/config/     env + Vault secret file
internal/server/     probes, middleware (recover, access log, gzip)
internal/emsp/       eMSP mock + dual-role partner, page routing
internal/cpo/        CPO mock
internal/requests/   saved requests (Requests panel)
internal/store/      PostgreSQL repository
internal/ocpi/       OCPI envelope, headers, validation helpers
internal/web/        หน้าเว็บ (emsp / cpo / dual) embed เข้า binary
scripts/db/          SQL ที่ต้องรันด้วยมือ
```

| ตาราง | เก็บอะไร |
|---|---|
| `partner_console_connections` | 1 แถวต่อหน้าต่อขา: `/emsp` (`leg=emsp`), `/cpo` (`leg=cpo`), `/dual` (ทั้งสองขา, `profile=dual`) — config, status, credentials, token ที่เข้ารหัส (`secrets`) และ endpoint ของ Evolt (`endpoints`) |
| `partner_console_objects` | OCPI object ของทั้งสองขา แยกด้วย `leg` และ `module` (locations, tariffs, sessions, cdrs, tokens) |
| `partner_console_commands` | START / STOP ของทั้งสองขา แยกด้วย `leg` |
| `partner_console_exchanges` | log request/response ทุกครั้ง ของ connection (`connection_id`) หรือของ saved request (`saved_request_id`) — method, url, http_status ฯลฯ เป็น column ให้ query ตรงๆ ได้ |
| `partner_console_saved_requests` | request ที่บันทึกไว้ในแผง Requests |

DB ตรวจข้อมูลผิดรูปเอง: `leg` ของ `partner_console_objects`/`partner_console_commands` ต้องตรงกับ connection (composite foreign key), column ที่ใช้แค่ขาเดียวห้ามมีค่าในอีกขา, `module`/`profile`/`lane` ต้องเป็นค่าที่รู้จัก และมีได้ 1 connection ต่อหน้าต่อขา

ปุ่ม Reset ลบแถวใน `partner_console_connections` แล้วสร้างใหม่ — `partner_console_objects`, `partner_console_commands`, `partner_console_exchanges` ของ connection นั้นหายตาม (`ON DELETE CASCADE`) โดยไม่กระทบหน้าอื่น ส่วน saved request ไม่ถูกล้าง · ล้างข้อมูลด้วยปุ่ม Reset เท่านั้น อย่า `DELETE`/`INSERT` ตาราง `partner_console_connections` เอง

พฤติกรรม OCPI อ้างอิง OCPI 2.2.1 (`OCPI-2.2.1-d2.pdf`) และหา endpoint ของ Evolt จาก Version Details เท่านั้น
