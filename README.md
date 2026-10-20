# evolt-ocpi-partner-console

OCPI 2.2.1 partner mock สำหรับทดสอบ roaming กับ Evolt แบบ black-box: จำลอง partner ภายนอกได้ 3 แบบ ตั้งแต่ Versions/Credentials จนถึง Tokens, Commands, Sessions และ CDRs

| หน้า | Partner ที่จำลอง | Versions URL ที่ให้ Evolt |
|---|---|---|
| `/emsp` | eMSP — ทดสอบ Evolt ฝั่ง CPO | `<PUBLIC_BASE_URL>/ocpi/emsp/versions` |
| `/cpo` | CPO — ทดสอบ Evolt ฝั่ง eMSP | `<PUBLIC_BASE_URL>/ocpi/cpo/versions` |
| `/dual/` | CPO + eMSP ใน credentials ชุดเดียว | `<PUBLIC_BASE_URL>/dual/ocpi/versions` |

`/` redirect ไป `/emsp` · ทุกหน้ามีแผง Requests สำหรับเก็บและยิง request ที่เขียนเอง

### หลาย env ใน instance เดียว

dev อยู่ที่ URL ข้างบน env อื่นที่ตั้งใน `CONSOLE_ENVS` อยู่ใต้ `/<env>` ทุกหน้า เช่น `CONSOLE_ENVS=stg`:

| | dev | stg |
|---|---|---|
| หน้าเว็บ | `/emsp` · `/cpo` · `/dual/` | `/stg/emsp` · `/stg/cpo` · `/stg/dual/` |
| Versions URL ที่ให้ Evolt | `<PUBLIC_BASE_URL>/ocpi/emsp/versions` | `<PUBLIC_BASE_URL>/stg/ocpi/emsp/versions` |

แต่ละ env มี connection, token, object และ CPO state ของตัวเอง Reset ของ env หนึ่งไม่แตะอีก env · saved request ใช้ร่วมกันทุก env แต่ยิงด้วย token และเก็บประวัติแยกตาม env ที่เปิดอยู่ · สลับ env ด้วยปุ่ม DEV / STG มุมล่างของแถบด้านข้าง · env ที่ไม่ใช่ dev มีแถบสีม่วงบนสุดบอกชื่อ env พร้อมลิงก์กลับหน้าเดียวกันบน DEV และชื่อแท็บขึ้นต้นด้วย [STG]

## Deploy (สำหรับ DevOps)

### Render + Neon

สำเนานี้ใช้ GitHub `chakitp-evolt/ocpi-partner-console` และมี [render.yaml](render.yaml) สำหรับ Render Web Service แบบ Docker + Neon แยกต่างหาก โดยไม่สร้าง Render Postgres

1. สร้าง Neon project/database แยกสำหรับ console และเลือก region ใกล้ Singapore
2. ใน Neon SQL Editor เลือก database นั้น แล้วรันเนื้อหาของ `scripts/db/001_init.sql` ตามด้วย `scripts/db/002_connection_env.sql` **ครั้งเดียวตามลำดับ** ก่อน deploy แอป หรือใช้ `psql` ตามหัวข้อ Database ด้านล่างกับ direct connection string ของ Neon
3. จาก Neon → Connect คัดลอก pooled connection string (hostname มี `-pooler`) พร้อม `sslmode=require` เป็น `DATABASE_URL` เก็บ query parameters ที่ Neon ให้มา และเพิ่ม `connect_timeout=15` ได้ ตัวอย่างอยู่ใน [.env.render.example](.env.render.example)
4. หลัง push โค้ดและ config ขึ้น GitHub แล้ว ใน Render เลือก **New → Blueprint** เชื่อม repo นี้ เลือก branch ที่มี `render.yaml` แล้วกรอก `DATABASE_URL`, `CONSOLE_PASSWORD` และ `EVOLT_VERSIONS_URL` ที่ Render ถาม ค่า Versions URL ต้องเป็น endpoint ของ Evolt ที่เรียกถึงได้จากอินเทอร์เน็ต
5. Render สร้าง `ENCRYPTION_KEY` ให้ครั้งแรก และตั้ง username เป็น `admin` (เปลี่ยนได้ใน Environment) เก็บ key นี้ไว้กับฐานข้อมูลเดิมเสมอ รวมถึงเมื่อสร้าง service ใหม่หรือ restore DB
6. เปิด URL `https://<ชื่อ-service>.onrender.com` แล้ว login ด้วย `CONSOLE_USERNAME` / `CONSOLE_PASSWORD` ตรวจ `/health` และ `/readiness` ต้องได้ HTTP 200

`PUBLIC_BASE_URL` ใช้ `RENDER_EXTERNAL_URL` อัตโนมัติ จึงไม่ต้องเดาชื่อโดเมนก่อน deploy ถ้าใช้ custom domain ค่อยตั้ง `PUBLIC_BASE_URL=https://<โดเมนจริง>` ก่อนสร้าง connection ใหม่ ส่วน connection ที่บันทึกไว้แล้วต้องแก้ Partner public URL ในหน้าเว็บให้ตรงด้วย

Blueprint ตั้ง region `singapore`, plan `free`, health check `/health` และปิด auto deploy (`autoDeployTrigger: off`) ให้สั่ง deploy เองหลังเปลี่ยนโค้ด โดยใช้ **1 instance** เสมอ เพราะ mock มี state และ SSE ใน process หากต้องรับ OCPI callback ต่อเนื่อง ให้เปลี่ยนเป็น paid compute: free service พักเมื่อไม่มี traffic และการปลุกอาจทำให้ request แรกหรือ callback timeout ได้ การ restart/redeploy จะตัด SSE และ state ใน memory บางส่วน

หน้าเว็บ, assets และ console API ใช้ Basic Auth ส่วน `/ocpi/*`, `/dual/ocpi/*` และ path เหล่านี้ใต้ env prefix ใช้ OCPI authentication ตามเดิม จึงให้ Evolt เรียกกลับได้โดยไม่ต้องส่งรหัสผ่านหน้าเว็บ `/health` และ `/readiness` เปิดให้ health probe เรียกได้ บน Render แอปจะไม่เริ่มถ้าไม่มีรหัสผ่าน console หรือใช้ encryption key ตัวอย่าง

อ้างอิง: [Render Blueprint](https://render.com/docs/blueprint-spec), [Render environment variables](https://render.com/docs/environment-variables), [Render free services](https://render.com/docs/free), [Neon Go](https://neon.com/docs/guides/go), [Neon pooling](https://neon.com/docs/connect/connection-pooling)

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

แล้วรัน `002` ต่อครั้งเดียว (ใช้กับ DB ที่มีข้อมูลแล้วได้ แถวเดิมทั้งหมดกลายเป็น dev และ version เดิมยังทำงานต่อได้ — รันก่อน deploy version ที่รองรับหลาย env):

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/db/002_connection_env.sql
```

ต้องรันด้วย user ที่เป็นเจ้าของตาราง (`002` ใช้ `ALTER TABLE`) · ถ้ายังไม่ได้รัน app จะ boot ไม่ผ่าน (`relation ... does not exist` / `column "env" does not exist`) · script รันใน transaction เดียว และหยุดทันทีถ้า DB มีของที่มันสร้างอยู่แล้ว (ไม่แก้อะไร) · ตารางขึ้นต้นด้วย `partner_console_` จึงใช้ DB ร่วมกับ service อื่นได้โดยไม่ชนชื่อ แต่แนะนำ database แยก

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
| `PUBLIC_BASE_URL` | | `RENDER_EXTERNAL_URL` บน Render, อื่น ๆ `http://localhost:8080` | URL ที่ Evolt (adapter-ocpi) เรียกกลับมาหา mock ได้; ตั้งเองเมื่อใช้ custom domain |
| `CONSOLE_USERNAME` / `CONSOLE_PASSWORD` | บน Render ✓ | | Basic Auth สำหรับหน้าเว็บและ console API ต้องตั้งทั้งคู่; local ไม่ตั้งได้ |
| `EVOLT_VERSIONS_URL` | | `http://bff-external-service.dev.evtech.dev/api/ocpi/versions` | Versions URL ของ Evolt (dev) ที่ mock ไป register ด้วย |
| `CONSOLE_ENVS` | | | env เพิ่มเติมคั่นด้วย `,` เช่น `stg` — ตัวพิมพ์เล็ก ตัวเลข หรือ `-` ห้ามใช้ `dev` `emsp` `cpo` `dual` `api` `ocpi` |
| `EVOLT_VERSIONS_URL_<ENV>` | ✓** | | Versions URL ของแต่ละ env ใน `CONSOLE_ENVS` เช่น `EVOLT_VERSIONS_URL_STG` (`-` เป็น `_`) |
| `SECRET_ENV_FILE` | | | path ไฟล์ secret เพิ่มเติม |

\* ต้องมี `DATABASE_URL` หรือ `DATABASE_HOST` + `DATABASE_USER` + `DATABASE_NAME`

\*\* บังคับเมื่อมี env นั้นใน `CONSOLE_ENVS` — ขาด = boot ไม่ผ่าน เพื่อไม่ให้ env ใหม่ชี้ dev เงียบๆ

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

- หน้าเว็บและ console API ต้องตั้ง `CONSOLE_USERNAME` + `CONSOLE_PASSWORD` ก่อนเปิด public และใช้ HTTPS; ถ้าไม่ตั้งจะใช้ได้เฉพาะ network ภายใน/VPN เพราะ `/api/*/tokens` แสดง OCPI token ได้
- `/ocpi/*` และ `/dual/ocpi/*` ต้องให้ Evolt (adapter-ocpi) เรียกถึงผ่าน `PUBLIC_BASE_URL`
- mock ต้องเรียกออกไปหา `EVOLT_VERSIONS_URL` และ endpoint ที่ Evolt ประกาศได้

## Local

```bash
cp .env.example .env
make db-up      # Postgres เปล่าที่ :55432 (local เท่านั้น)
psql "postgres://partner_console:partner_console@localhost:55432/partner_console" -v ON_ERROR_STOP=1 -f scripts/db/001_init.sql
psql "postgres://partner_console:partner_console@localhost:55432/partner_console" -v ON_ERROR_STOP=1 -f scripts/db/002_connection_env.sql
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
