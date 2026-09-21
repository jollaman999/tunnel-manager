# 보안 강화 16건

2026-09-22 · 승인됨

## 무엇을 하나

보안 감사에서 나온 17건 중 16건을 고친다. 의존성 취약점 30건은 `4ee1ab8` 에서 이미 닫았고,
여기서 다루는 것은 코드와 설정에 남은 것이다.

감사 방법은 네 축으로 나눈 코드 전수 판독 + `govulncheck` 이었다. 서버를 띄워 요청을 보낸
재현은 하지 않았으므로, 아래 항목은 전부 `file:line` 근거이지 실측 재현이 아니다.

## 1. SSH 호스트 키 검증

가장 큰 구멍이다. `internal/tunnel/manager.go:397` 이 `ssh.InsecureIgnoreHostKey()` 라서
SSH 서버의 신원을 전혀 확인하지 않는다. 비테스트 코드에서 `HostKeyCallback` 이 설정되는 곳은
여기 하나뿐이다.

경로상의 공격자가 SSH 서버를 사칭하면 `internal/tunnel/manager.go:255` 의 `ssh.Password` 로
평문 SSH 비밀번호가 그대로 넘어간다. 비밀번호를 AES-256-GCM 으로 봉인해 저장하는
(`internal/crypto/crypto.go:57`) 수고가 첫 연결에서 무의미해진다.

### 모델

`models.Host` 에 두 칸을 더한다.

| 칸 | 무엇 |
|----|------|
| `HostKey` | 신뢰하기로 한 공개키. `<알고리즘> <base64>` 한 줄 |
| `PendingHostKey` | 상대가 제시했으나 아직 승인되지 않은 키. 같은 형식 |

둘 다 공개키라 비밀이 아니지만, 화면과 API 응답에 싣는 것은 SHA256 지문이다. 키 원문을
그대로 내보내면 화면이 길어지기만 하고 비교에 도움이 안 된다.

컬럼 추가는 `AutoMigrate` 가 기존 행을 NULL 로 채운다. `Tunnel.ServerBanner`
(`internal/models/models.go:88`) 와 `Tunnel.ForwardReach` (`:99`) 가 같은 방식으로 붙은
선례다.

### 콜백

| 상태 | 동작 | 터널 상태 |
|------|------|-----------|
| `HostKey` 가 빔 | 연결을 끊고 제시된 키를 `PendingHostKey` 에 기록 | `host_key_unapproved` |
| 제시된 키 == `HostKey` | 통과 | 평소대로 |
| 제시된 키 != `HostKey` | 연결을 끊고 `PendingHostKey` 에 기록 | `host_key_mismatch` |

**첫 연결에도 승인을 요구한다.** TOFU 로 조용히 저장하는 쪽이 ssh 클라이언트 관례이고 손이
덜 가지만, Host 를 등록하는 그 순간에 공격자가 경로에 있으면 TOFU 는 공격자의 키를 신뢰로
굳힌다. 등록은 드물게 일어나는 일이고 그때 한 번 지문을 확인하는 비용이, 굳어 버린 신뢰를
나중에 알아채지 못하는 비용보다 싸다.

그 대가로 **기존 설치본의 Host 가 전부 한 번씩 멈춘다.** 업그레이드 직후 모든 터널이
`host_key_unapproved` 로 내려가고, 운영자가 Host 수만큼 승인해야 살아난다. 이것을 알고
고른 것이다.

### 승인

status 화면 (`internal/web/static/screens.js:315` 의 `drawStatus`, `:443` 의 `statusBadge`)
에 그 상태의 배지와 버튼이 뜬다. 버튼은 `internal/web/static/app.js:1406` 의 `openModal` 을
쓴다. 이 패널은 `#app` 이 아니라 `body` 에 붙으므로 status 의 주기 갱신에 지워지지 않는다.

| 경우 | 모달 | 재확인 |
|------|------|--------|
| 첫 연결 | 제시된 지문 하나 | 세션만 |
| 불일치 | 신뢰 중인 지문과 제시된 지문을 나란히 + 경고 | **계정 비밀번호** |

불일치에 비밀번호를 다시 묻는 것은 `internal/api/uninstall.go:172` 가 이미 쓰는 방식이다.
세션을 훔친 자가 중간자를 조용히 승인해 버리는 것을 막는다. 첫 연결에는 묻지 않는다 - Host
를 등록할 때마다 거치는 자리라 비밀번호까지 물으면 등록이 무거워지고, 그 자리에는 아직
뒤집을 신뢰가 없다.

승인하면 `HostKey` 를 갈아끼우고 `PendingHostKey` 를 비운다. `connectionFingerprint`
(`internal/tunnel/reconcile.go:64-76`) 의 입력에 `HostKey` 를 넣어 두므로 reconcile 이 바뀐
것을 보고 터널을 다시 세운다. 넣지 않으면 승인해도 다음 연결 끊김까지 반영되지 않는다.

### 문자열

새 UI 문자열은 13개 언어를 모두 채운다. 지금 749키 × 13개 파일이고, 영어만 채우고 폴백에
맡기는 길도 있지만 (`internal/web/web_test.go:594` 가 폴백을 보장한다) 기존 관례가 전부
채우는 쪽이다.

## 2~17. 나머지 15건

| # | 어디 | 무엇이 문제인가 | 어떻게 |
|---|------|-----------------|--------|
| 2 | `internal/settings/settings.go:185-245` | `logging_file_path`·`security_key_file` 에 경로 규칙이 없다. 인증된 관리자가 `PUT /api/settings` + `POST /api/restart` 로 root 권한의 임의 파일 추가쓰기(`main.go:193`, 0644 생성)와 `GET /api/logs` 임의 파일 tail 을 얻는다 | `Validate()` 가 데이터 디렉터리 하위 상대경로만 받는다. 이미 저장된 위반값은 기동 시 거부하고 기본값으로 떨어뜨리며 경고 |
| 3 | `go.mod:9` | echo v4.13.3 의 `%2F` 가 라우트 보호를 우회한다 (GO-2026-6293). 이 앱은 `/api/*` 그룹 미들웨어로 인증을 건다 (`main.go:1164`) | 도달 여부를 요청으로 확인해 기록하고, 결과와 무관하게 v4.15.3 으로 올린다 |
| 4 | `internal/database/database.go:285`, `main.go:188,193` | DB·로그 파일 0644, 디렉터리 0755. 키 파일만 0600 이다 (`internal/crypto/crypto.go:21`) | 파일 0600, 디렉터리 0700 으로 생성하고 기동 시 기존 파일도 좁힌다 |
| 5 | `main.go:1166` | 로그인에 시도 제한이 없다. 관련 코드 0건 | IP·계정 단위 실패 카운터. 메모리 상태로 두고 재시작 시 초기화 |
| 7 | `internal/tlsserve/cert.go:117,120` | 자체서명 인증서가 `IsCA: true` + `CertSign` 이고 이름 제약이 없다. 신뢰 저장소에 넣은 클라이언트에서, 키를 얻은 자가 임의 도메인 인증서를 발급할 수 있다 | `IsCA:false` 로 내리거나 `PermittedDNSDomains`/`PermittedIPRanges` 를 건다. 어느 쪽인지는 구현 시 정해 보고 |
| 8 | `main.go:1090-1097` | 본문 크기 제한과 서버 타임아웃이 없다. 걸린 곳은 평문 리다이렉트 서버뿐이다 (`internal/tlsserve/redirect.go:98`) | `BodyLimit` + `ReadHeaderTimeout`·`ReadTimeout`·`IdleTimeout` |
| 9 | `internal/install/fetch.go:162-179,512-526` | 릴리즈에 `SHA256SUMS` 자산이 없으면 무검증 설치가 된다 (`Verified:false`). 리다이렉트 스킴·호스트 검사가 없다 | 체크섬 자산이 없으면 설치를 중단하고 실행 중 바이너리로 폴백. `CheckRedirect` 로 https 아닌 스킴과 github 밖 호스트 거부 |
| 10 | `main.go:1090-1097` | 보안 헤더가 하나도 없다 | `X-Frame-Options: DENY`·`nosniff`·`default-src 'self'` 는 항상. HSTS 는 운영자가 정식 인증서를 등록했을 때만 - 자체서명에 HSTS 를 걸면 되돌릴 수 없다 |
| 11 | `internal/api/auth.go:292,309` | 쿠키 `Secure` 를 `c.IsTLS()` 로만 정한다. TLS 를 종단하는 프록시 뒤에서는 세션 쿠키가 `Secure` 없이 나간다 | 신뢰 프록시 설정이 켜졌을 때만 `X-Forwarded-Proto` 를 반영. 기본은 지금 그대로 |
| 12 | `internal/auth/auth.go:79-96` | 초기 비밀번호 파일을 `O_TRUNC` 로 열어 내용을 먼저 쓰고 나중에 `Chmod(0600)` 한다 | `O_EXCL` 로 만들거나 쓰기 전에 `Chmod` |
| 13 | `internal/api/auth.go:189` | 세션에 절대 수명이 없다. 조회마다 만료가 밀린다 | 생성 시각을 두고 절대 상한 |
| 14 | `internal/database/database.go:158,166` | `zap.String("sql", sql)` 의 `sql` 은 gorm 이 바인딩 값을 끼워 넣은 것이다. 계정 테이블 INSERT/UPDATE 가 실패하면 bcrypt 해시가 로그에 남고, 그 로그는 0644 이자 `GET /api/logs` 로 읽힌다. 실패 분기는 debug 가 아니라 기본 레벨에서도 찍힌다 | 계정 테이블 문장을 마스킹 |
| 15 | `Dockerfile:15,24,27` | `USER root`, `alpine:3.21.0`, 두 베이스가 digest 고정이 아니다 | 비루트 `USER`, alpine 갱신, digest 고정 |
| 16 | `.github/workflows/build.yaml:16` | `permissions:` 블록이 없어 `GITHUB_TOKEN` 이 리포 기본 권한으로 돈다 | `permissions: contents: read` |
| 17 | `internal/install/files.go:88-95` | `-purge` 가 저장된 `security.key_file` 이 아니라 기본값으로 키 경로를 계산해, 옮겨 둔 키를 남긴다 | 주석이 이유(돌고 있는 서비스의 DB 를 열어야 한다)를 적어 둔 트레이드오프다. 지우지 못했을 수 있다고 보고에 적는다 |

## 하지 않는 것

**6번 - 자격증명 변경 시 호출자 세션 토큰 회전.** 감사에서 나왔으나 뺀다.
`internal/api/account.go:232-245` 가 그 세션을 남기는 이유를 명시적으로 적어 두었다. 세션
탈취 시나리오를 고려하면 뒤집을 근거가 있지만, 명시적으로 반대로 정해 둔 결정이라 이번
범위에서 제외한다.

**`service_ip` 가 루프백·링크로컬을 가리킬 수 있는 것.** 설계상 기능이다. 임의 주소로
포워딩하는 것이 이 프로그램이 하는 일이고, 막으면 정당한 용도가 깨진다. 위험면으로만 남긴다.

**echo 외 나머지 의존성.** gorm, validator, sqlite 에 호출되는 취약점이 없다.

**전송 파일의 재생 방지.** `internal/api/transfer.go:59-65` 의 `ExportedAt` 이 검사되지
않아 옛 익스포트를 다시 임포트할 수 있으나, 비밀번호와 세션이 둘 다 필요해 우선순위가 낮다.

**릴리즈 재빌드와 태그.** 구현이 끝난 뒤 별도 절차로 간다.

## 구현이 설계와 갈린 곳

만들면서 알게 된 것 때문에 설계와 다르게 간 것이 넷이다. 무엇을 왜 바꿨는지 여기 남긴다.

| 항목 | 설계 | 구현 | 왜 |
|------|------|------|-----|
| 5 로그인 시도 제한 | 누적 지연 | **거절** | 지연은 핸들러 안에서 요청을 붙잡을 뿐이라, 동시에 보낸 50개가 나란히 지연되고 전부 bcrypt 에 닿는다. 추측 속도도 CPU 도 안 묶인다. 비밀번호를 보기 전에 쓰는 거절이 둘 다 묶는다 |
| 8 본문 크기 제한 | `1M` 하나 | **2단: 일반 1M, 임포트 32M** | 설정 익스포트가 Host 마다 PEM 개인키·패스프레이즈·SSH 비밀번호를 담고 전체를 base64 로 싼다. Host 가 수백 개면 1M 을 넘어 임포트가 깨진다 |
| 14 계정 테이블 | 테이블명 `users` | **`user`** | `internal/models/models.go:184` 의 `TableName()` 이 단수로 고정한다. gorm 이 복수로 만드는 것을 그 메서드가 막고 있다 |
| 15 Dockerfile | 비루트 `USER` 포함 | **digest 고정과 alpine 갱신만** | `docker-compose.yaml:25` 의 `./_data:/data` 바인드 마운트가 호스트 디렉터리 소유권을 그대로 쓰고, docker 가 없는 디렉터리를 root 로 만든다. 비루트로는 DB 생성이 실패하는 것을 실측했다. 배포 쪽(`user:` 지정 + 호스트 디렉터리 chown)이 같이 가야 한다 |

## 작업 순서

호스트 키 넷을 먼저 하고, 나머지는 서로 얽히지 않으므로 값싼 것부터 간다. 작업마다
`go build` · `go vet` · `gofmt` · `go test -race` 를 돌리고 커밋한다.

| T | 작업 |
|---|------|
| 1 | 호스트 키: 모델·마이그레이션·SSH 콜백·reconcile 지문 |
| 2 | 호스트 키: 승인 API + 비밀번호 재확인 |
| 3 | 호스트 키: status 배지·모달 + `en.json` |
| 4 | 호스트 키: 나머지 12개 언어 |
| 5 | 2번 경로 검증 |
| 6 | 4번 파일 권한 |
| 7 | 8번 본문 제한·타임아웃 |
| 8 | 10번 보안 헤더 |
| 9 | 11번 프록시 뒤 쿠키 |
| 10 | 13번 세션 절대 수명 |
| 11 | 3번 echo |
| 12 | 16번 CI 권한 |
| 13 | 15번 Dockerfile |
| 14 | 5번 로그인 시도 제한 |
| 15 | 9번 릴리즈 체크섬·리다이렉트 |
| 16 | 12번 초기 비밀번호 파일 |
| 17 | 14번 SQL 로그 마스킹 |
| 18 | 17번 `-purge` 의 키 경로 보고 |
