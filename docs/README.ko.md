# Tunnel Manager

[English](../README.md)

Tunnel Manager 는 SSH 터널을 열고 그 상태를 계속 유지합니다. 접속할 SSH 서버(**Host**)와
내보낼 서비스(**서비스 포트**)를 등록해 두면, 둘의 모든 조합마다 터널을 하나씩 만들고
그때부터 계속 지켜봅니다. REST API 와 브라우저 UI 와 터널을 한 바이너리가 전부 맡습니다.

여기서 만드는 터널은 **역방향(reverse)** 터널입니다. 즉 대기 소켓이 Tunnel Manager 가 도는
장비가 아니라 **Host 쪽에** 열립니다. Host 의 `local_port` 로 접속한 클라이언트는 SSH 연결을
타고 Tunnel Manager 까지 오고, Tunnel Manager 가 `service_ip:service_port` 로 연결해 양방향으로
바이트를 옮깁니다. Tunnel Manager 만 닿을 수 있는 서비스를 Host 에서 쓸 수 있게 만드는 것이
이 구조입니다.

| 등록하는 것 | 항목 | 무엇인가 |
|-------------|------|----------|
| Host | `ip`, `port`, `user`, `password`, `description`, `enabled` | Tunnel Manager 가 사용자명과 비밀번호로 접속하는 SSH 서버입니다. 비밀번호는 암호화해서 저장합니다. |
| 서비스 포트 | `service_ip`, `service_port`, `local_port` | 내보낼 서비스와, 그 서비스에 닿기 위해 각 Host 에 열 포트입니다. |

`ip` 와 `service_ip` 는 IPv4 와 IPv6 를 모두 받습니다. IPv6 주소는 `2001:db8::1` 처럼 그대로 적고,
연결할 때 필요한 대괄호는 쓰이는 자리에서 붙습니다. `fe80::1%eth0` 같은 영역(zone) 표기는 거부합니다.

**활성(enabled)** Host 하나와 서비스 포트 하나의 조합이 터널 하나입니다. 활성 Host 둘에
서비스 포트 셋이면 터널은 여섯입니다.

**바이너리 말고 따로 설치할 것이 없습니다.** 데이터베이스는 프로세스가 스스로 만드는 SQLite
파일 하나이고, 설정은 전부 그 파일 안에 있으며 브라우저 화면에서 고칩니다. UI 는 실행 파일
안에 들어 있습니다. 데이터베이스 서버도, 설정 파일도, 옆에 딸려 다녀야 할 디렉터리도 없습니다.

## 목차

- [시스템 요구사항](#시스템-요구사항)
- [동작 방식](#동작-방식)
- [설치 및 실행](#설치-및-실행)
- [첫 기동과 계정 설정](#첫-기동과-계정-설정)
- [내장 UI](#내장-ui)
- [설정](#설정)
- [제거](#제거)
- [스크립트에서 API 호출하기](#스크립트에서-api-호출하기)
- [API 엔드포인트](#api-엔드포인트)
- [터널 상태 읽기](#터널-상태-읽기)
- [암호화 키](#암호화-키)
- [비root로 실행하는 경우](#비root로-실행하는-경우)
- [라이선스](#라이선스)

## 시스템 요구사항

릴리즈 바이너리를 실행하는 데는 아무것도 필요 없습니다. SQLite 엔진도 UI 도 바이너리 안에
들어 있습니다.

| 하려는 일 | 필요한 것 |
|-----------|-----------|
| 릴리즈 바이너리 실행 | 없음 |
| 소스에서 빌드 | Go 1.23 이상 |
| 컨테이너로 실행 | Docker 와 Docker Compose |

SQLite 드라이버가 순수 Go 구현(`modernc.org/sqlite` 위의 `github.com/glebarez/sqlite`)이라서
바이너리는 `CGO_ENABLED=0` 으로 빌드되고, 내려받은 장비에 C 라이브러리가 없어도 됩니다.

### 지원 플랫폼

`make release` 가 아래 각각에 대해 바이너리를 하나씩 만듭니다. 그 밖에 Go 가 지원하는 곳에서도
`go build` 로 빌드됩니다. 아래 주의사항만 보면 됩니다.

| 플랫폼 | 릴리즈 바이너리 |
|--------|-----------------|
| Linux amd64 | `tunnel-manager-linux-amd64` |
| Linux arm64 | `tunnel-manager-linux-arm64` |
| macOS 인텔 | `tunnel-manager-darwin-amd64` |
| macOS 애플 실리콘 | `tunnel-manager-darwin-arm64` |
| Windows amd64 | `tunnel-manager-windows-amd64.exe` |

유닉스에서는 기동할 때 프로세스가 열 수 있는 파일 디스크립터 한도를 스스로 올립니다. 터널 하나마다
여러 개를 쓰기 때문입니다. Windows 에는 프로세스마다 걸리는 그런 한도가 없어서 이 단계가 아무것도
하지 않습니다. 그 밖에 다른 점은 없습니다.

같이 들어 있는 systemd 유닛은 Linux 용입니다. 다른 플랫폼에서는 그 시스템이 쓰는 방법으로 프로세스를
계속 띄워 두어야 합니다.

**실제로 돌려 본 것은 Linux 바이너리뿐입니다.** 나머지는 그 플랫폼의 컴파일러와 vet 검사를 통과한
것까지만 확인했습니다.

## 동작 방식

### 조정 루프

Tunnel Manager 는 세상을 두 그림으로 나눠 들고 계속 견줍니다.

| 그림 | 무엇인가 | 어디서 오나 |
|------|----------|-------------|
| 목표 상태 | 활성 Host 전부와 서비스 포트 전부의 조합 | `hosts`, `service_ports` 행 |
| 실제 상태 | 지금 돌고 있는 터널 | 프로세스 안의 매니저와 그것이 쓰는 `tunnels` 행 |

**조정 패스(reconcile pass)** 는 둘을 견줘서 차이만 메웁니다. 목표에 있는데 안 도는 것은
띄우고, 도는데 목표에 없는 것은 끄고, 접속 정보(서버 주소, 원격 주소, 로컬 포트, 사용자,
비밀번호)가 행과 어긋난 채 돌고 있는 터널은 끄고 새 정보로 다시 띄웁니다.

지금 API 요청은 이렇게 처리됩니다.

```text
POST /api/service-port
  트랜잭션 { INSERT INTO service_ports } 커밋
  조정 루프 깨우기
  201 Created          <- 터널을 기다리지 않고 바로 응답한다

조정 루프
  목표 = 활성 Host x 서비스 포트
  실제 = 돌고 있는 터널
  목표에 있는데 안 돈다   -> 띄운다
  도는데 목표에 없다      -> 끈다
  도는데 설정이 옛것이다  -> 끄고 다시 띄운다
```

패스는 세 시점에 돕니다.

| 언제 | 왜 |
|------|-----|
| 기동할 때, API 가 아무것도 응답하기 전에 | 첫 요청이 물어보는 시점에는 저장된 행의 터널이 이미 떠 있다 |
| `POST`·`PUT`·`DELETE` 가 커밋된 직후 | 다음 주기를 기다리지 않고 바로 반영된다 |
| 조정 주기마다 (기본 5초) | 실패한 패스가 못 한 일을 다시 시도한다 |

터널이 생기기 전에 응답이 나가므로, **쓰기가 성공했다고 터널이 떴다는 뜻은 아닙니다.**
그것에 답하는 것은 `GET /api/status` 입니다. [터널 상태 읽기](#터널-상태-읽기)를 보십시오.

### 터널 하나의 동작

```mermaid
sequenceDiagram
    participant Host as Host (SSH server)
    participant Bastion as Tunnel Manager
    participant WAS as Service

    rect rgb(255, 255, 220)
        Note over Host,WAS: Initial Setup Phase
        Bastion->>Host: SSH Authentication (Password)
    end

    rect rgb(255, 255, 220)
        Note over Host,WAS: Tunnel Creation Phase
        Bastion->>Host: Create SSH Tunnel
        Note right of Bastion: For each service port:-R 0.0.0.0:localPort:remoteIP:remotePort
    end

    rect rgb(255, 255, 220)
        Note over Host,WAS: Service Access Phase
        Host->>Host: Connect to localPort (listener bound to 0.0.0.0)
        Host->>Bastion: Forward Traffic through tunnel
        Bastion->>WAS: Forward to remoteIP:remotePort
        WAS-->>Bastion: Response
        Bastion-->>Host: Response through tunnel
    end

    Note over Host,WAS: Monitoring & Auto-reconnect
    loop Every monitoring interval
        Bastion->>Host: keepalive@tunnel check
        alt Connection Lost
            Bastion->>Host: Reconnect SSH Tunnel
        end
    end
```

Host 쪽 리스너가 정말 `0.0.0.0` 으로 열리는지는 Host 의 SSH 서버 설정에 달려 있습니다.
`GatewayPorts` 가 꺼져 있으면 요청한 주소와 관계없이 루프백에만 바인딩되고, 그 사실이
로그에 남습니다.

모니터링 주기와 조정 주기는 다른 일을 합니다. 모니터는 이미 떠 있는 터널이 아직 살아 있는지
물어보고 죽었으면 다시 잇습니다. 조정 루프는 떠 있어야 할 터널이 애초에 전부 있는지를 봅니다.

## 설치 및 실행

**설치가 파일 하나를 놓는 일입니다.** 릴리즈 페이지에서 플랫폼에 맞는 바이너리를 내려받아
실행 권한을 주고 띄우면 됩니다. 데이터베이스 파일도 계정도 나머지 전부도 첫 기동이 알아서
만듭니다.

```bash
chmod +x tunnel-manager-linux-amd64
./tunnel-manager-linux-amd64
```

플래그는 이것이 전부입니다.

| 플래그 | 하는 일 |
|--------|---------|
| `-db <경로>` | 데이터베이스 파일. 설정과 등록한 호스트와 계정이 전부 이 안에 있으며, 없으면 상위 디렉터리까지 만들어 생성합니다 |
| `-reset-settings` | 저장된 설정을 전부 기본값으로 되돌리고, 무엇이 바뀌었는지 찍고 종료합니다. [서버가 안 뜰 때](#서버가-안-뜰-때) 참조 |
| `-version` | 버전을 찍고 종료합니다 |
| `-help` | 플래그를 찍고 종료합니다 |

### 파일이 어디에 생기나

**디렉터리 하나가 이 설치의 전부입니다.** `-db` 가 데이터베이스 파일을 지정하고, 이 설치를
이루는 나머지 전부가 그 파일이 있는 디렉터리에 들어갑니다.

```text
<데이터베이스 파일이 있는 디렉터리>/
    tunnel-manager.db          설정, Host, 서비스 포트, 계정
    tunnel-manager.db-wal      SQLite 가 옆에 두는 쓰기 선행 로그
    tunnel-manager.db-shm      SQLite 가 옆에 두는 공유 메모리 파일
    initial-password           첫 기동이 쓰고 계정 설정이 지웁니다
    keys/tunnel-manager.key    SSH 비밀번호를 암호화하는 키
    logs/tunnel-manager.log    로그 파일과 그 옆의 회전된 파일들
```

키 파일과 로그 파일은 플래그가 아니라 **설정**입니다. Settings 화면에 있고 기본값은 각각
`keys/tunnel-manager.key` 와 `logs/tunnel-manager.log` 입니다. **설정에 적은 상대 경로는
작업 디렉터리가 아니라 데이터베이스 파일이 있는 디렉터리를 기준으로 읽습니다.** 작업
디렉터리는 띄우는 방법마다 달라서, 상대 경로를 그것에 맞춰 읽으면 장비마다 키가 다른 자리에
생깁니다. 설정에 절대 경로를 주면 그 경로가 이기며, 키나 로그를 설치 디렉터리 밖에 일부러
둘 때 그렇게 합니다.

**프로세스를 띄운 디렉터리에는 아무것도 생기지 않습니다.**

이 데이터베이스에 행으로 들어가지 않고 파일로 남는 것은 로그 하나뿐이고, 이유는 셋입니다.
로거는 데이터베이스를 열기 전에 서 있어야 합니다. 데이터베이스를 여는 단계가 가장 실패하기
쉬운 단계이고, 그 이유를 말할 곳이 있어야 하기 때문입니다. 커넥션 풀에 커넥션이 하나라서,
로그 한 줄마다 프로세스가 원래 하려던 질의들 뒤에 줄을 서게 됩니다. 그리고 데이터베이스가
자기 질의를 바로 그 로거로 찍기 때문에, 로그를 남기는 일이 로그를 남기는 질의가 됩니다.

`-db` 를 안 주면 그 플랫폼이 사용자 데이터를 두는 자리에서 경로를 만듭니다.

| 띄운 방법 | `-db` | 설치가 들어앉는 디렉터리 |
|-----------|-------|---------------------------|
| 플래그 없이, Windows | 안 줌 | `%AppData%\tunnel-manager\` |
| 플래그 없이, macOS | 안 줌 | `~/Library/Application Support/tunnel-manager/` |
| 플래그 없이, Linux | 안 줌 | `$XDG_CONFIG_HOME/tunnel-manager/`, 그 변수가 없으면 `~/.config/tunnel-manager/` |
| 같이 주는 systemd 유닛 | `/var/lib/tunnel-manager/tunnel-manager.db` | `/var/lib/tunnel-manager/` |
| Docker Compose | `/data/tunnel-manager.db` | `/data/`. compose 파일이 호스트의 `./_data` 에 연결합니다 |

`$XDG_CONFIG_HOME` 도 `$HOME` 도 없는 장비에는 그런 자리가 없습니다. 이때는 자리를 지어내지
않고 그 사실을 말합니다.

```text
Failed to work out where the database file goes: no default location for the database file
is available: neither $XDG_CONFIG_HOME nor $HOME are defined. Give -db an absolute path
```

기동 로그에는 실제로 연 데이터베이스 파일·키 파일·로그 파일의 절대 경로가 찍히므로, 어느
파일을 쓰고 있는지는 로그를 보면 항상 알 수 있습니다.

### Docker Compose 를 쓰는 경우

```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
docker-compose up -d
```

이미지는 바이너리를 `-db /data/tunnel-manager.db` 로 띄우고, compose 파일이 `/data` 를
호스트의 `./_data` 에 연결합니다. 컨테이너를 지웠다 다시 만들어도 데이터베이스와 키와
로그가 남는 이유가 이것입니다.

임시 비밀번호 파일도 그 디렉터리에 써지므로 호스트에서도 읽을 수 있습니다.

```bash
docker compose exec tunnel-manager cat /data/initial-password
```

### systemd 서비스로 돌리는 경우

`_scripts/systemd/tunnel-manager.service` 는 `-db` 를 절대 경로로 줍니다.

```ini
ExecStart=/usr/local/bin/tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db
StateDirectory=tunnel-manager
```

`StateDirectory=tunnel-manager` 가 `/var/lib/tunnel-manager` 를 만들어 `User=` 의 계정에게
넘겨주고, 데이터베이스와 키와 로그와 임시 비밀번호가 전부 그 안에 들어갑니다. 유닛은
`User=root` 로 배포되니 계정을 바꾸려면 [비root로 실행하는 경우](#비root로-실행하는-경우)를
보십시오.

### 소스에서 빌드하기

```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
make
./tunnel-manager
```

`make` 는 `CGO_ENABLED=0` 으로 바이너리를 만듭니다. `make release` 는 위 표의 플랫폼마다
하나씩 만듭니다.

## 첫 기동과 계정 설정

**API 와 UI 는 로그인 뒤에 있습니다.** 계정은 하나이고 첫 기동 때 만들어집니다. 첫 기동
전에 이 절을 읽으십시오. 모르면 로그인을 못 합니다.

1. 첫 기동이 `user` 테이블의 유일한 행을 만듭니다. 이 행에는 **아직 사용자명이 없고**
   설정이 필요한 상태로 표시됩니다.
2. 임시 비밀번호는 **데이터베이스 파일이 있는 디렉터리**의 `initial-password` 파일에 권한
   `0600` 으로 써집니다. 대문자와 숫자 52자입니다.
3. **로그에는 경로만 남고 값은 안 남습니다.** 로그는 콘솔에도 나가고 보관·회전되는 파일에도
   쌓이므로, 거기에 비밀번호를 적으면 아무도 안 보는 곳에 계정 설정보다 오래 남습니다.
   파일이 유일한 사본입니다.
4. 그 비밀번호로 로그인합니다. 이 시점에는 사용자명을 보지 않으므로 빈 값으로 보냅니다.
   응답에 `"setup_required": true` 가 실리고 UI 는 설정 화면으로 갑니다. 거기서 계정이
   계속 쓸 사용자명과 비밀번호를 정합니다.
5. **설정이 끝나면 임시 비밀번호 파일이 지워집니다.** 그 시점에 파일 속 비밀번호는 계정을
   더 이상 열지 못하므로, 남겨 두면 읽을 수 있는 죽은 자격증명 사본일 뿐입니다. 임시
   비밀번호로 열린 다른 세션도 같이 끊깁니다. 설정을 하고 있는 그 세션만 남습니다.
6. **설정이 끝나기 전에는 세션이 `POST /api/setup` 말고 아무것도 못 부릅니다.** `/api` 아래
   다른 경로는 전부 `403` 과 `The account setup is not finished` 로 응답합니다.

설정에서 정하는 비밀번호는 **12바이트 이상 72바이트 이하**여야 합니다. 글자 수가 아니라
바이트 수입니다. 한글 한 글자는 3바이트입니다. 상한이 72인 이유는 비밀번호를 해시하는
bcrypt 가 앞의 72바이트까지만 읽기 때문입니다. 그 뒤는 로그인에서 검사되지 않으므로,
조용히 잘라 쓰지 않고 거부합니다.

설정은 **한 번만** 됩니다. 현재 비밀번호를 묻지 않기 때문에 나중에 자격증명을 바꾸는
통로가 아니며, 두 번째 호출은 `409` 로 응답합니다.

세션은 마지막으로 쓰인 시점부터 12시간 살아 있고, 요청이 있을 때마다 그 시한이 미뤄집니다.
세션은 메모리에만 있으므로 재기동하면 전부 사라지고 다시 로그인해야 합니다.

## 내장 UI

브라우저로 `http://<주소>:<포트>/` 를 엽니다. `/` 는 `/ui/` 로 리다이렉트하고, UI 는
거기서 제공됩니다.

**따로 배포할 것이 없습니다.** 화면 파일이 바이너리 안에 들어 있어서, 옆에 딸려 다녀야 할
디렉터리도 없고 설정할 경로도 없습니다.

| 화면 | 경로 | 무엇을 보여주고 무엇을 하나 |
|------|------|------------------------------|
| 상태 | `/ui/status` | 숫자 셋(목표, 행, 연결됨)과 그 차이를 설명하는 한 줄, 그리고 터널마다 한 줄씩 Host, 서비스 포트, 상태, 서버, 로컬, 원격, 재시도 횟수, 마지막 연결 시각, 마지막 오류. 5초마다 다시 물어봅니다. |
| 호스트 | `/ui/hosts` | Host 마다 한 줄씩 ID, IP, 포트, 사용자, 설명, 활성 여부, 수정 시각. 추가, 수정, 활성·비활성 전환, 삭제를 합니다. |
| 서비스 포트 | `/ui/service-ports` | 서비스 포트마다 한 줄씩 ID, 서비스 IP, 서비스 포트, 로컬 포트, 설명, 수정 시각. 추가, 수정, 삭제를 합니다. |
| 로그 | `/ui/logs` | 로그 파일의 끝부분. 최신 줄이 아래입니다. 레벨 필터와 보여줄 줄 수를 고를 수 있고 5초마다 다시 물어봅니다. 지금 쓰고 있는 파일만 읽고 회전된 파일은 안 보여줍니다. |
| 설정 | `/ui/settings` | 저장된 설정 전부와, 저장이 무엇을 바꿨고 그것이 지금 반영됐는지, 지금 서비스 중인 인증서와 갱신 버튼·수동 등록 칸, 그리고 맨 아래에 제거. [설정](#설정) 참조 |
| 로그인 | `/ui/login` | 세션이 없는 클라이언트가 도착하는 화면. 첫 로그인에서는 사용자명을 비워 둡니다. 계정 설정이 아직이면 설정 화면으로 이어집니다. |

바이너리의 버전이 모든 화면 오른쪽 아래에 보입니다. 로그인 화면도 마찬가지입니다.

폼은 보내기 전에 입력을 검사합니다. 포트는 숫자만 받고 1 에서 65535 사이여야 하며, IP 칸은
주소를 이루는 문자만 받고 IPv4 나 IPv6 로 읽혀야 합니다. 무엇이 잘못됐는지는 그 칸 옆에 적히고,
맞을 때까지 브라우저 밖으로 아무것도 나가지 않습니다.

UI 파일은 일부러 세션 없이 제공합니다. 누구에게나 같은 바이트이고 그 자체에 데이터가 없기
때문입니다. 화면이 보여주는 내용은 전부 `/api/**` 에서 가져오고, 로그인이 지키는 것은
그쪽입니다.

## 설정

**설정 파일이 없습니다.** 모든 설정은 데이터베이스 파일 안에 있는 `settings` 행 하나의
칸이고, 고치는 자리는 Settings 화면입니다. 같은 값을 `GET /api/settings` 와
`PUT /api/settings` 로도 읽고 쓸 수 있습니다.

| 화면 표기 | API 항목 | 변경 보고 이름 | 기본값 | 언제 반영되나 |
|-----------|----------|----------------|--------|---------------|
| API port | `api_port` | `api.port` | `8888` | 다음 기동부터 |
| Serve over HTTPS | `api_https_enabled` | `api.https_enabled` | `true` | 다음 기동부터 |
| Monitoring interval (seconds) | `monitoring_interval_sec` | `monitoring.interval_sec` | `5` | 다음 기동부터 |
| Reconcile interval (seconds) | `reconcile_interval_sec` | `reconcile.interval_sec` | `5` | 다음 기동부터 |
| Encryption key file | `security_key_file` | `security.key_file` | `keys/tunnel-manager.key` | 다음 기동부터 |
| Log level | `logging_level` | `logging.level` | `info` | **저장하는 즉시** |
| Log format | `logging_format` | `logging.format` | `json` | 다음 기동부터 |
| Log file | `logging_file_path` | `logging.file.path` | `logs/tunnel-manager.log` | 다음 기동부터 |
| Log size before rotation (MB) | `logging_file_max_size` | `logging.file.max_size` | `100` | 다음 기동부터 |
| Rotated files kept | `logging_file_max_backups` | `logging.file.max_backups` | `5` | 다음 기동부터 |
| Days a rotated file is kept | `logging_file_max_age` | `logging.file.max_age` | `30` | 다음 기동부터 |
| Compress rotated files | `logging_file_compress` | `logging.file.compress` | `false` | 다음 기동부터 |

**도는 프로세스가 바로 받아들이는 설정은 로그 레벨 하나뿐입니다.** 기동할 때 만들어진 모든
로거에 닿으며, 데이터베이스가 질의를 찍는 로거도 포함됩니다. `debug` 를 켜는 이유의 절반이
그 질의 로그입니다. 나머지는 저장만 되고 다음 기동에 읽힙니다. 화면이 항목마다 그렇게 적고,
저장 응답도 변경마다 `now` 인지 `restart` 인지 표시합니다.

값이 규칙을 어기면 저장되기 전에 거부됩니다.

| 항목 | 규칙 |
|------|------|
| `api_port` | 1 에서 65535 |
| `monitoring_interval_sec`, `reconcile_interval_sec` | 0 보다 커야 함 |
| `security_key_file` | 비어 있으면 안 됨 |
| `logging_level` | `debug`, `info`, `warn`, `error`, `dpanic`, `panic`, `fatal` 중 하나 |
| `logging_format` | `json` 또는 `console` |
| `logging_file_max_size`, `logging_file_max_backups`, `logging_file_max_age` | 0 이상 |

```bash
curl -s -b cookies.txt -X PUT "$BASE/api/settings" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"api_port":9999,"logging_level":"debug"}'
```

```json
{
  "success": true,
  "data": {
    "settings": { "api_port": 9999, "logging_level": "debug", "...": "..." },
    "changes": [
      { "name": "api.port", "from": "8888", "to": "9999", "applied": "restart" },
      { "name": "logging.level", "from": "info", "to": "debug", "applied": "now" }
    ],
    "restart_required": true
  }
}
```

요청 본문은 저장된 값 위에 덮이므로, 일부 항목만 적어 보내면 그것만 바뀌고 나머지는
그대로 남습니다.

### 서버가 안 뜰 때

설정이 파일에 있을 때는 잘못 넣어도 파일을 고치면 됐습니다. 이제는 데이터베이스 안에 있고,
그것을 고칠 화면은 뜨지 않는 그 서버가 제공합니다. 빠져나오는 길이 `-reset-settings` 입니다.

```bash
./tunnel-manager -db <경로> -reset-settings
```

설정을 전부 기본값으로 되돌리고, 무엇이 바뀌었는지 찍고 종료합니다. 다음 기동은 기본값으로
돌고 Settings 화면에 다시 닿을 수 있습니다. 저장된 값이 위 규칙을 어겨서 못 뜰 때는 그
사실과 이 플래그를 함께 말합니다.

```text
fatal  failed to read the settings  {"error": "the stored settings are refused: invalid API port: 0.
       Start with -reset-settings to put every setting back to its default"}
```

## 제거

Settings 화면 맨 아래에서 이 설치를 지웁니다. 터널을 전부 끊고, 설치를 이루는 파일들을
지우고, 프로세스가 끝납니다. 스크립트에서는 `POST /api/uninstall` 이 같은 일을 합니다.

> **암호화 키를 지우는 것은 되돌릴 수 없습니다.** 모든 Host 의 SSH 비밀번호가 그 키로
> 봉인돼 있습니다. 미리 떠 둔 데이터베이스 백업이 있어도 소용없습니다. 그 안의 비밀번호는
> 계속 못 읽고, 새로 설치한 곳에 Host 를 비밀번호와 함께 전부 다시 등록해야 합니다.

| 지웁니다 | 그대로 둡니다 |
|----------|---------------|
| 데이터베이스 파일과, SQLite 가 옆에 두는 `-wal`·`-shm` 파일 | **실행 파일** |
| 암호화 키 파일 | 그것을 띄우는 서비스 등록 |
| 임시 비밀번호 파일 (아직 남아 있으면) | 파일들이 있던 디렉터리 |
| 로그 파일과 그 옆의 회전된 로그 파일들 | |

**실행 파일은 지우지 않습니다.** Windows 는 도는 프로세스가 자기 실행 이미지를 못 지우고,
유닉스에서도 프로세스가 끝날 때까지 디스크에 남아서 절반만 된 일이 됩니다. 실행 파일은
손으로 지우고, 서비스로 등록했다면 systemd 유닛이나 compose 파일도 같이 지우십시오.

**계정의 비밀번호를 다시 입력받고** 아무것도 건드리기 전에 확인합니다. 그러지 않으면 자리를
비운 사이 열려 있는 화면이 한 번의 클릭으로 이 일을 하게 되고, 지나가는 사람이 댈 수 없는
것이 비밀번호이기 때문입니다. 틀리면 `401` 을 주고 아무것도 멈추지 않습니다.

순서가 이 기능의 전부이고 고정돼 있습니다. 조정 루프를 먼저 멈춥니다. 터널을 다시 띄우는
것이 그것이기 때문입니다. 그다음 터널을 끊어서 원격 호스트에 리스너를 남기지 않고, 그다음
데이터베이스를 닫고 파일을 지우고, 그다음 응답을 씁니다. 프로세스는 그로부터 3초쯤 뒤에
끝납니다. 브라우저가 응답을 받을 시간을 두기 위해서입니다.

```bash
curl -s -b cookies.txt -X POST "$BASE/api/uninstall" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"password":"<your-password>"}'
```

```json
{
  "success": true,
  "data": {
    "removed": [
      { "path": "/var/lib/tunnel-manager/tunnel-manager.db", "what": "the database" },
      { "path": "/var/lib/tunnel-manager/keys/tunnel-manager.key", "what": "the encryption key" },
      { "path": "/var/lib/tunnel-manager/logs/tunnel-manager.log", "what": "the log file" }
    ],
    "failed": [],
    "exit_in_sec": 3
  }
}
```

지우지 못한 파일은 이유와 함께 `failed` 에 실리고 디스크에 남습니다. 손으로 정리하십시오.
그 하나 때문에 나머지를 멈추지는 않습니다. Windows 에서는 이 프로세스가 쓰고 있는 로그
파일을 열린 채로 지울 수 없는데, 거기서 멈추면 애초에 지워질 수 없던 파일 하나 때문에
데이터베이스와 키가 남게 됩니다.

## 스크립트에서 API 호출하기

**`/api` 아래 모든 경로는 세션이 필요하고, `POST`·`PUT`·`DELETE` 는 CSRF 토큰도 필요합니다.**

1. `POST /api/login` 에 사용자명과 비밀번호를 보냅니다. 서버가 내려주는 쿠키
   `tm_session` 과 `tm_csrf` 를 보관합니다.
2. 응답의 `data.csrf_token` 을 꺼내서 **모든** `POST`·`PUT`·`DELETE` 에 `X-CSRF-Token`
   헤더로 붙입니다.
3. `GET` 은 토큰이 필요 없습니다. 바꾸는 것이 없기 때문입니다.

CSRF 는 cross site request forgery, 즉 다른 사이트가 내 브라우저를 시켜 내 쿠키가 붙은
요청을 보내게 하는 공격입니다. 그 사이트는 내 로그인 응답을 읽을 수도 없고 헤더를 붙일
수도 없으므로, 토큰을 검사하면 막힙니다.

서버는 `Access-Control-Allow-Origin` 헤더를 하나도 보내지 않습니다. CORS 는 브라우저가
페이지에 적용하는 규칙이지 이 서버가 하는 검사가 아니므로 `curl` 과 스크립트와 서버 간
호출은 영향이 없고, 다른 출처의 브라우저 페이지만 막힙니다.

아래 예제가 한 세션 전체입니다. 쿠키 항아리 파일을 씁니다. `-c` 는 서버가 내려준 쿠키를
적고, `-b` 는 그것을 다시 보냅니다.

```bash
BASE=http://127.0.0.1:8888

# 1. 로그인한다. -c 가 tm_session 과 tm_csrf 를 cookies.txt 에 적는다.
curl -s -c cookies.txt -X POST "$BASE/api/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"operator","password":"<your-password>"}' > login.json

# {"success":true,"data":{"setup_required":false,"csrf_token":"<token>"}}

# 2. 응답에서 토큰을 꺼낸다.
CSRF=$(python3 -c 'import json; print(json.load(open("login.json"))["data"]["csrf_token"])')

# 3. 읽기는 쿠키만 있으면 된다.
curl -s -b cookies.txt "$BASE/api/status"

# 4. 쓰기는 헤더까지 필요하다.
curl -s -b cookies.txt -X POST "$BASE/api/host" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"ip":"192.0.2.10","port":22,"user":"ubuntu","password":"<host-password>","description":"example"}'

curl -s -b cookies.txt -X POST "$BASE/api/service-port" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"service_ip":"198.51.100.20","service_port":8080,"local_port":18080}'

# 5. 스크립트가 끝나면 로그아웃한다.
curl -s -b cookies.txt -X POST "$BASE/api/logout" -H "X-CSRF-Token: $CSRF"
```

맨 처음 로그인할 때는 사용자명을 비우고 임시 비밀번호로 로그인한 다음, 다른 것을 하기
전에 계정 설정부터 끝냅니다. `initial-password` 는 데이터베이스 파일이 있는 디렉터리에
있습니다.

```bash
curl -s -c cookies.txt -X POST "$BASE/api/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"\",\"password\":\"$(cat initial-password)\"}" > login.json

CSRF=$(python3 -c 'import json; print(json.load(open("login.json"))["data"]["csrf_token"])')

curl -s -b cookies.txt -X POST "$BASE/api/setup" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"username":"operator","password":"<your-password>"}'
```

무엇이 잘못됐을 때 어떻게 보이는지는 이렇습니다.

| 응답 | 무슨 뜻인가 |
|------|-------------|
| `401 Authentication required` | 세션 쿠키를 안 보냈거나 세션이 만료됐습니다. 다시 로그인하십시오. |
| `403 The request carries no valid X-CSRF-Token header...` | 쓰기에 토큰이 없거나 틀렸습니다. 로그인 응답의 `data.csrf_token` 을 보내십시오. |
| `403 The account setup is not finished...` | 계정에 아직 사용자명이 없습니다. `POST /api/setup` 을 먼저 부르십시오. |
| `401 Invalid username or password` | 로그인이 거부됐습니다. 둘 중 무엇이 틀렸는지는 일부러 알려주지 않습니다. |
| `400 The settings are refused: ...` | 설정이 위 규칙 중 하나를 어겼습니다. 아무것도 저장되지 않았습니다. |
| `401 The password does not open this account` | 제거 요청의 비밀번호가 틀렸습니다. 아무것도 멈추지 않았고 아무것도 지워지지 않았습니다. |

모든 응답은 같은 모양입니다. `{"success":true,"data":...}` 또는
`{"success":false,"error":"..."}` 입니다.

## API 엔드포인트

`POST /api/login` 을 뺀 `/api` 아래 전부가 세션을 요구합니다. `GET` 이 아닌 것은 전부
`X-CSRF-Token` 헤더를 요구합니다.

### 계정

| 메서드 | 경로 | 하는 일 |
|--------|------|---------|
| `POST` | `/api/login` | `username` 과 `password` 를 받아 세션·CSRF 쿠키를 내리고 `setup_required` 와 `csrf_token` 을 응답 |
| `POST` | `/api/logout` | 세션을 지우고 쿠키 둘을 만료시킴 |
| `POST` | `/api/setup` | 아직 설정이 필요한 계정에 사용자명과 비밀번호를 한 번 정함 |

### Host 관리

| 메서드 | 경로 | 하는 일 |
|--------|------|---------|
| `POST` | `/api/host` | Host 생성 |
| `GET` | `/api/host` | Host 목록 조회 |
| `GET` | `/api/host/:id` | 특정 Host 조회 |
| `PUT` | `/api/host/:id` | Host 수정. 모든 항목이 선택이며, `enabled` 를 false 로 하면 그 Host 의 터널이 멈춤 |
| `DELETE` | `/api/host/:id` | Host 삭제 |

### 서비스 포트 관리

| 메서드 | 경로 | 하는 일 |
|--------|------|---------|
| `POST` | `/api/service-port` | 서비스 포트 생성 |
| `GET` | `/api/service-port` | 서비스 포트 목록 조회 |
| `GET` | `/api/service-port/:id` | 특정 서비스 포트 조회 |
| `PUT` | `/api/service-port/:id` | 서비스 포트 수정. `service_ip`, `service_port`, `local_port` 가 모두 필수 |
| `DELETE` | `/api/service-port/:id` | 서비스 포트 삭제 |

### 상태 조회

| 메서드 | 경로 | 하는 일 |
|--------|------|---------|
| `GET` | `/api/status` | 숫자 셋과 전체 터널 |
| `GET` | `/api/status/:hostId` | 그 Host 와 그 Host 의 터널 |

### 설정과 제거

| 메서드 | 경로 | 하는 일 |
|--------|------|---------|
| `GET` | `/api/settings` | 저장된 설정 조회 |
| `PUT` | `/api/settings` | 본문의 설정을 저장된 값 위에 덮고, 무엇이 바뀌었는지와 재기동이 필요한지를 응답 |
| `GET` | `/api/certificate` | 지금 서비스 중인 인증서. 지문, subject, 발급자, 포함된 이름들, 유효기간, 남은 일수 |
| `POST` | `/api/certificate/renew` | 자체 서명 인증서를 새로 만들어 다음 연결부터 서비스 |
| `PUT` | `/api/certificate` | `cert_pem` 과 `key_pem` 을 받아 저장하고 다음 연결부터 서비스 |
| `POST` | `/api/uninstall` | `password` 를 받아 설치를 지우고 프로세스를 끝냄 |
| `GET` | `/api/logs` | 로그 파일의 끝부분. `lines` 로 줄 수를 정하고 상한은 2000 입니다 |

인증서 관련 세 호출은 `api_https_enabled` 가 꺼져 있으면 `409` 로 응답합니다. 쓰는 인증서가
없기 때문입니다. 조회든 교체든 응답에 개인키는 들어가지 않습니다. 개인키는 Host 의 SSH
비밀번호와 같은 암호화 키로 봉해져 저장되고 프로세스 밖으로 나가지 않습니다. `cert_pem` 에는
체인을 넣을 수 있고, 서버 인증서가 먼저 오고 중간 CA 가 그 뒤에 옵니다.

교체는 다음 연결부터 적용되고 이미 열려 있는 연결에는 적용되지 않습니다. TLS 는 핸드셰이크에서
인증서를 정하고 그 뒤로는 다시 보지 않기 때문입니다. 그래서 갱신 요청의 응답은 옛 인증서 위로
돌아오고, 브라우저는 페이지를 다시 읽기 전까지 옛 지문을 계속 보여줍니다. 교체가 일어나면 옛
지문과 새 지문이 함께 로그에 남습니다.

`/api/logs` 의 응답에는 줄과 함께 그것을 어떻게 가져왔는지가 실립니다. `path` 는 읽은 파일,
`requested` 와 `max_lines` 는 요청한 줄 수와 상한, `capped` 는 그 상한이 실제로 걸렸는지,
`size` 와 `read` 는 파일 크기와 그 끝에서 실제로 읽은 양입니다. 파일을 끝에서부터 읽기 때문에
파일이 아무리 커도 `read` 는 작게 유지됩니다.

각 줄은 `level`, `time`, `caller`, `message`, `extra` 로 나뉘어 오고, `raw` 에 써진 그대로가,
`parsed` 에 나누기가 됐는지가 담깁니다. 나누지 못한 줄도 `parsed` 를 false 로 해서 그대로
돌려줍니다. 이상해 보이는 줄이야말로 읽을 가치가 있는 줄이기 때문입니다.

### UI

| 메서드 | 경로 | 하는 일 |
|--------|------|---------|
| `GET` | `/` | `302` 로 `/ui/` 로 보냄 |
| `GET` | `/ui` | `302` 로 `/ui/` 로 보냄 |
| `GET` | `/ui/version.json` | 바이너리의 버전. `{"version":"3.0.0"}` |
| `GET` | `/ui/*` | 바이너리 안의 UI 를 제공 |

`/ui/version.json` 은 `/ui/` 의 나머지와 마찬가지로 세션 없이 열립니다. 로그인 화면에도 버전이
보여야 하고, 그 번호는 공개 저장소의 릴리즈 페이지에 어차피 있습니다.

Host 의 SSH 비밀번호와 계정의 비밀번호 해시는 어떤 응답에도 실리지 않습니다.

## 터널 상태 읽기

```bash
curl -s -b cookies.txt http://127.0.0.1:8888/api/status
```

```json
{
  "success": true,
  "data": {
    "desired_tunnels": 1,
    "total_tunnels": 1,
    "connected_tunnels": 0,
    "tunnels": [
      {
        "host_id": 1,
        "sp_id": 1,
        "status": "starting",
        "last_error": "",
        "retry_count": 0,
        "last_connected_at": "0001-01-01T00:00:00Z",
        "server": "192.0.2.10:22",
        "local": "0.0.0.0:18080",
        "remote": "198.51.100.20:8080"
      }
    ]
  }
}
```

숫자 셋은 서로 다른 세 질문에 답하고, 그 사이의 차이도 뜻이 다릅니다.

| 숫자 | 무엇을 세나 |
|------|-------------|
| `desired_tunnels` | 떠 **있어야 할** 터널 수. 활성 Host 곱하기 서비스 포트이며, 조정 패스가 목표 상태를 만드는 방식 그대로 셉니다 |
| `total_tunnels` | 터널 **행**의 수. 어떤 상태로 끝났든 한 번이라도 시작된 터널마다 하나씩 있습니다 |
| `connected_tunnels` | 그 행들 중 `connected` 인 것의 수 |

| 차이 | 무슨 뜻인가 |
|------|-------------|
| `desired > total` | 떠 있어야 할 터널이 아예 시작되지도 않았습니다. 아직 패스가 안 돈 순간이거나, 패스가 시작을 못 한 것입니다. 후자의 예로 저장된 비밀번호가 지금 쓰는 암호화 키로 안 열리는 경우가 있습니다. 이유는 로그에 있습니다. |
| `total > connected` | 시작은 됐는데 트래픽을 나르지 못하고 있습니다. 이유는 그 행의 `status` 와 `last_error` 에 있습니다. |

`status` 는 넷 중 하나입니다.

| 값 | 뜻 |
|----|-----|
| `starting` | 행은 써졌고 SSH 연결을 만드는 중 |
| `connected` | Host 에 리스너가 열려 있음 |
| `reconnecting` | 연결이 끊겼거나 keepalive 에 응답이 없어 다시 잇는 중 |
| `error` | 시도가 실패함. 이유는 `last_error` 에 있음 |

`GET /api/status/:hostId` 는 `desired_tunnels` 를 뺀 같은 숫자들과 그 Host 자체를 줍니다.

## 암호화 키

Host 의 SSH 비밀번호는 AES-256-GCM 으로 암호화해서 저장합니다. 키는 **Encryption key file**
설정이 가리키는 파일에서 읽고 기본값은 `keys/tunnel-manager.key` 입니다. 그 파일이 없으면 첫
기동이 32바이트 키를 만들어 권한 `0600` 으로 저장하고, 있으면 그대로 읽습니다.

> **키를 잃어버리면 저장된 비밀번호를 하나도 복호할 수 없습니다.** 등록된 Host 를 전부 다시
> 등록하는 것 말고는 방법이 없습니다. 데이터베이스 파일을 백업할 때 키 파일도 같이 백업해야
> 짝이 맞습니다.

키 파일을 그룹이나 다른 사용자가 읽을 수 있으면 기동을 거부합니다. `chmod 600` 으로 권한을
좁힌 뒤 다시 실행하십시오.

키로 저장된 비밀번호가 하나도 안 열리고 그중 암호화된 표시가 붙은 것이 하나라도 있으면,
기동을 멈춥니다. 겉보기에 멀쩡한 API 가 뜨는데 어떤 Host 에도 접속할 수 없는 상태를 만들지
않기 위해서입니다. 일부만 열리면 안 열리는 것들을 경고에 이름으로 남기고, 그 터널은 만들지
않으며, 저장된 값은 그대로 둡니다. 안 열리는 비밀번호는 다른 어디에도 없어서 덮어쓰면 영영
사라지기 때문입니다. 그 Host 들은 API 로 비밀번호를 다시 설정하십시오.

이 설정의 상대 경로는 데이터베이스 파일이 있는 디렉터리를 기준으로 읽습니다. 그래서
기본값이면 키가 데이터베이스 옆의 `keys/` 에 생깁니다. 절대 경로를 주면 그대로 쓰이므로,
키만 별도 볼륨에 두는 식으로 다른 자리에 둘 수 있습니다. 기동 로그에 실제로 연 파일의 절대
경로가 찍히니, 어느 키를 읽었는지는 로그를 보면 됩니다.

## 비root로 실행하는 경우

프로세스는 root 가 아니어도 뜹니다. `not running as root` 경고를 남기고 올릴 수 있는 만큼만
올립니다.

- 파일 디스크립터 한도를 65535까지 올리려 하지만, root 가 아니면 소프트 리밋을 하드 리밋까지만
  올릴 수 있습니다. 하드 리밋이 그보다 낮으면 `max ulimit is low` 경고를 남기고 그대로
  진행합니다. 터널이 많으면 하드 리밋을 미리 올려 두어야 합니다.
- API 포트를 1024 미만으로 두면 비root 프로세스는 바인딩에 실패합니다. 1024 이상을 쓰거나
  실행 파일에 `CAP_NET_BIND_SERVICE` 를 주어야 합니다.
- 프로세스는 **데이터베이스 파일이 있는 디렉터리에 쓸 수 있어야** 합니다. 첫 기동 때 임시
  비밀번호 파일이 거기에 생기기 때문입니다. 쓸 수 없는 디렉터리면 기동이 멈춥니다. 아무도
  비밀번호를 읽을 수 없는 계정은 아무도 로그인할 수 없는 API 이기 때문입니다.

`service_ports.local_port` 가 1024 미만이면 Host 의 sshd 가 리스너를 열어 주지 않습니다. 이
리스너는 Tunnel Manager 가 아니라 Host 의 sshd 가 만들기 때문에, 이 제약은 Tunnel Manager 를
돌리는 계정이 아니라 Host 에 등록한 SSH 접속 계정에 걸립니다. ssh(1) 에 적힌 대로 특권
포트는 원격 계정이 root 일 때만 포워딩됩니다. Host 의 SSH 계정이 root 가 아니면
`local_port` 는 1024 이상으로 잡아야 합니다.

### 소유권과 서비스 계정

따로 마련할 두 번째 디렉터리가 없습니다. 키와 로그의 기본값이 데이터베이스 파일이 있는
디렉터리 아래의 `keys/` 와 `logs/` 라서, 그 디렉터리에 쓸 수 있는 계정이면 필요한 것이 전부
갖춰집니다. 로그 파일을 못 만들어도 기동을 멈추지는 않습니다. 파일 로깅만 끄고 콘솔에 전부
남기며, 이유는 `logging to file is disabled` 경고에 적힙니다.

systemd 로 돌릴 때는 `_scripts/systemd/tunnel-manager.service` 가 `User=root` 이므로 계정만
바꾸고 `StateDirectory=` 는 그대로 두면 됩니다.

```ini
[Service]
User=tunnel-manager
Group=tunnel-manager
```

`StateDirectory=tunnel-manager` 가 `/var/lib/tunnel-manager` 를 `User=`/`Group=` 소유로
만들어 주고, 이미 root 소유로 있던 디렉터리도 소유자가 바뀝니다. 이미 만들어져 있는 키
파일도 그 계정이 읽을 수 있어야 하니 소유자를 바꾸고 권한은 `0600` 으로 둡니다.

컨테이너는 `Dockerfile` 이 `USER root` 로 끝나서 root 로 돕니다. 비root 로 돌리려면
docker-compose.yaml 의 서비스에 `user: "<uid>:<gid>"` 를 주고, 호스트의 `./_data` 를 그
uid 소유로 만들어 두어야 합니다. root 로 한 번 띄운 뒤라면 그 디렉터리가 root 소유로 남아
있으니 소유자부터 바꿔야 합니다. 파일 디스크립터 한도는 docker-compose.yaml 의 `ulimits`
가 정하므로 컨테이너 안의 계정과는 상관이 없습니다.

## 라이선스

MIT License
