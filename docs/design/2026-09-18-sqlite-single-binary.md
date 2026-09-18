# SQLite 로 바꿔 바이너리 하나로 돌린다

날짜: 2026-09-18 · 갈래: architectural · 상태: 승인됨

## 왜

지금은 MySQL 이나 MariaDB 가 따로 떠 있어야 한다. 이 도구가 하는 일은 SSH 터널 몇 개를
띄우고 살려 두는 것인데, 그것 때문에 데이터베이스 서버를 하나 세우고 계정을 만들고 백업을
챙겨야 한다. 저장하는 것은 호스트 몇 줄, 서비스 포트 몇 줄, 계정 한 줄이다.

바이너리 하나로 돌면 설치가 파일 하나를 놓는 일이 된다.

## 실측으로 확인한 것

설계를 정하기 전에 스크래치 모듈에서 직접 돌렸다. 아래는 그 결과다.

| 확인한 것 | 결과 |
|-----------|------|
| `glebarez/sqlite` + `modernc.org/sqlite` 가 `CGO_ENABLED=0` 으로 교차 빌드되나 | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 **5개 전부 빌드되고 실제로 동작** |
| 라이선스 | `glebarez/sqlite` MIT, `modernc.org/sqlite` 와 `modernc.org/libc` BSD-3-Clause. Apache 2.0 호환 |
| 바이너리 크기 | 프로브 단독 11.1 MB. 현재 바이너리가 15.5 MB 이므로 합치면 대략 10 MB 가량 는다 |
| 지금 모델이 그대로 마이그레이션되나 | `hosts`, `service_ports`, `tunnels`, `user` 네 테이블 생성됨. `user` 라는 이름도 문제 없음 |
| 유니크 인덱스가 실제로 거나 | `hosts.ip` 중복과 `service_ports.local_port` 중복 모두 `UNIQUE constraint failed` |

### `FOR UPDATE` 가 조용히 사라진다

이 설계에서 가장 중요한 사실이다.

```
생성된 SQL: SELECT * FROM `rows` WHERE `id` = ? ORDER BY `id` LIMIT 1
실행 결과: err=<nil>
```

`clause.Locking{Strength: "UPDATE"}` 를 붙여도 SQLite 용 SQL 에는 안 들어가고 **에러도 안 난다.**
커밋 `596898e` 가 넣은 행 잠금 다섯 곳이 말없이 무력화된다.

그래서 그것이 실제로 데이터를 깨뜨리는지 확인했다. 읽기와 쓰기 사이에 장벽을 두어 두
트랜잭션이 반드시 겹치게 만든 뒤 같은 행의 다른 칸을 각각 고치게 했다.

| 모드 | 결과 |
|------|------|
| 기본 | 한쪽이 `SQLITE_BUSY` 로 거부. 데이터는 안 깨짐 |
| WAL + `busy_timeout` | 같음 |
| WAL + `busy_timeout` + `_txlock=immediate` | 두 번째가 `BEGIN` 에서 막혔다가 3초 뒤 `SQLITE_BUSY` |

**변경이 사라지는 일은 어느 모드에서도 없었다.** 대신 실패하는 방식이 바뀐다. MySQL 은 기다렸다가
성공하지만 SQLite 는 `database is locked` 로 거부한다. 지금 코드 그대로면 클라이언트가 500 을 받는다.

## 결정 1 - MySQL 을 버리고 SQLite 로 간다

둘 다 지원하지 않는다. 이 도구는 이미 단일 인스턴스를 전제로 한다. 세션이 메모리에 있고
(`internal/api/auth.go`), 조정 루프가 프로세스 안의 맵을 실제 상태로 삼는다
(`internal/tunnel/reconcile.go`). 프로세스 두 개가 같은 데이터베이스를 보면 그 둘이 서로를
모른 채 같은 터널을 띄운다. MySQL 을 남겨 두어도 쓸 수 있는 구성이 아니다.

드라이버를 import 하는 곳은 `internal/database/database.go:11` 한 곳뿐이다.

## 결정 2 - 쓰기를 프로세스 안에서 한 줄로 세운다

`SQLITE_BUSY` 를 재시도로 다루지 않는다. 위 실측에서 `_txlock=immediate` 로도 3초를 기다린 뒤
거부했다. 재시도는 언제 끝나는지 보장이 없고, 실패가 사용자에게 500 으로 샌다.

대신 쓰기 트랜잭션을 여는 자리 앞에 프로세스 뮤텍스를 하나 세운다. 쓰기는 호스트나 서비스
포트를 등록하고 고치고 지울 때만 일어나고, 그것은 사람이 화면에서 하는 일이라 초당 몇 번을
넘지 않는다. 읽기는 뮤텍스를 안 거치므로 조회와 상태 화면은 그대로 빠르다.

`clause.Locking` 다섯 곳은 **지운다.** 남겨 두면 지키는 것이 없는데 지키는 것처럼 보이는 코드가 된다.
무엇이 그 자리를 대신하는지는 주석으로 남긴다.

## 결정 3 - 데이터베이스 파일은 `/var/lib` 아래에 둔다

홈 폴더가 아니다. 서비스가 root 로 도는데 root 홈에 상태 파일을 두는 것은 FHS 에 어긋나고
운영자가 찾을 자리가 아니다. `/var/lib` 가 애플리케이션 상태 데이터의 자리이고 데이터베이스가
정확히 그것이다. 유닛에는 이미 `StateDirectory=tunnel-manager` 와
`WorkingDirectory=/var/lib/tunnel-manager` 가 있다 (v2.0.1).

설정은 `database.path` 하나이고 기본값은 상대 경로 `data/tunnel-manager.db` 다. 암호화 키와
같은 방식이라 규칙이 하나로 유지된다.

| 띄운 방법 | 작업 디렉터리 | 기본 데이터베이스 파일 |
|-----------|---------------|------------------------|
| 같이 주는 systemd 유닛 | `/var/lib/tunnel-manager` | `/var/lib/tunnel-manager/data/tunnel-manager.db` |
| Docker Compose | `/` | `/data/tunnel-manager.db`, 볼륨으로 `./_data/tunnel-manager` |
| `make run` | 저장소 | `<저장소>/data/tunnel-manager.db` |
| Windows, macOS, 그 밖에 서비스 관리자가 없는 자리 | 실행한 자리 | 그 자리의 `data/tunnel-manager.db` |

절대 경로를 주면 그것이 이긴다. 기동 로그에는 해석된 절대 경로가 찍힌다 (v2.0.1 에서 키에
넣은 것과 같은 이유다).

### Windows 와 macOS 에서는 "실행한 자리" 가 답이다

`/var/lib` 는 Windows 에 없고 macOS 에서도 관례가 아니다. 두 곳에는 유닛이 작업 디렉터리를
정해 주는 일도 없다.

그래서 상대 경로 기본값이 그대로 답이 된다. 바이너리를 폴더에 놓고 실행하면 그 폴더에
`data\tunnel-manager.db` 가 생긴다. 실행 파일 하나와 그 옆의 데이터, 이것이 바이너리 하나로
돌리자는 이번 변경이 노리는 모습이다. 폴더째 옮기면 설정도 데이터도 같이 간다.

주의할 것은 하나다. `C:\` 나 `C:\Windows\System32` 처럼 쓰기 권한이 없거나 어울리지 않는
자리에서 실행하면 데이터베이스가 그 자리를 노린다. 기동 로그가 해석된 절대 경로를 찍으므로
어디에 만들려 했는지는 바로 보이고, 그 자리가 아니면 `database.path` 에 절대 경로를 준다.
README 에 이것을 적는다.

**Windows 와 macOS 에서 실제로 돌려 보지는 않았다.** 다섯 플랫폼 교차 빌드와 이 장비에서의
동작까지가 확인한 범위다. 지금 릴리즈되는 바이너리에 붙어 있는 것과 같은 단서다.

`/etc/tunnel-manager/` 에는 두지 않는다. `/etc` 는 설정 자리지 계속 쓰이는 데이터베이스 자리가 아니다.

## 설정이 어떻게 바뀌나

전:
```yaml
database:
  host: 127.0.0.1
  port: 3307
  user: tunnel-manager
  password: <비밀번호>
  name: tunnel-manager
  timeout_sec: 30
```

후:
```yaml
database:
  path: "data/tunnel-manager.db"
```

`host`, `port`, `user`, `password`, `name`, `timeout_sec` 가 전부 사라진다. 서버에 붙는 일이
없으므로 기다릴 것도 없고, `main.go:52` 의 `initDatabase` 재시도 루프도 없어진다. 파일을 못
열면 그것은 기다려서 풀리는 문제가 아니라 그 자리에서 알려야 하는 문제다.

옛 항목이 그대로 남아 있는 설정 파일은 **기동을 멈추고 무엇을 고쳐야 하는지 말한다.** 조용히
무시하면 운영자가 아직 MySQL 을 보고 있다고 믿는다.

## 이관

이 머신에 배포된 것만 옮긴다. 다른 배포본을 위한 일반 도구는 만들지 않는다.

| 옮길 것 | 어떻게 |
|---------|--------|
| `hosts` | 암호화 키를 그대로 쓴다. 암호문을 건드리지 않고 그대로 넣는다 |
| `service_ports` | 그대로 |
| `user` | bcrypt 해시 그대로. 지금 비밀번호로 계속 로그인된다 |
| `tunnels` | **안 옮긴다.** 기동 때 `RestoreAllTunnels` 가 전부 지우고 다시 만든다 |

옮긴 뒤 화면에서 호스트와 서비스 포트가 보이고 지금 비밀번호로 로그인되는 것을 확인한 **다음에만**
MariaDB 컨테이너와 `_data/mariadb` 를 지운다. 지우기 전에 덤프를 떠 둔다.

## 안 하는 것

- **MySQL 과 SQLite 를 둘 다 지원하는 것.** 저장소 계층을 인터페이스로 추상화하면 코드와
  테스트가 두 배가 되는데, 단일 인스턴스 전제에서 얻는 것이 없다
- **`SQLITE_BUSY` 재시도.** 결정 2 에서 뮤텍스로 대신한다
- **다른 배포본을 위한 일반 이관 도구.** 이 머신 것만 옮긴다
- **여러 프로세스가 같은 데이터베이스 파일을 쓰는 구성.** 지금도 안 되던 것이고 계속 안 된다
- **암호화 키 옮기기.** 키는 자리를 안 바꾼다. 그래야 암호문이 그대로 읽힌다

## 되돌리기 어려운 것

- **설정 파일의 `database` 절 구조 변경.** 옛 설정으로는 새 바이너리가 안 뜨고, 새 설정으로는
  옛 바이너리가 안 뜬다. 롤백하려면 설정도 같이 되돌려야 한다
- **MariaDB 컨테이너와 `_data/mariadb` 삭제.** 이관 검증 뒤에 하고 그 전에 덤프를 뜬다
- **의존성 추가.** `glebarez/sqlite` 와 그것이 끌고 오는 `modernc.org/*`. 라이선스는 확인했다
  (MIT, BSD-3-Clause). 바이너리가 10 MB 가량 는다

## 작업 목록

| # | 작업 | 건드리는 곳 |
|---|------|-------------|
| S1 | 설정 스키마 교체와 옛 항목 거부 | `internal/config/config.go` |
| S2 | 드라이버 교체, 기동 경로에서 재시도 제거 | `internal/database/database.go`, `main.go` |
| S3 | 쓰기 직렬화, `clause.Locking` 제거 | `internal/api/handlers.go`, `internal/api/auth.go` |
| S4 | 배포 자산 갱신 | `docker-compose.yaml`, `Dockerfile`, `_scripts/systemd/`, `config/config.yaml`, `.gitignore` |
| S5 | 이 머신 데이터 이관 | 스크립트, 저장소 밖 |
| S6 | README 영문·한글 | `README.md`, `docs/README.ko.md` |
