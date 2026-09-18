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

## 결정 2 - 커넥션을 하나로 묶어 쓰기를 줄 세운다

SQLite 는 한 번에 한 쓰기만 받는다. 그러므로 기다리게 만드는 장치가 필요하다.

`clause.Locking` 은 그 장치가 아니다. SQLite 용 SQL 에 들어가지도 않고 에러도 안 난다.
**이미 아무 일도 안 하고 있다.** 지우는 것은 보호를 없애는 것이 아니라 일하지 않는 코드를
치우는 것이고, 무엇이 그 자리를 대신하는지는 주석으로 남긴다.

대신할 것은 `db.SetMaxOpenConns(1)` 이다. 핸들러에 뮤텍스를 거는 것으로는 모자란다.
데이터베이스에 쓰는 곳이 API 밖에도 있기 때문이다.

```
internal/tunnel/ssh.go:90, 621
internal/tunnel/manager.go:178, 249     조정 루프
internal/auth/auth.go:145
```

핸들러만 감싸면 조정 루프의 쓰기가 밖에 남는다. 커넥션이 하나면 누가 부르든 풀에서 줄을 서므로
빠뜨릴 자리가 없다.

실측이다. 두 쓰기 트랜잭션에 각각 150ms 를 물려 시간상 겹치는지를 쟀다. 3회 반복 모두 같았다.

| 설정 | 트랜잭션이 겹치나 | 결과 |
|------|-------------------|------|
| 기본 (커넥션 무제한) | 겹친다 | 한쪽이 `SQLITE_BUSY`, 그 변경은 반영 안 됨 |
| `SetMaxOpenConns(1)` | **안 겹친다** | **둘 다 성공하고 둘 다 반영됨** |

두 번째 트랜잭션은 풀에서 기다린다. 이것이 "기다리게 만드는 장치" 다.

읽기도 같은 커넥션을 쓰므로 함께 줄을 선다. 저장하는 것이 호스트 몇 줄과 서비스 포트 몇 줄이고
조회가 그 위를 훑는 정도라 문제가 되는 규모가 아니다. 문제가 되면 그때 읽기 전용 커넥션을
따로 여는 것을 검토한다.

## 결정 3 - 플랫폼이 정한 사용자 데이터 자리를 기본으로 쓴다

작업 디렉터리에 기대지 않는다. 그것이 v2.0.1 에서 암호화 키를 파일시스템 루트의 `/keys` 에
만든 바로 그 함정이다.

기본값은 `os.UserConfigDir()` 아래다. 플랫폼마다 그 자리가 다르고, Go 가 그 차이를 안다.

| 플랫폼 | `os.UserConfigDir()` |
|--------|----------------------|
| Windows | `%AppData%` |
| macOS | `~/Library/Application Support` |
| Linux | `$XDG_CONFIG_HOME`, 없으면 `~/.config` |

바이너리를 내려받아 그냥 실행하면 그 플랫폼이 정한 자리에 `tunnel-manager/tunnel-manager.db`
가 생긴다. 실행한 위치와 상관이 없다.

### 서비스로 깔 때는 설정이 자리를 명시한다

`/root/.config` 는 시스템 데몬의 상태 파일 자리가 아니다. `/var/lib` 가 그 자리다.

그래서 **같이 주는 systemd 유닛과 설정 예시는 절대 경로를 적는다.**

| 상황 | 데이터베이스 파일 |
|------|-------------------|
| 설정에 `database.path` 가 없음 | `os.UserConfigDir()/tunnel-manager/tunnel-manager.db` |
| 같이 주는 systemd 유닛과 설정 | `/var/lib/tunnel-manager/tunnel-manager.db` (설정에 명시) |
| Docker Compose | `/data/tunnel-manager.db` (설정에 명시, 볼륨으로 연결) |

혼자 쓰면 알아서 제자리에 가고, 패키지로 깔면 설정이 자리를 말한다.

### `HOME` 이 없으면 기동을 멈춘다

`os.UserConfigDir()` 은 `XDG_CONFIG_HOME` 도 `HOME` 도 없으면 에러를 준다. 실측:

```
HOME 없음   UserConfigDir="" err=neither $XDG_CONFIG_HOME nor $HOME are defined
```

이때는 자리를 지어내지 않고 **기동을 멈추고 `database.path` 에 절대 경로를 달라고 말한다.**
어딘가에 만들어 두면 다음 기동에 다른 자리를 골라 빈 데이터베이스를 새로 만들 수 있고,
그러면 등록한 호스트가 사라진 것처럼 보인다.

기동 로그에는 해석된 절대 경로가 찍힌다 (v2.0.1 에서 키에 넣은 것과 같은 이유다).

## 결정 4 - 설정을 데이터베이스로 옮긴다

설정 파일에 남는 것은 `database.path` 하나다. 나머지는 전부 `settings` 테이블에 들어가고
Settings 화면에서 고친다.

### 기동 순서를 다시 짠다

설정이 데이터베이스에 있는데 데이터베이스를 열려면 설정이 필요하다. 그 고리를 푸는 것이
이 결정의 실제 내용이다.

지금은 이렇다.

```
설정 파일 전부 읽기 -> 로거 -> 암호화 키 -> 데이터베이스 -> 매니저 -> 서버
```

바꾼 뒤.

```
설정 파일에서 database.path 만 읽기
  -> 임시 로거 (콘솔, info)          데이터베이스를 열다 실패하면 이 로거가 말한다
  -> 데이터베이스 열기 + AutoMigrate
  -> settings 읽기 (없으면 기본값으로 만들어 넣는다)
  -> 진짜 로거를 settings 로 다시 세운다
  -> 암호화 키 (경로는 settings)
  -> 매니저, 조정 루프, 서버 (주기와 포트는 settings)
```

로거가 두 번 서는 것이 이 순서의 값이다. 데이터베이스를 못 열었을 때 그 이유를 말할 곳이
있어야 하는데, 그 시점에 설정은 아직 없다.

### 잘못 저장해서 못 뜨는 것을 막는다

설정이 파일에 있을 때는 잘못 넣어도 파일을 고치면 됐다. 데이터베이스에 있으면 그 파일이 없다.
`api.port` 에 0 을 저장하고 재기동하면 다시 못 뜨고 고칠 화면도 없다.

두 가지로 막는다.

1. **저장할 때 검증한다.** 지금 `config.Validate()` 가 보는 규칙을 그대로 쓴다. 통과 못 하는
   값은 저장되지 않으므로 못 뜨는 상태가 데이터베이스에 들어가지 않는다
2. **`-reset-settings` 플래그를 둔다.** 설정을 기본값으로 되돌리고 종료한다. 1번을 빠져나간
   경우가 생겨도 손이 닿는다

### 즉시 반영되는 것과 재기동이 필요한 것

| 설정 | 반영 |
|------|------|
| `reconcile.interval_sec`, `monitoring.interval_sec` | 다음 주기부터 |
| `logging.level` | 즉시 (zap 의 AtomicLevel) |
| `api.port`, `logging.file.*`, `security.key_file` | **재기동해야 적용** |

재기동이 필요한 항목은 화면에서 그렇게 말한다. 저장은 되지만 지금 도는 프로세스는 안 바뀐다.

## 결정 5 - Uninstall

Settings 화면에 둔다. 누르면 데이터를 지우고 프로세스가 스스로 끝난다.

### 지우는 것

| 지운다 | 안 지운다 |
|--------|-----------|
| 데이터베이스 파일 | **실행 파일** |
| 암호화 키 파일 | |
| 임시 비밀번호 파일 (남아 있으면) | |
| 로그 파일과 회전된 것들 | |
| 설정 파일 | |

**암호화 키를 지우는 것은 되돌릴 수 없다.** 데이터베이스 백업이 있어도 그 안의 SSH 비밀번호는
다시 못 읽는다. 화면이 이것을 그대로 말한다.

실행 파일은 안 지운다. Windows 는 도는 동안 자기 실행 파일을 못 지우고, Unix 에서 지워도
프로세스가 끝날 때까지 남아 있어 절반만 된 일이 된다. 지우는 것은 사람이 한다.

### 실수로 누르는 것을 막는다

**로그인 비밀번호를 다시 입력받는다.** bcrypt 로 확인하고 틀리면 아무것도 안 한다.
세션이 열린 화면을 남이 보고 있을 때 한 번의 클릭으로 끝나지 않게 하는 것이 목적이다.

### 순서

```
1. 비밀번호 확인            틀리면 여기서 끝
2. 터널을 전부 끊는다        원격 호스트에 리스너를 남기지 않는다
3. 조정 루프를 멈춘다        멈추지 않으면 지운 뒤 다시 만든다
4. 데이터베이스 연결을 닫는다
5. 파일들을 지운다
6. 200 을 응답한다          브라우저가 완료 화면을 그릴 수 있게
7. 몇 초 뒤 프로세스 종료
```

**응답이 브라우저에 닿은 뒤에 종료해야 한다.** 그래서 6과 7 사이에 시간을 둔다.

**완료 화면은 서버에 요청하지 않는다.** 그 시점에 서버가 사라지기 때문이다. 화면은 이미
브라우저에 올라와 있는 스크립트가 그린다. 서버로 가는 요청이 하나라도 있으면 그 화면은 못 뜬다.

3번을 먼저 하는 이유는 루프가 목표 상태를 보고 터널을 다시 만들기 때문이다. 끊기만 하고
루프를 안 멈추면 다음 주기에 되살아난다.

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
| S3 | `SetMaxOpenConns(1)`, `clause.Locking` 제거 | `internal/database/database.go`, `internal/api/handlers.go`, `internal/api/auth.go` |
| S4 | 배포 자산 갱신 | `docker-compose.yaml`, `Dockerfile`, `_scripts/systemd/`, `config/config.yaml`, `.gitignore` |
| S5 | 이 머신 데이터 이관 | 스크립트, 저장소 밖 |
| S6 | 설정을 데이터베이스로, 기동 순서 재구성 | `internal/config`, `internal/settings`(신규), `main.go` |
| S7 | Settings 화면 | `internal/web/static`, `internal/api` |
| S8 | Uninstall | `internal/api`, `internal/web/static`, `main.go` |
| S9 | README 영문·한글 | `README.md`, `docs/README.ko.md` |
