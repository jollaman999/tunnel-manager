# Tunnel Manager

SSH 터널을 관리하기 위한 RESTful API 서버입니다. 여러 VM에 대한 SSH 터널을 생성하고 관리할 수 있습니다.

## 주요 기능

- VM 및 서비스 포트 관리
- SSH 터널 자동 생성 및 관리
- 터널 상태 모니터링
- 장애 발생 시 자동 재연결
- RESTful API 인터페이스

## 시스템 요구사항

- Go 1.23 이상
- MySQL 5.7 이상 또는 MariaDB 10.3 이상
- Docker & Docker Compose (선택사항)

## 동작 방식

```mermaid
sequenceDiagram
    participant VM as VM (Local)
    participant Bastion as Tunnel Manager
    participant WAS as Remote Server
    
    rect rgb(255, 255, 220)
        Note over VM,WAS: Initial Setup Phase
        Bastion->>VM: SSH Authentication (Password)
    end

    rect rgb(255, 255, 220)
        Note over VM,WAS: Tunnel Creation Phase
        Bastion->>VM: Create SSH Tunnel
        Note right of Bastion: For each service port:-R 127.0.0.1:localPort:remoteIP:remotePort
    end
    
    rect rgb(255, 255, 220)
        Note over VM,WAS: Service Access Phase
        VM->>VM: Connect to 127.0.0.1:localPort
        VM->>Bastion: Forward Traffic through tunnel
        Bastion->>WAS: Forward to remoteIP:remotePort
        WAS-->>Bastion: Response
        Bastion-->>VM: Response through tunnel
    end

    Note over VM,WAS: Monitoring & Auto-reconnect
    loop Every monitoring_interval_sec
        Bastion->>VM: keepalive@tunnel check
        alt Connection Lost
            Bastion->>VM: Reconnect SSH Tunnel
        end
    end
```

## 설치 및 실행

### Docker Compose를 사용하는 경우

1. 저장소 클론
```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
```

2. 설정 파일 수정
```bash
# config.yaml 파일을 환경에 맞게 수정
vi config/config.yaml
```

3. Docker Compose로 실행
```bash
docker-compose up -d
```

### 직접 실행하는 경우

1. 저장소 클론
```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
```

2. 의존성 다운로드
```bash
go mod tidy
```

3. 빌드
```bash
make
```

4. 설정 파일 수정
```bash
cp config/config.yaml config.yaml

# config.yaml 파일을 환경에 맞게 수정
vi config.yaml
```

5. 실행
```bash
./tunnel-manager
```

## API 엔드포인트

### VM 관리
- `POST /api/vms` - VM 생성
- `GET /api/vms` - VM 목록 조회
- `GET /api/vms/:id` - 특정 VM 조회
- `PUT /api/vms/:id` - VM 정보 수정
- `DELETE /api/vms/:id` - VM 삭제

### 서비스 포트 관리
- `POST /api/service-ports` - 서비스 포트 생성
- `GET /api/service-ports` - 서비스 포트 목록 조회
- `GET /api/service-ports/:id` - 특정 서비스 포트 조회
- `PUT /api/service-ports/:id` - 서비스 포트 정보 수정
- `DELETE /api/service-ports/:id` - 서비스 포트 삭제

### 상태 모니터링
- `GET /api/status` - 전체 터널 상태 조회
- `GET /api/status/:vmId` - 특정 VM의 터널 상태 조회

## 설정 파일 구조

config.yaml:
```yaml
database:
  host: tunnel-manager-db
  port: 3306
  user: tunnel-manager
  password: tunnel-manager-pass
  name: tunnel-manager
  timeout_sec: 30

api:
  port: 8080

monitoring:
  interval_sec: 60

logging:
  level: info
  format: json
```

## 라이선스

MIT License
