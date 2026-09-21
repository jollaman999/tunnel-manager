# 설치·제거를 바이너리 안으로

2026-09-21 · 승인됨

## 무엇을 만드나

`tunnel-manager -install` 과 `tunnel-manager -uninstall`. 리눅스·맥·윈도우에서 같은 명령으로
서비스를 등록하고 내린다. 별도 스크립트 파일은 만들지 않는다.

## 왜 스크립트가 아니라 플래그인가

`./install` 은 윈도우 cmd·PowerShell 에서 셸 스크립트를 실행하지 못한다. 스크립트로 가면
`install` 과 `install.ps1` 두 파일이 되고, 사용자는 릴리즈에서 바이너리와 스크립트를 따로
받아야 한다. 바이너리에 넣으면 받는 파일이 하나로 유지되고, 세 OS 의 서비스 관리자를
Go 코드 한 자리에서 다룰 수 있다.

## 플래그

```
tunnel-manager -install                                   전부 기본값
tunnel-manager -install -bin /opt/tm/tunnel-manager -db /opt/tm/tm.db
tunnel-manager -uninstall                                 등록된 것을 읽어서 제거
tunnel-manager -uninstall -purge                          데이터까지
```

`-install` 에 경로를 직접 붙이지 않는 이유는 Go 표준 `flag` 의 제약이다. 문자열 플래그는 값이
필수라 `-install` 만 쓰면 오류가 나고, `-install -db x` 는 `install="-db"` 가 된다. 그래서
`-install` 은 불리언이고 경로는 `-bin` 과 이미 있는 `-db` 가 받는다.

## OS 별 자리

| | 리눅스 | 맥 | 윈도우 |
|---|--------|-----|--------|
| 실행 파일 | `/usr/local/bin/tunnel-manager` | 같음 | `C:\Program Files\tunnel-manager\tunnel-manager.exe` |
| 데이터 | `/var/lib/tunnel-manager` | `/Library/Application Support/tunnel-manager` | `C:\ProgramData\tunnel-manager` |
| 서비스 | systemd 유닛 | LaunchDaemon plist | SCM 서비스 |
| 계정 | root | root | LocalSystem |
| 자동 재시작 | `Restart=always` `RestartSec=5` | `KeepAlive` | `sc failure restart/5000` |

리눅스 유닛은 `_scripts/systemd/tunnel-manager.service` 와 같은 내용으로 쓴다. 그 파일이 지금
운영 중인 유닛과 한 글자도 다르지 않다는 것을 확인했다.

## 설치할 바이너리를 어디서 가져오나

1. 기본은 GitHub 최신 릴리즈에서 이 OS/arch 자산을 받는다.
2. 릴리즈에 `SHA256SUMS` 가 있으면 검증하고, 없으면 HTTPS 만 믿는다. 앞으로 릴리즈에
   `SHA256SUMS` 를 같이 올린다.
3. 내려받기가 실패하면 **지금 실행 중인 파일**을 쓴다. 어느 쪽을 썼는지 반드시 찍는다.

## 제거가 경로를 아는 법

별도 상태 파일을 두지 않는다. 등록된 서비스가 실행 파일 경로와 `-db` 경로를 둘 다 갖고 있다.

| OS | 무엇을 읽나 |
|----|-------------|
| 리눅스 | `systemctl show -p FragmentPath -p ExecStart` |
| 맥 | plist 의 `ProgramArguments` |
| 윈도우 | `svc/mgr` 의 `Config().BinaryPathName` |

리눅스에서 실제로 확인한 출력:

```
$ systemctl show tunnel-manager -p FragmentPath --value
/usr/lib/systemd/system/tunnel-manager.service
$ systemctl show tunnel-manager -p ExecStart --value
... argv[]=/usr/local/bin/tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db ...
```

등록된 서비스가 없으면 아무것도 지우지 않고 그렇게 말하고 끝낸다. 그때는 `-bin`·`-db` 로
직접 지목할 수 있다.

## 이미 설치돼 있을 때

| 상황 | 행동 |
|------|------|
| 등록된 경로와 같음 | 덮어쓰고 재시작. 전후 md5 를 찍는다 |
| 다름 | 멈추고 "먼저 `-uninstall` 하라"고 말한다. 옛 실행 파일과 옛 데이터가 고아로 남는 것을 막는다 |
| 없음 | 새로 등록 |

유닛 파일 자리도 같다. 먼저 `FragmentPath` 를 물어보고 있으면 그 자리를 덮어쓰고, 없을 때만
`/etc/systemd/system/` 에 새로 쓴다. 이 기계의 유닛은 `/usr/lib/systemd/system/` 에 있어서,
그냥 `/etc` 에 쓰면 유닛이 둘이 되고 `/etc` 쪽이 이겨 옛 파일이 남는다.

## 윈도우

지금 바이너리는 Windows 서비스 제어 프로토콜을 말하지 않는다 (`windows/svc` 사용처 0건).
`sc create` 로 등록해도 시작에 실패한다. `golang.org/x/sys/windows/svc` 를 쓴다. 이미
`go.mod` 에 `v0.28.0` 이 indirect 로 있어 새 모듈은 아니고 direct 로 올라갈 뿐이다.

`svc.IsWindowsService()` 가 참이면 `svc.Run` 으로 들어가고, 아니면 지금 `main()` 대로 간다.
이것이 되면 화면의 "서비스 재기동" 도 윈도우에서 제대로 돌아온다. 지금은
`reexec_windows.go:16` 이 `false` 를 내서 프로세스가 멈춘 채 끝나고, 되살리는 것은 감독하는
쪽에 맡겨져 있다.

실행 중인 `.exe` 는 지울 수 없다. `-uninstall` 이 자기 자신을 지우는 경우
`MoveFileEx` 의 재부팅 시 삭제로 넘기고 그렇게 찍는다.

## 안 하는 것

- Docker Compose 경로
- 비root 계정으로 도는 서비스 구성 (문서의 "Running as a non-root user" 대로 손으로)
- 업그레이드 전용 명령 (`-install` 재실행이 덮어쓴다)
- deb·rpm·brew·msi 패키지
- 화면의 `POST /api/uninstall` 동작 변경 (데이터만 지우는 것이 그쪽 일이다)

## 되돌리기 어려운 것

기존 실행 파일 덮어쓰기와 서비스 재기동. 설치 전후 md5 를 찍고, 이미 서비스가 있으면 멈췄다
뜬다는 것을 먼저 알린다. 권한이 없으면 아무것도 하지 않고 끝낸다.

## 검증에서 못 하는 것

리눅스는 이 기계에서 왕복 시험이 된다 (운영 서비스가 아니라 별도 이름·경로로). 맥과 윈도우는
실행 환경이 없다. 만들어지는 plist XML 과 SCM 설정을 Go 단위 테스트로 고정하고 문서에 싣되,
실제 기동 확인은 그 OS 에서 해야 한다.

## 작업 목록

| # | 작업 | 완료 조건 | 의존 |
|---|------|-----------|------|
| 1 | `internal/install` 골격: 경로 기본값, 권한 확인, 공통 흐름, 보고 출력 | `go test ./internal/install/` 통과, OS 별 기본 경로가 표대로 | - |
| 2 | 리눅스 백엔드 (systemd) | 이 기계에서 별도 이름으로 등록·조회·해제 왕복 | 1 |
| 3 | 맥 백엔드 (launchd) | 생성되는 plist XML 이 테스트로 고정됨 | 1 |
| 4 | 윈도우 백엔드 (SCM) + `svc.IsWindowsService()` 분기 | `GOOS=windows go build` 통과, SCM 설정이 테스트로 고정됨 | 1 |
| 5 | 릴리즈 내려받기 + `SHA256SUMS` 검증 + 실행 파일 폴백 | 네트워크 없을 때 폴백이 도는 것을 테스트로 | 1 |
| 6 | `main.go` 플래그 배선과 usage | `-install`·`-uninstall`·`-bin` 이 `-help` 에 나옴 | 1 |
| 7 | 제거 흐름: 등록에서 경로 읽기, `-purge`, 윈도우 자기 삭제 | 2·3·4 의 읽기 경로가 같은 구조체를 냄 | 2,3,4 |
| 8 | README 설치·삭제 절 (영·한·일·중) + `docs/reference.md` 갱신 | 4개 README 구조가 같고 앵커 안 깨짐 | 6,7 |
| 9 | 릴리즈에 `SHA256SUMS` 올리기 (Makefile·릴리즈 절차) | `make release` 가 파일을 만듦 | - |
