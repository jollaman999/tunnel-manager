# 터널 조정 루프, 인증, 내장 UI

작성: 2026-09-16 · 상태: 승인됨

## 왜 하나

터널을 띄우는 것은 되돌릴 수 없는데, 지금은 그것을 되돌릴 수 있는 것(DB 트랜잭션) 안에서 한다.
`CreateServicePort` 는 트랜잭션이 열린 채로 SSH 접속을 만들고(`handlers.go:481-530`),
`DeleteHost` 는 트랜잭션을 열기도 전에 터널을 끊는다(`handlers.go:382`).
핸들러 여섯이 터널 조작을 트랜잭션 기준 세 군데에 서로 다르게 놓고 있다.

`tunnels` 테이블은 여덟 곳에서 쓰이는데 전부 `m.db` 다. 핸들러의 트랜잭션이 아니다.
그래서 핸들러가 롤백해도 그 행은 안 돌아온다.

`h.rwLock` 은 이 어긋남을 힘으로 누르려고 요청 전체를 잡는다. SSH 접속까지 그 안에 있어서,
호스트 세 대가 등록된 상태에서 하나가 죽어 있으면 서비스 포트 하나를 등록하는 동안
`GET /api/host` 같은 무관한 조회까지 최대 30초 막힌다.

## 결정 1 - 조정 루프

목표 상태와 실제 상태를 나누고, 둘을 대조해 차이만 메우는 루프를 둔다.

| 무엇 | 누가 쓰나 |
|------|-----------|
| 목표 상태 - `hosts`(enabled=true) x `service_ports` | 핸들러가 트랜잭션으로 |
| 실제 상태 - `tunnels` 테이블, `m.tunnels` 맵 | 매니저와 터널 고루틴만 |

핸들러는 행만 쓰고 루프를 깨운 뒤 즉시 응답한다. 터널을 직접 건드리지 않는다.

```
POST /api/service-port
  트랜잭션 { service_ports INSERT } 커밋
  루프 깨우기
  201

조정 루프
  목표 = 활성 호스트 x 서비스 포트
  실제 = m.tunnels
  목표에 있는데 실제에 없으면 -> 띄운다
  실제에 있는데 목표에 없으면 -> 끈다
```

`RestoreAllTunnels` 는 첫 조정 패스가 되고, `StopAllTunnels` 는 목표를 비우고 한 번 더 도는 것이 된다.
`stopTunnelsStarted`(`a562b59`)는 제거한다. 루프가 그 일을 한다.

### 설정이 바뀐 터널도 다시 띄운다

키(`hostID-spID`)의 유무만 비교하면 부족하다. `UpdateHost` 로 IP, 포트, 사용자, 비밀번호를 바꾸거나
`UpdateServicePort` 로 포트를 바꿔도 키는 그대로라, 떠 있던 터널이 **옛 설정으로 계속 돈다.**
증상이 조용하다 - API 는 200 을 주고 DB 도 바뀌는데 실제 연결만 옛 주소를 물고 있다.

고치기 전에는 `resolveTunnelActions` 의 `connectionChanged` 가 중지와 시작을 시켰다.
그 함수를 없애면서 같이 사라졌으므로, 루프가 그 판정을 대신해야 한다.

그래서 **키가 양쪽에 있을 때 접속 정보가 같은지도 본다.**

| 비교 결과 | 행동 |
|-----------|------|
| 목표에만 있다 | 띄운다 |
| 실제에만 있다 | 끈다 |
| 양쪽에 있고 접속 정보가 같다 | 건드리지 않는다 |
| **양쪽에 있는데 접속 정보가 다르다** | **끄고 다시 띄운다** |

접속 정보는 터널을 만들 때 쓰는 값 전부다 - 서버 주소, 원격 주소, 로컬 주소, 사용자, 비밀번호.
비밀번호까지 넣는 이유는 그것만 바뀌어도 기존 연결이 옛 자격증명으로 맺어져 있기 때문이다.
비밀번호를 그대로 들고 비교하지 않도록, 떠 있는 터널에 그 값들의 지문을 붙여 두고 지문끼리 비교한다.

### 바뀐 뒤의 동작

`POST /api/service-port` 가 201 을 주는 것은 **적어 뒀다**는 뜻이다. 터널이 떴다는 뜻이 아니다.
지금은 터널이 하나라도 못 뜨면 500 을 주고 행도 안 남는데, 그 보장이 사라진다.

대신 `GET /api/status` 가 어긋남을 보여준다.

```
desired_tunnels   3    떠야 할 것
total_tunnels     3    tunnels 행
connected_tunnels 2    실제로 붙은 것
tunnels[].last_error   왜 못 붙었는지
```

`desired_tunnels` 는 새 필드다. 기존 필드의 뜻은 안 바꾼다.

### 루프를 언제 도나

**즉시 반영되어야 하는 자리가 둘이다.**

| 언제 | 어떻게 |
|------|--------|
| 서비스 포트나 호스트를 등록·수정·삭제할 때 | 핸들러가 커밋 직후 루프를 깨운다 |
| 앱이 기동할 때 | 첫 조정 패스를 돌고 나서 HTTP 서버를 띄운다 |

깨우기를 놓친 경우를 위해 주기 폴백을 둔다. 주기는 `reconcile.interval_sec` 을 새로 만들고
**기본 5초**다. 목표와 실제를 비교하는 일은 작은 테이블 두 개를 읽는 것이라 짧게 잡아도 부담이 없고,
깨우기가 안 왔을 때 어긋난 상태가 남아 있는 시간이 그만큼 짧아진다.

`monitoring.interval_sec` 과 기본값이 같지만 항목은 따로 둔다. 한쪽은 떠 있는 터널이 살아 있는지
보는 값이고 다른 쪽은 떠야 할 것과 떠 있는 것을 맞추는 값이라, 한 값으로 묶으면 한쪽을 조절할 때
다른 쪽이 끌려간다.

### 동시 수정

`h.rwLock` 을 전부 제거한다. 핸들러가 DB 만 만지므로 트랜잭션이 직렬화한다.
다만 같은 행을 동시에 고치면 나중 쓰기가 앞 것을 덮으므로,
`UpdateHost` 와 `UpdateServicePort` 는 트랜잭션 안에서 대상 행을 `SELECT ... FOR UPDATE` 로 잠근다.
같은 행을 건드리는 요청만 기다리고 조회는 안 막힌다.

### 프로세스가 중간에 죽으면

첫 조정 패스가 기동할 때 목표와 실제를 다시 맞추므로 복구된다.
터널만 뜨고 행이 없던 경우는 그 SSH 연결이 프로세스와 함께 죽는다.

## 결정 2 - keepalive 데드라인

감시자가 보내는 keepalive(`ssh.go:176`)에 데드라인이 없다.
피어가 응답도 거부도 안 하면 TCP 가 포기할 때까지 감시 틱을 붙잡는다.

`ssh.Client` 가 밑단 `net.Conn` 을 감추고 있어서, 연결 수립을 바꿔야 데드라인을 걸 수 있다.

```
지금:   ssh.Dial(...)
바꾼 뒤: net.DialTimeout(...) -> conn
        ssh.NewClientConn(conn, ...) -> ssh.NewClient(...)
        keepalive 전에 conn.SetDeadline
```

## 결정 3 - 인증

지금 API 에는 인증이 없다. UI 가 붙으면 주소만 알면 누구나 호스트를 추가하고
SSH 비밀번호를 넣을 수 있게 되므로 같이 넣는다.

### 계정

계정은 하나이고 **DB 에 둔다**. 설정 파일에는 사용자명도 비밀번호도 두지 않는다.
설정 파일을 고치지 않고 계정을 바꿀 수 있게 하기 위해서다.

테이블 이름은 `user` 다. 행이 하나뿐이라 복수형이 맞지 않는다.
`user` 는 SQL 의 함수 이름이기도 하지만, GORM 의 mysql 드라이버가 식별자를 백틱으로 감싸므로
(`gorm.io/driver/mysql@v1.5.7/mysql.go:290`) 따옴표 없이 나가는 경로가 없다.

```
user
  id
  username        설정 전에는 빈 문자열
  password_hash   bcrypt
  setup_required  bool
  created_at, updated_at
```

`setup_required` 를 "username 이 비어 있는가" 로 대신할 수도 있지만 칼럼을 둔다.
로그인 흐름이 이 값으로 갈리는데, 그 분기를 "사용자명이 우연히 비어 있다" 에 기대게 하면
나중에 비밀번호 강제 재설정 같은 것을 붙일 때 깨진다.

### 비밀번호는 해시로 저장한다

SSH 접속 비밀번호는 원문을 SSH 서버에 보내야 하므로 복호 가능한 암호화(AES-256-GCM)를 쓴다.
로그인 비밀번호는 입력값이 맞는지만 보면 되고 원문이 필요한 곳이 없으므로 **해시**로 둔다.
복구할 수 없게 두는 편이 안전하고, 암호화 키가 새도 로그인 비밀번호는 안 샌다.

bcrypt 를 쓴다. `golang.org/x/crypto` 가 이미 직접 의존성(v0.31.0)이고 그 안에 있어서
`go.mod` 에 `require` 가 늘지 않는다.

### 첫 기동과 설정

`user` 가 비어 있으면 임시 비밀번호를 난수로 만들어 파일에 `0600` 으로 쓰고,
**사용자명 없이** 행 하나를 만든다 (`username` 은 빈 문자열, `setup_required` 는 true).

**로그에는 경로만 남기고 값은 안 남긴다.** 로그는 파일과 콘솔 양쪽으로 나가기 때문이다.
암호화 키 파일과 같은 방식이다. 경로는 `auth.initial_password_file`, 기본 `keys/initial-password`.

사용자명과 비밀번호는 **처음 로그인한 뒤에 정한다.**

```
1. 첫 로그인   임시 비밀번호만 입력한다. 사용자명은 아직 없다
2. 설정        사용자명과 새 비밀번호를 정한다
               setup_required 를 내리고 임시 비밀번호 파일을 지운다
3. 그 뒤       사용자명과 비밀번호로 로그인한다
```

`POST /api/login` 은 `setup_required` 가 true 면 비밀번호만 보고, false 면 사용자명도 본다.
설정 전에는 로그인해도 `POST /api/setup` 말고는 아무 경로도 통과하지 못한다.

### 로그인

쿠키 세션이다. 세션은 메모리에 두고 재기동하면 사라진다. 단일 인스턴스를 전제로 한다.

```
POST /api/login  -> 성공하면 Set-Cookie
POST /api/logout -> 세션 삭제
POST /api/setup  -> 사용자명과 비밀번호를 정한다 (설정 전에만)

미들웨어가 /api/** 와 /ui/** 를 막는다
  예외: /api/login, 정적 자산
setup_required 가 true 면 /api/setup 외의 경로를 거부한다
```

## 결정 4 - 내장 UI

HTML, CSS, JS 를 그대로 쓰고 Go 의 `embed` 로 바이너리에 넣는다.
빌드 단계가 안 늘고 `make` 가 그대로다. `Dockerfile`, CI, `.dockerignore` 를 안 건드린다.

```
internal/web/
  static/   index.html, app.js, style.css
  web.go    embed.FS 와 핸들러
```

화면은 넷이다.

| 화면 | 무엇 |
|------|------|
| 로그인 | 설정 전에는 비밀번호만, 설정 뒤에는 사용자명과 비밀번호 |
| 상태 | 터널 목록. desired / total / connected 와 last_error |
| 호스트 | 목록과 추가, 수정, 삭제. enabled 토글 |
| 서비스 포트 | 목록과 추가, 수정, 삭제 |

`/` 를 부르면 `/ui/` 로 리다이렉트한다.

## 안 하는 것

- **`POST` 응답에 터널 결과를 싣는 것.** 즉시 응답이 조정 루프의 전제다. 기다리게 하면 원래대로 돌아간다
- **`models.Tunnel` 외래키.** 루프가 고아 행을 지우므로 DB 제약이 없어도 안 남는다.
  넣으면 기존 DB 에 고아 행이 있을 때 `AutoMigrate` 가 거부해 기동이 막힌다
- **낙관적 잠금(버전 칼럼).** 행 잠금으로 충분하다. 스키마와 API 를 둘 다 바꾸고 클라이언트가 재시도를 다뤄야 한다
- **계정 여러 개.** 사용자 관리 화면과 CRUD 가 한 쌍 더 늘고 첫 계정 생성 경로가 또 필요하다
- **설정 파일의 DB 비밀번호 암호화.** 그 키를 어디서 가져올지가 다시 문제가 된다. 별건이다
- **프론트엔드 프레임워크.** 번들러를 들이면 `Dockerfile` builder 단계, CI, `.dockerignore` 가 전부 바뀐다
- **세션을 DB 나 외부 저장소에 두는 것.** 단일 인스턴스를 전제로 한다

## 되돌리기 어려운 것

- **API 동작 변경.** `POST`/`PUT` 이 터널 실패에 500 을 안 준다. 이 API 를 부르는 쪽이
  그 500 을 보고 무언가 한다면 그게 안 온다
- **인증 추가.** 기존 API 호출부가 전부 로그인을 거쳐야 한다
- **스키마 변경.** `user` 테이블이 는다. `AutoMigrate` 가 만든다
- **설정 항목 추가.** `reconcile.interval_sec`, `auth.initial_password_file`.
  기존 설정 파일은 기본값으로 돈다

## 작업 목록

| # | 작업 | 건드리는 곳 |
|---|------|-------------|
| R1 | 조정 루프 신설, Restore/StopAllTunnels 대체 | `internal/tunnel/manager.go` |
| R7 | 접속 정보가 바뀐 터널을 다시 띄운다 | `internal/tunnel/reconcile.go` |
| R2 | 핸들러에서 터널 조작과 rwLock 제거, 루프 깨우기 | `internal/api/handlers.go` |
| R3 | Update 계열에 행 잠금 | `internal/api/handlers.go` |
| R4 | `reconcile.interval_sec` 추가 | `internal/config/config.go` |
| R5 | `/api/status` 에 `desired_tunnels` | `internal/api/handlers.go` |
| R6 | keepalive 데드라인, 연결 수립 경로 재작성 | `internal/tunnel/ssh.go` |
| A1 | `user` 모델, 첫 기동 임시 비밀번호 생성 | `internal/models`, `main.go` |
| A2 | 로그인, 로그아웃, 세션 미들웨어 | `internal/api` |
| A3 | 사용자명과 비밀번호 설정, 강제 전환 | `internal/api` |
| U1 | `embed` 정적 서빙, 라우팅, `/` 리다이렉트 | `internal/web`, `main.go` |
| U2 | 상태 화면 | `internal/web/static` |
| U3 | 호스트 관리 화면 | `internal/web/static` |
| U4 | 서비스 포트 관리 화면 | `internal/web/static` |
| U5 | 로그인과 초기 설정 화면 | `internal/web/static` |
| D1 | README 갱신 | `README.md` |
