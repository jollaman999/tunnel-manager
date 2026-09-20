# Tunnel Manager 参考手册

[回到 README](README.zh.md)

这里是全部内容：安装、设置、API，以及各个页面都做什么。[README](README.zh.md) 是它的简版。

Tunnel Manager 打开 SSH 隧道，并让它们一直开着。你注册它要登录的 SSH 服务器（**Host**）和要
发布的服务（**服务端口**），说明哪台 Host 承载哪个服务端口，它就为每条分配关系建一条隧道，
从此一直盯着。REST API、浏览器界面和隧道本身，全都由这一个可执行文件提供。

这里建的是**反向**隧道，也就是说监听套接字开在 Host 上，而不是开在 Tunnel Manager 所在的机器
上。客户端连上 **Host 的** `local_port`，流量顺着 SSH 连接送到 Tunnel Manager，再由它连上
`service_ip:service_port`，两个方向来回搬字节。只有 Tunnel Manager 能访问到的服务，就是这样
变成从 Host 也能访问的。

| 你注册的东西 | 字段 | 是什么 |
|--------------|------|--------|
| Host | `ip`、`port`、`user`、`private_key`、`key_passphrase`、`password`、`description`、`enabled` | Tunnel Manager 要登录的 SSH 服务器。可以用私钥登录，可以用密码登录，也可以两个都注册，但至少要有一个。密钥、密钥的密码和登录密码都加密保存。 |
| 服务端口 | `service_ip`、`service_port`、`local_port` | 要发布的服务，以及在承载它的每台 Host 上打开的端口。 |
| 分配关系 | `host_id`、`sp_id` | 一台 Host 配一个服务端口，表示这台 Host 要承载它。隧道就是从它建起来的，你注册 Host 或服务端口时它会自动生成。 |

`ip` 和 `service_ip` 都收 IPv4 或 IPv6 地址。IPv6 地址照原样写成 `2001:db8::1`，建立连接时需要
的方括号，由用到这个地址的地方自己补上。带 zone 的写法，像 `fe80::1%eth0`，会被拒绝。

**一条分配关系，只要它的 Host 是启用的，就是一条隧道。** 承载三个服务端口的 Host 跑三条隧道，
一个都不承载的 Host 一条也不跑，存了多少个服务端口都一样。注册一台 Host 时，现有的服务端口
全部分配给它；注册一个服务端口时，现有的 Host 全部分到它，除非请求里另有交代。所以从不碰分配
关系的安装，跑的还是原来那样的全组合。之后要改一台 Host 承载什么，用 Hosts 页面上那一行里的
**Service ports** 按钮，或者走[一台 Host 承载的服务端口](#一台-host-承载的服务端口)。

**登录一台 Host 可以用密钥、用密码，也可以两个都用。** `private_key` 要发 PEM 私钥文件的文本
内容，密钥带密码保护时把 `key_passphrase` 跟它一起发。密钥在注册时就读一遍，所以根本不是密钥
的文件、要密码却没给密码的密钥、打不开密钥的密码，都在那一刻就被拒绝，而不是拖到下一次连接。
两个都注册了的话，先拿密钥去连，密码是退路，所以给一台隧道正跑着的 Host 加密钥不会把隧道弄断。
隧道开着时注册的密钥，从下一次建连接起用上。

密钥、密钥的密码和登录密码都不会再离开这个进程：哪个应答里都没有它们，编辑一台 Host 时这几个
框是空的。框留空表示已存的值不动，而发上来的密钥会把已存的密钥连同它的密码一起替换掉。

**除了这个可执行文件，没有别的要装。** 数据库是进程自己创建的 SQLite 文件，设置存在这个文件
里、在浏览器的页面上改，界面编译进了可执行文件。不用数据库服务器，没有配置文件，也没有哪个
目录非得跟着它一起搬。

## 目录

- [运行条件](#运行条件)
- [工作原理](#工作原理)
- [安装与运行](#安装与运行)
- [HTTPS 与证书](#https-与证书)
- [首次启动与账号](#首次启动与账号)
- [内置界面](#内置界面)
- [设置](#设置)
- [卸载](#卸载)
- [在脚本里调用 API](#在脚本里调用-api)
- [API 接口](#api-接口)
- [读懂隧道状态](#读懂隧道状态)
- [加密密钥](#加密密钥)
- [以非 root 用户运行](#以非-root-用户运行)
- [许可证](#许可证)

## 运行条件

跑发布出来的可执行文件：什么都不用准备。SQLite 引擎、界面和它需要的一切都带在里面。

| 要做的事 | 需要什么 |
|----------|----------|
| 跑发布的可执行文件 | 别的都不用 |
| 从源码构建 | Go 1.23 或更新的版本 |
| 跑容器 | Docker 和 Docker Compose |

SQLite 驱动是纯 Go 写的（`github.com/glebarez/sqlite` 架在 `modernc.org/sqlite` 上），所以
可执行文件用 `CGO_ENABLED=0` 构建，落到哪台机器上都不需要 C 库。

### 支持的平台

`make release` 会给下面每一项各构建一个可执行文件。Go 支持的其他平台用 `go build` 也能构建，
只是要留意下面这段。

| 平台 | 发布的可执行文件 |
|------|------------------|
| Linux amd64 | `tunnel-manager-linux-amd64` |
| Linux arm64 | `tunnel-manager-linux-arm64` |
| macOS Intel | `tunnel-manager-darwin-amd64` |
| macOS Apple silicon | `tunnel-manager-darwin-arm64` |
| Windows amd64 | `tunnel-manager-windows-amd64.exe` |

在 Unix 上，进程启动时会自己抬高可打开文件描述符的上限，因为每条隧道都要占掉好几个。Windows
没有这种按进程算的上限可抬，那一步在那里什么也不做。除此之外没有别的差别。

随附的 systemd unit 是给 Linux 用的。在别的平台上，得靠那个系统自己的办法让进程一直跑着。

实际跑过的只有 Linux 的可执行文件。其余几个只是用对应平台的编译器和 vet 工具构建并检查过，
仅此而已。

## 工作原理

### 调谐循环

Tunnel Manager 不停地把该有的样子和眼下的样子放在一起比对。

| 状态 | 是什么 | 从哪里来 |
|------|--------|----------|
| 期望 | 每台启用的 Host 上，分配给它的每一个服务端口 | `hosts`、`service_ports` 和 `host_service_ports` 里的行 |
| 实际 | 此刻正跑着的隧道 | 进程内部的管理器，以及它写下的 `tunnels` 行 |

一次**调谐**把两边比一遍，再把差距抹平：期望有而没跑的就启动，跑着但不再期望的就停掉，连接
参数（服务器地址、远端地址、本地端口、用户、密码）和表里对不上的隧道，停掉之后用新参数再建一次。

现在一个 API 请求是这样走的：

```text
POST /api/service-port
  事务 { INSERT INTO service_ports } 提交
  唤醒调谐循环
  201 Created          <- 应答不等任何一条隧道

调谐循环
  期望 = Host 处于启用状态的那些分配关系
  实际 = 正跑着的隧道
  期望有而没跑着    -> 启动
  跑着而不再期望    -> 停掉
  跑着但设置是旧的  -> 停掉再重建
```

调谐会在三个时刻各跑一遍：

| 什么时候 | 为什么 |
|----------|--------|
| 启动时，在 API 应答任何请求之前 | 等到第一个请求能来问的时候，表里那些行的隧道已经起来了 |
| 一次 `POST`、`PUT` 或 `DELETE` 提交之后立刻 | 改动立刻生效，不用等到下一个周期 |
| 每个调谐间隔，默认 5 秒 | 上一次没做完的事再试一遍 |

因为应答是在隧道存在之前就发出去的，**写请求成功并不等于隧道起来了。** 回答这件事的是
`GET /api/status`。见[读懂隧道状态](#读懂隧道状态)。

### 分配关系

**一条隧道只代表一条分配关系，不代表别的。** `host_service_ports` 表里，一台 Host 配一个服务
端口就是一行，这一对就是这行的全部内容，所以数据库本身就不允许同一条分配关系存两次。

| 发生了什么 | 分配关系会怎样 |
|------------|----------------|
| 注册一台 Host | 那一刻存着的服务端口全部分给它，除非请求把 `assign_all_service_ports` 发成 false |
| 注册一个服务端口 | 那一刻存着的每台 Host 都分到它，除非请求把 `assign_to_all_hosts` 发成 false |
| 停用一台 Host | 分配关系原样留着。它的隧道会停下来，重新启用就都回来 |
| 删掉一台 Host 或一个服务端口 | 提到它的分配关系在同一个事务里一起删掉 |
| 加了这张表的那次升级之后的第一次启动 | 每台 Host 分到每一个服务端口 |

最后一行是为升级准备的。这张表存在之前，调谐循环假定的就是每台 Host 承载每一个服务端口，这件
事从来没有被记下来过；而一次把新表留空的启动，读出来就是“没有 Host 承载任何东西”，会把所有隧道
都拆掉。所以只在创建这张表的那次启动时填一遍，此后再不填：之后的启动会把已经被去掉的分配关系
又加回来，而能去掉它们正是这张表存在的意义。

指向不存在的 Host 或服务端口的分配关系建不出任何东西，调谐会直接跳过它。从它身上什么也建不
起来：地址、端口和凭据都在那些已经没了的行上。

### 一条隧道的全过程

```mermaid
sequenceDiagram
    participant Host as Host（SSH 服务器）
    participant Bastion as Tunnel Manager
    participant WAS as 服务

    rect rgb(255, 255, 220)
        Note over Host,WAS: 准备阶段
        Bastion->>Host: SSH 认证（密码）
    end

    rect rgb(255, 255, 220)
        Note over Host,WAS: 建立隧道阶段
        Bastion->>Host: 创建 SSH 隧道
        Note right of Bastion: 每个已分配的服务端口各一条：-R 0.0.0.0:localPort:remoteIP:remotePort
    end

    rect rgb(255, 255, 220)
        Note over Host,WAS: 访问服务阶段
        Host->>Host: 连接 localPort（监听绑在 0.0.0.0）
        Host->>Bastion: 流量顺着隧道转发过来
        Bastion->>WAS: 转发到 remoteIP:remotePort
        WAS-->>Bastion: 应答
        Bastion-->>Host: 应答顺着隧道回去
    end

    Note over Host,WAS: 监控与自动重连
    loop 每个监控间隔
        Bastion->>Host: keepalive@tunnel 检查
        alt 连接断了
            Bastion->>Host: 重建 SSH 隧道
        end
    end
```

Host 上的监听到底会不会开在 `0.0.0.0` 上，由 Host 上的 SSH 服务器说了算。它的 `GatewayPorts`
关着的时候，不管请求的是哪个地址，监听都绑在回环地址上，日志里会写出来。隧道起来之后，
tunnel-manager 会自己去试那个转发端口，把试出来的结果报出来，见
[转发端口是否可达](#转发端口是否可达)。

监控间隔和调谐间隔是两件不同的事。监控是问一条已经起来的隧道还活着没有，不活了就重连。调谐
循环问的是该有的那一组隧道到底在不在。

## 安装与运行

**一套安装就是一个文件。** 到发布页面下载对应平台的可执行文件，加上执行权限再启动。数据库
文件、账号和它需要的其他东西，都在第一次启动时创建。

```bash
chmod +x tunnel-manager-linux-amd64
./tunnel-manager-linux-amd64
```

命令行参数就这几个：

| 参数 | 做什么 |
|------|--------|
| `-db <path>` | 数据库文件。设置、注册的 Host 和账号都在里面，文件不存在就创建，上层目录也一并创建 |
| `-reset-settings` | 把每一项设置都还原成默认值，打印改了什么然后退出。注册的 Host、服务端口、账号和证书都原样留着。见[服务起不来的时候](#服务起不来的时候) |
| `-version` | 打印版本号后退出 |
| `-help` | 打印参数后退出 |

### 文件放在哪里

**一个目录装下整套安装。** `-db` 指定数据库文件，这套安装的其他东西都待在这个文件所在的目录里。

```text
<数据库文件所在的目录>/
    tunnel-manager.db          设置、Host、服务端口、账号
    tunnel-manager.db-wal      SQLite 在旁边留的预写日志
    tunnel-manager.db-shm      SQLite 在旁边留的共享内存文件
    initial-password           第一次启动时写出，初始化完成后删除
    keys/tunnel-manager.key    用来加密 SSH 密码的密钥
    logs/tunnel-manager.log    日志文件，轮转出来的文件也在旁边
```

密钥文件和日志文件是**设置**，不是命令行参数：它们在 Settings 页面上，默认值是
`keys/tunnel-manager.key` 和 `logs/tunnel-manager.log`。**设置里写相对路径，是相对数据库
文件所在的目录来解析的，不是相对当前工作目录。** 工作目录每次都不一样，相对它解析的默认值会
让密钥在每台主机上落在不同的地方。给设置写绝对路径，那个路径就算数，这也正是把密钥或日志特意
放到安装目录之外的办法。

**进程启动时所在的那个目录里，什么都不会创建。**

只有日志是写成文件的，没有像别的东西那样写成数据库里的一行，这有三个原因。日志器必须在数据库
打开之前就先就位，因为打开数据库是最容易出岔子的一步，总得有人能说清它为什么失败。连接池里只有一条连接，
每写一行日志都得排在进程真正要跑的查询后面。而且数据库自己的语句也是通过这个日志器报出来的，
那样写一行日志就会变成一次写一行日志的查询。

不写 `-db` 的话，它由所在平台存放用户数据的位置推算出来。

| 怎么启动的 | `-db` | 这套安装待在哪个目录 |
|------------|-------|----------------------|
| 不带参数，Windows | 没给 | `%AppData%\tunnel-manager\` |
| 不带参数，macOS | 没给 | `~/Library/Application Support/tunnel-manager/` |
| 不带参数，Linux | 没给 | `$XDG_CONFIG_HOME/tunnel-manager/`，这个变量没设时是 `~/.config/tunnel-manager/` |
| 随附的 systemd unit | `/var/lib/tunnel-manager/tunnel-manager.db` | `/var/lib/tunnel-manager/` |
| Docker Compose | `/data/tunnel-manager.db` | `/data/`，compose 文件把它绑到宿主机的 `./_data` |

`$XDG_CONFIG_HOME` 和 `$HOME` 都没有的机器上就没有这样的位置，启动时会直说，而不是自己编一个：

```text
Failed to work out where the database file goes: no default location for the database file
is available: neither $XDG_CONFIG_HOME nor $HOME are defined. Give -db an absolute path
```

启动时会把打开的数据库文件、密钥文件和日志文件的绝对路径都记进日志，所以日志里始终写着用的
是哪几个文件。

### 用 Docker Compose 运行

```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
docker-compose up -d
```

镜像用 `-db /data/tunnel-manager.db` 启动这个可执行文件，compose 文件把 `/data` 绑到宿主机
的 `./_data`。容器被换掉时，数据库、密钥和日志就是靠它留下来的。

初始密码文件就写在那个目录里，所以从宿主机也读得到：

```bash
docker compose exec tunnel-manager cat /data/initial-password
```

### 作为 systemd 服务运行

`_scripts/systemd/tunnel-manager.service` 用绝对路径的 `-db` 启动这个可执行文件：

```ini
ExecStart=/usr/local/bin/tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db
StateDirectory=tunnel-manager
```

`StateDirectory=tunnel-manager` 会建出 `/var/lib/tunnel-manager` 并交给 `User=` 里的账号，
数据库、密钥、日志和初始密码全都待在里面。unit 里带的是 `User=root`，要改的话见
[以非 root 用户运行](#以非-root-用户运行)。

### 从源码构建

```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
make
./tunnel-manager
```

`make` 用 `CGO_ENABLED=0` 构建可执行文件。`make release` 给上面表里的每个平台各构建一个。

## HTTPS 与证书

**API 和界面都走 HTTPS，用的是本安装给自己做的证书。** 在浏览器里打开
`https://<地址>:<端口>/`。证书在第一次启动时生成，存进数据库文件，所以事前什么都不用准备，
也没有文件要往哪里放。

**端口还是那一个。** 明文打到这个端口上的请求，会收到一个 `307`，重定向到同一地址的 `https`
版本，所以还写着 `http://` 的书签或脚本照样落到它本来要去的地方。`307` 保留方法和请求体，
`301` 和 `302` 则会把它们变成 `GET`。明文请求的请求体从来不会被读：它在链路上是怎么写的就是
怎么躺着，客户端会用 TLS 重发一遍。防火墙上也不用为此多开什么。

| 自己生成的证书 | |
|----------------|--|
| 什么时候生成 | 第一次启动时；已存的那张读不出来或者过期了，再生成一次 |
| 密钥 | P-256 曲线上的 ECDSA |
| 有效期 | 5 年 |
| 签给谁 | `localhost`、`127.0.0.1`、`::1`、本机主机名，以及各网卡上的地址 |
| 存在哪 | 数据库文件里，挨着设置。私钥用封住 SSH 密码的那把密钥加密，所以光有一份数据库文件也带不走它 |
| 谁签的 | 它自己 |

启动时会把指纹写进日志，连同证书覆盖的名字和到期的日子。

### 浏览器的警告

**没有人给这张证书背书，所以浏览器会警告，脚本会拒绝。** 没有机构签发的证书就长这样，也没有
哪个设置能让这个警告自己消失。

核对这个警告值得用的是指纹。它在启动日志里，登录之后也在 Settings 页面上，写法和
`openssl x509 -fingerprint -sha256` 一样：32 个字节，大写十六进制，用冒号隔开。拿浏览器证书
查看器里显示的东西跟它对一下。指纹一样，说明连的就是这台服务器；不一样，说明有别的东西在冒充
它应答。

```bash
openssl s_client -connect 127.0.0.1:8888 </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

不想每次点过警告而是想彻底摆脱它，就把证书装进你用来浏览的那台机器的信任库。
Settings 页面上按 PEM 显示它就是为了这个，那和服务器在握手时交给每个客户端的是同一串字节。另一条路是注册你自己的证书。

`curl` 会拒绝这张证书，退出码 `60`。用 `--cacert <文件>` 把证书指给它，或者用 `-k` 跳过检查。
[在脚本里调用 API](#在脚本里调用-api) 里的例子改成设一次 `CURL_CA_BUNDLE`，那个 shell 里的
每个 `curl` 都会读它。

### 注册自己的证书

**Settings 页面上有两个框，一个放证书，一个放它的私钥，都是 PEM。** 把签发给你的内容粘进去，
按 **Register certificate**。从下一个连接起就换成它，进程不重启。从脚本上做同一
件事是 `PUT /api/certificate`。

签发方给了中间证书的话，把它们粘进同一个框里，放在服务器证书下面，按给的顺序排。服务器证书在
最前面，这也是 TLS 本身要求的顺序，整条链会被整个存下来、整个发出去。

私钥和自己生成的那把一样是加密保存的。它不会显示在页面上，也不会被发回来。

粘进去的内容在存之前就先读一遍，所以写错了会收到答复，而不是被拿去服务：

| 粘进去的是什么 | 会怎样 |
|----------------|--------|
| 里面根本没有 PEM 块的文本 | 拒绝，并指出是两个框里的哪一个读出来是这样 |
| 两个框填反了 | 拒绝，并说它看着就是填反了 |
| 带密码保护的私钥 | 拒绝，并给出把密码去掉的那行 `openssl pkey` |
| 属于另一张证书的私钥 | 拒绝 |
| 已经过期的证书 | 拒绝：没有谁会连上它 |
| 扩展密钥用途里没有 `serverAuth` 的证书 | 拒绝：服务器给出这样的证书，每个客户端都会拒绝，那就连改回来的页面都没有了 |
| 有效期还没开始的证书 | **存下来**，并警告它从什么时候起才管用。两台机器的钟差几分钟是常事，拒绝它会让这种证书根本装不上去 |

被拒绝就什么都没存，所以正在服务的还是原来那张。

### 更换证书

Settings 页面上的 **Make a new certificate** 会把正在用的那张换成另一张自签证书。这和
第一次启动时生成的是同一套流程，所以新证书签给的是这台机器**现在**响应的那些名字和地址，换了地址的机器要的正是这个。
从脚本上做同一件事是 `POST /api/certificate/renew`。

**不用重启。** 证书是每次握手时挑的，所以下一个连接拿到的就是新的。指纹变了，跟着有两件事：

- 按下按钮的那个页面，服务它的还是旧证书。已经开着的连接保持它建立时的那张。重新加载页面才
  看得到新的。
- 每个被告知信任旧证书的浏览器和脚本都会再警告一次，直到新指纹也被信任为止。

换证书时会把新旧两个指纹一起写进日志，这样本安装自己换掉的指纹，和不是它换的，分得出来。

### 关闭 HTTPS

Settings 页面上的 **Serve over HTTPS**，在 API 里是 `api_https_enabled`，决定这件事。和其他设置
一样，它在下一次启动时才生效。

关掉之后，那个端口只说 HTTP。没有证书，也没有重定向，页面上发出去的一切都是明文，这个账号的
密码和 Host 的 SSH 密码都在里头。关着的时候，三个证书接口都答 `409`，因为那时没有正在使用的
证书可读、可换。

证书仍然留在数据库里。重新打开 HTTPS，服务的还是原来那张，指纹也和原来一样。

之所以留了个能关掉的口子，是因为没人背书的证书在有些地方会挡路，而一个连页面都打不开的运维
什么也修不了。

## 首次启动与账号

**API 和界面都要先登录才能用。** 账号只有一个，在第一次启动时创建。第一次启动之前先读这一节，
不然你会登不进去。

1. 第一次启动会创建 `user` 表里唯一的那一行。它**还没有用户名**，并被标成待初始化。
2. 初始密码写进一个叫 `initial-password` 的文件，**就放在数据库文件所在的目录里**，权限
   `0600`。它是 52 个字符的大写字母和数字。
3. **日志里只有路径，从来没有密码。** 日志既打到控制台，也打到一个会保留和轮转的文件里，写在
   那里的密码会在初始化之后继续躺在没人盯着的地方。那个文件是唯一的一份。
4. 用它登录。这时用户名不看，发空的就行。应答里带着 `"setup_required": true`，界面转到初始化
   页面。在那里定下这个账号今后用的用户名和密码。
5. **初始化会删掉初始密码文件。** 里面那个密码从那一刻起就打不开账号了，留着也只是一份读得到
   的废凭据。用初始密码开出来的其他会话同时全部掐断，正在做初始化的这个留着。
6. **初始化做完之前，一个会话只能调 `POST /api/setup`，别的都不行。** `/api` 下面其他路径都
   答 `403`，写着 `The account setup is not finished`。

初始化时定的密码长度必须在 **12 到 72 字节**之间。是字节不是字符：一个汉字就是三个字节。上限
是 72，因为给密码做哈希的 bcrypt 只读到这里，超出的部分根本不参与登录时的校验。太长的密码会
被拒绝，而不是悄悄截断。

初始化只做**一次**。它不问当前密码，所以它不是以后用来改凭据的办法；再调一次答 `409`。

一个会话在最后一次用到它之后还能活 12 小时，每个请求都会把这个期限往后推。会话存在内存里，
所以重启一次就全没了，得重新登录。

### 修改用户名和密码

Settings 页面上两个都能改，从脚本上做同一件事是 `PUT /api/account`。把当前密码和要改的那一项
一起发过来：`username`、`new_password`，或者两个都发。没发的那一项保持不动；两个都不发的请求
会被拒绝，发的值和账号现在的一样也会被拒绝。新密码同样卡在 12 到 72 字节。

**每一次都要当前密码**，只改名字的那次也要。问它，是为了把本人在改和有人捡了一台没人看着的
屏幕上留着的会话这两件事分开；初始化不能做第二次也是同一个道理。

**其他会话全部下线，只留这一个。** 不管改的是哪一项，改名也一样：会话指向的是账号而不是账号
的名字，改名却放着它们不管，等于换了登录用的东西，已经登进来的人却还原封不动待在里面。想改
凭据，一半的理由就是别人可能已经知道了，所以规则就一条：凭据变了，那么除了执行这次修改的会话
以外，全部作废。做这次修改的会话继续能用，连它手上的 CSRF 令牌也继续有效，这样发起请求的那个
页面才能把结果显示出来。应答里会说有多少个其他客户端被下线了。

当前密码不对答 `401`，什么都不改。页面会让你把新密码输两遍；第二遍从不离开浏览器，因为同一个
字符串交给服务器两次，服务器从第二次里学不到任何东西。

```bash
curl -s -b cookies.txt -X PUT "$BASE/api/account" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"current_password":"<the password now>","username":"operator"}'
```

```json
{
  "success": true,
  "data": {
    "username": "operator",
    "username_changed": true,
    "password_changed": false,
    "sessions_ended": 1
  }
}
```

## 内置界面

在浏览器里打开 `https://<地址>:<端口>/`。`/` 会重定向到 `/ui/`，界面就是从那里提供的。第一次
进去时浏览器会对证书发出警告，拿什么去核对见 [HTTPS 与证书](#https-与证书)。

**界面不用部署。** 这些文件编译进了可执行文件，所以没有哪个目录要跟着它走，也没有路径要配。

| 页面 | 路径 | 显示什么、能做什么 |
|------|------|--------------------|
| Status | `/ui/status` | 三个计数（期望、行数、已连接）、一句话说明它们之间的差是怎么回事，以及每条隧道一行：Host、服务端口、状态、服务器、本地、远端、端口是否可达、重试次数、上次连上的时间。出了问题的隧道会在它下面横跨整张表再加一行写清哪里不对；转发端口没连上的隧道，在那里写的是要在它点名的那台 SSH 服务器上改什么、还要查什么。隧道行是一页一页出的，一开始每页十行，页大小和页码在表格上方选；三个计数始终是所有隧道的计数，不是这一页的。它每 5 秒问一次，回来时还停在你正在看的那一页。 |
| Hosts | `/ui/hosts` | 每台 Host 一行，有 ID、IP、端口、用户、描述、是否启用和更新时间。行是一页一页出的，一开始每页十行，页大小（10、20、30、50 或 100）和页码在表格上方选。这个选择只记在这个页面上，而短到一页最小页大小就放得下的列表，索性连控件都不显示。可以添加 Host、编辑、启用或停用、删除。添加和编辑表单里有粘贴私钥的框、拖放密钥文件的区域，还有给带密码的密钥填密码的框；添加表单里有一个默认勾上的 **Assign all service ports**，它决定这台 Host 一开始承载什么。行里的 **Service ports** 会打开一个面板，列出所有服务端口，这台 Host 承载的那些打着勾；保存时只发改动过的部分，所以在那里打一个勾，不会动到你没翻到的那些页。 |
| Service Ports | `/ui/service-ports` | 每个服务端口一行，有 ID、服务 IP、服务端口、本地端口、描述和更新时间。行和 Hosts 一样是一页一页出的，页大小和页码各记各的。可以添加、编辑和删除。添加表单里有一个默认勾上的 **Assign to all hosts**，它决定一开始哪些 Host 承载它；之后哪些 Host 承载它，改起来在 Hosts 页面上。 |
| Logs | `/ui/logs` | 日志文件的末尾，最新的在最下面，可以按级别过滤，也可以选看多少行。它每 5 秒问一次。它读的是进程此刻正在写的那个文件，轮转出去的文件不显示。行按页面的语言显示，文件本身还是英文；见[页面的语言](#页面的语言)。 |
| Settings | `/ui/settings` | 已经存下但还没生效的设置，那张卡片里有一个把它们落实下去的 Restart；所有已存的设置，以及一次保存改了什么（没挑过语言的浏览器看这套安装用哪种语言，也在这里）；正在服务的证书，带一个重新生成它的按钮和两个注册你自己证书的框；这个账号的用户名和密码；把隧道配置和这台管理器的设置各封成一个文件导出，以及把这样的文件导回来的导入；一个把服务停掉再拉起来的 Restart；还有最下面的卸载。见[设置](#设置)。 |
| Manual | `/ui/manual` | 一套安装由什么组成，在一个页面上连图带话讲清楚：这东西做什么、一条隧道从头到尾怎么走、Host 和服务端口以及它们之间的分配关系、转发端口连不上意味着什么、那两个间隔，还有文件放在哪里。它不向服务器要任何东西，所以登录页面也能显示同样的内容。 |
| Login | `/ui/login` | 没有会话的客户端会落到这里。第一次登录时用户名留空。账号还没有用户名时，它会把你带到初始化页面。上面的 **Manual** 按钮会把手册作为面板盖在它上面打开，不需要会话，因为最需要手册的时刻，正是什么都还没跑起来的时候。 |

可执行文件的版本号在每个页面的右下角，登录页面也有。

**页面有浅色和深色两套主题**，开关在每个页面的右上角，登录页面也有。按之前由浏览器决定，而
一个页面开着时浏览器从浅色改成深色，页面不用重新加载就跟着变。按下的选择记在那个浏览器的本地
存储里，键是 `tm_theme`，从不发往任何地方：一个页面用哪套主题看，属于这个页面而不属于这套
安装，两个人看同一台服务器可能想要不一样的。

**页面有十三种语言**，挑语言的地方是每个页面右上角、主题开关旁边的那个列表，登录页面也有。
列表里每种语言都用它自己的名字写：English、한국어、日本語、中文、Español、Français、Deutsch、
Português (Brasil)、Русский、العربية、हिन्दी、Tiếng Việt、ไทย。阿拉伯语是从右往左写的，整个
页面会跟着翻过去。没挑过语言的浏览器看哪种语言，是这套安装的一项设置；那项设置和语言按什么
顺序定下来，见[页面的语言](#页面的语言)。

表单在发出去之前先检查输入。端口只收数字，必须在 1 到 65535 之间；IP 框只收地址里会出现的
字符，而且要能读成 IPv4 或 IPv6 地址。哪里不对就写在哪个字段旁边，改对之前什么都不会离开浏览器。

**拖进来的密钥文件是在浏览器里读的。** 发出去的是密钥的文本，和粘贴进去完全一样；文件本身不会
上传。比私钥可能有的大小还要大的文件，或者拖进来的根本不是文件，都会被拒绝，理由写在拖放区
下面。密钥也可以粘贴，密钥在别处的终端里时用的就是这个。

界面文件是特意不要会话就提供的：它们对每个客户端都是同样的字节，不带任何数据。它们显示的一切
都是从 `/api/**` 取的，而登录守的正是那里。

### 页面的语言

页面用哪种语言画，按下面的顺序定，先找到的那个算数：

1. **这个浏览器角上挑的语言。** 和主题一样，记在那个浏览器的本地存储里，键是 `tm_lang`，
   从不发往任何地方。
2. **这套安装设定的语言。** 就是 Settings 页面上的 `ui_default_language`。谁都没挑的时候
   给所有人看的就是它，有了会话之后才读。
3. **浏览器要的语言。** 拿它和十三种比。先整个标签比，所以设成 `pt-BR` 的浏览器拿到巴西
   葡萄牙语的目录；再只比语言那一段，所以设成 `pt-PT` 的浏览器拿到的也是同一份目录，而不是
   英文。
4. 英文。

角上挑的压过设置，是故意的。设置是给什么都没说的人看的；挑过语言的人，已经在他正在读的
这个页面上说过话了。

**登录页面不跟设置走。** 读设置要有会话，登录页面没有，所以它按这个浏览器挑的语言画，没挑过
就按浏览器要的语言画。这不是毛病，就是这么设计的：登录一过，设置立刻生效，页面不用重新
加载；保存设置的那一刻也一样。

代码是 `en`、`ko`、`ja`、`zh`、`es`、`fr`、`de`、`pt-BR`、`ru`、`ar`、`hi`、`vi` 和 `th`，
`ui_default_language` 收的就是这些；见[设置](#设置)。

**翻译的是页面说的一切**，服务器的应答也包括在内。一次拒绝会带一个 `error_code`，还有这句话
里填进去的值 `error_args`，页面用自己的语言把这个代码对应的句子画出来，而 `error` 还是一直
以来那句英文。一行日志会带一个 `log_id`，Logs 页面按这个标识找到句子，用这一行的字段把它
填上。**日志文件本身还是英文。** `grep` 扫的是它，报问题时附上的也是它；文件要是跟着页面变，
就会用最后谁挑的语言来写。拒绝应答长什么样见[在脚本里调用 API](#在脚本里调用-api)，日志行
长什么样见[设置与卸载](#设置与卸载)。

每种语言是一份目录，从可执行文件里以 `/ui/lang/<code>.json` 提供，所以装在一台哪里都连不上
的主机上，十三种语言也一样都在。每份目录的键都一样，页面能显示的每句话一个键；某种语言还
没有词的键，画的是英文而不是空白。页面只取正在用的语言那份目录和英文那份，别的不取。

**加一种语言**，要动一份目录和四张代码清单。有测试把这五处放在一起对，所以只加了一处、漏了
其他的，坏的是构建而不是页面：

| 哪里 | 什么 |
|------|------|
| `internal/web/static/lang/<code>.json` | 目录，`en.json` 的每个键都要有 |
| `internal/web/static/app.js` 里的 `languages` | 代码、这种语言叫自己的名字，以及是不是从右往左写 |
| `internal/web/static/index.html` 里的 `codes` 和 `rightToLeft` | 同样两张清单，在取到 `app.js` 之前就在 head 里读 |
| `internal/settings/settings.go` 里的 `uiLanguages` | 检查 `ui_default_language` 时对照的清单 |
| `internal/web/web_test.go` 里的 `catalogCodes` | 测试拿来和其他四处对的清单 |

**翻译还做不到的。** 数数的句子只有一个和多个两种形式，英文是这样，俄语和阿拉伯语不是，每份
目录都是挑着措辞、靠这两种撑过去的。阿拉伯语里，以 `/` 开头的路径，那个斜杠可能被画得和后面
的内容分开：这是浏览器把从左往右的文字放进从右往左的一行时的排法，路径本身是完整的。还有，
没有一份目录经过母语者的眼睛，读着别扭的句子值得报上来。

## 设置

**没有配置文件。** 每一项设置都是数据库文件里 `settings` 那唯一一行的一列，改它的地方是
Settings 页面。同样的值通过 `GET /api/settings` 和 `PUT /api/settings` 可读可写。

| 页面上叫什么 | API 里的字段 | 报出来时叫什么 | 默认值 | 什么时候生效 |
|--------------|--------------|----------------|--------|--------------|
| API port | `api_port` | `api.port` | `8888` | 下次启动 |
| Serve over HTTPS | `api_https_enabled` | `api.https_enabled` | `true` | 下次启动 |
| Monitoring interval (seconds) | `monitoring_interval_sec` | `monitoring.interval_sec` | `5` | 下次启动 |
| Reconcile interval (seconds) | `reconcile_interval_sec` | `reconcile.interval_sec` | `5` | 下次启动 |
| Encryption key file | `security_key_file` | `security.key_file` | `keys/tunnel-manager.key` | 下次启动 |
| Log level | `logging_level` | `logging.level` | `info` | **保存的那一刻** |
| Log format | `logging_format` | `logging.format` | `json` | 下次启动 |
| Log file | `logging_file_path` | `logging.file.path` | `logs/tunnel-manager.log` | 下次启动 |
| Log size before it is rotated (MB) | `logging_file_max_size` | `logging.file.max_size` | `100` | 下次启动 |
| Rotated log files kept | `logging_file_max_backups` | `logging.file.max_backups` | `5` | 下次启动 |
| Days a rotated log file is kept | `logging_file_max_age` | `logging.file.max_age` | `30` | 下次启动 |
| Compress rotated log files | `logging_file_compress` | `logging.file.compress` | `true` | 下次启动 |
| Language this installation is shown in | `ui_default_language` | `ui.default_language` | 空，即不指定任何语言 | **保存的那一刻** |

**保存的那一刻就生效的设置有两项：日志级别和语言。** 日志级别会传到启动时发出去的每一个
日志器，包括数据库用来报自己语句的那个，而这通常正是开 `debug` 想看的另一半。语言这个进程
根本不读：是浏览器在读，从保存它的那次请求的应答里读，之后每次读取也都读得到，所以没有什么
要靠重启来落实。空的语言是一个值，不是没填：它的意思是这套安装不指定语言，浏览器看它自己
要的语言，也就是这项设置出现之前每个浏览器看到的那样。其余全部只是存下来，下次启动时才读；
页面上每个字段都写着这件事，保存的应答里也把每处改动标成 `now` 或 `restart`。

值不合规矩的保存，在存下来之前就被拒绝：

| 设置 | 规矩 |
|------|------|
| `api_port` | 1 到 65535 |
| `monitoring_interval_sec`、`reconcile_interval_sec` | 大于零 |
| `security_key_file` | 不能为空 |
| `logging_level` | `debug`、`info`、`warn`、`error`、`dpanic`、`panic` 或 `fatal` |
| `logging_format` | `json` 或 `console` |
| `logging_file_max_size`、`logging_file_max_backups`、`logging_file_max_age` | 零或更大 |
| `ui_default_language` | 空，或者 `en`、`ko`、`ja`、`zh`、`es`、`fr`、`de`、`pt-BR`、`ru`、`ar`、`hi`、`vi`、`th` 中的一个，一字不差：`EN` 和 `ko-KR` 都会被拒绝 |

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

请求体是绑到已存的值上面的，所以只点名了部分设置的请求就只改那几项，其余不动。

一次读取答的是已存的设置，旁边还带上这个进程还没跑上的那些：

```bash
curl -s -b cookies.txt "$BASE/api/settings"
```

```json
{
  "success": true,
  "data": {
    "api_port": 9999,
    "logging_level": "debug",
    "...": "...",
    "pending_restart": [
      { "name": "api.port", "running": "8888", "stored": "9999" }
    ]
  }
}
```

`pending_restart` 是服务器在每次读取时现算的：拿它启动时读到的设置和现在存着的比一比，
`running` 是这个进程正跑着的值，`stored` 是重启之后会变成的值。它对每个客户端、每个会话都
一样，而重启会把它清空却不需要谁去清，因为进程回来时跑的就是存着的值。日志级别和语言从不出现
在里面，因为两者保存的那一刻就已经到位了。已存的设置全都跑上了的安装，拿到的是 `[]`。

### 重启服务

Settings 页面上有一个 **Restart** 按钮，从脚本上做同一件事是 `POST /api/restart`。要让一项
得等下次启动才生效的设置落实下去，靠的就是它。API 会停止应答，每条隧道都会断掉，回来的路上再重建，
所以走隧道的一切在重启期间都是断的。它不像下面的卸载那样要账号密码：这里没有什么是不可挽回的。

应答先写出去，进程大约三秒后才走，这三秒是留给浏览器把“服务正在回来”那个页面画出来的时间。
之后的关停按信号触发时的顺序走：先排空 API 服务器，再排空重定向服务器，释放端口，停掉调谐
循环，最后逐条断开隧道。这一切全部结束之后，进程才用同样的参数、同样的环境，把程序原地再跑一遍。

**还是同一个进程。** Unix 是把一个正在跑的进程的映像换掉，而不是另起一个，所以 PID 不变，
systemd 和 Docker 看不出发生过什么，没有东西被启动两次，也不会冒出第二个实例来抢端口。端口
在换映像之前就释放了，因为接替它的程序过一会儿要绑同一个端口。先把磁盘上的可执行文件换掉再
重启，跑起来的就是新的：文件是在那一刻读的。

**Windows 没有 exec。** 在那里重启就是一次有序的停止，仅此而已，把程序再跑起来的事留给管着
这个服务的东西；手动启动的那种回不来。两种应答里都带着 `comes_back`，这样在按下之前页面就能
说清这套安装属于哪一种，而 `GET /api/restart` 什么都不做就能答这个。

会话存在内存里，所以随进程一起没了。服务回来之后，页面会重新要求登录。

```bash
curl -s -b cookies.txt -X POST "$BASE/api/restart" \
  -H "X-CSRF-Token: $CSRF"
```

```json
{
  "success": true,
  "data": {
    "exit_in_sec": 3,
    "comes_back": true
  }
}
```

### 服务起不来的时候

从前，一项能让进程起不来的设置，写在一个你可以直接打开来改的文件里。现在它在数据库里，而能改它的那个页面，
是由那个起不来的服务器提供的。`-reset-settings` 就是这时候的出路。

```bash
./tunnel-manager -db <path> -reset-settings
```

它把每一项设置都还原成默认值，打印改了什么然后退出。下一次启动跑的是默认值，Settings 页面也
就又能打开了。

**还原的只有设置。** 注册的 Host、服务端口、账号和证书都在同一个数据库文件里，原样留着：什么
都不用重新注册，你也还是用手上那个密码登录。

已存的一组设置过不了上面那些规矩时，它会说出来，并点名这个参数：

```text
fatal  failed to read the settings  {"error": "the stored settings are refused: invalid API port: 0.
       Start with -reset-settings to put every setting back to its default"}
```

## 卸载

Settings 页面最下面可以把这套安装删掉。它会停掉每条隧道，删掉组成这套安装的那些文件，
然后结束进程。从脚本上做同一件事是 `POST /api/uninstall`。

> **删掉加密密钥是不可挽回的。** 每台 Host 的 SSH 密码都是用那把密钥封住的。事先备份的数据库
> 也救不了：里面的密码依旧读不出来，每台 Host 都得带着密码在一套全新的安装上重新注册一遍。

| 会删掉 | 不动 |
|--------|------|
| 数据库文件，以及 SQLite 在旁边留的 `-wal` 和 `-shm` 文件 | **程序文件本身** |
| 加密密钥文件 | 启动它的那条服务配置 |
| 初始密码文件，如果它还在 | 这些文件所在的目录 |
| 日志文件，以及旁边轮转出来的日志文件 | |

**程序文件本身不删。** 在 Windows 上，正在跑的进程删不掉自己的映像；在 Unix 上，它也要等到
进程结束才真正从磁盘上消失，那只能算做了一半，算不上做完。请手动把它删掉；如果这是按服务装
的，把 systemd unit 或者 compose 文件也一并删掉。

**账号密码会再问一遍**，并在动任何东西之前先核对。不然的话，一台没人看着的屏幕上留着的会话，
离这件事就只有一下点击的距离，而密码恰恰是路过的人给不出来的东西。密码不对答 `401`，什么也
不会被停掉。

顺序是有讲究的，而且定死：先停调谐循环，因为它是会把隧道重新拉起来的那个；然后逐条断开隧道，
这样远端主机上不会留下没人管的监听；再关数据库、删文件；再把应答写出去；进程大约三秒后结束，
好让浏览器有时间收到它。

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

删不掉的文件会连同原因一起列在 `failed` 里，留在磁盘上由你处置。它不会挡住其余的步骤：在
Windows 上，这个进程正在写的日志文件开着就删不掉，而卡在那里停下来的一次卸载，会为了这一个
本来就删不掉的文件，把数据库和密钥都留在原地。

## 在脚本里调用 API

**`/api` 下面的每条路径都需要会话，而每个 `POST`、`PUT` 和 `DELETE` 还需要一个 CSRF 令牌。**

1. 用用户名和密码调 `POST /api/login`。把它设下的 cookie `tm_session` 和 `tm_csrf` 留着。
2. 从应答里取出 `data.csrf_token`，在**每一个** `POST`、`PUT` 和 `DELETE` 上作为
   `X-CSRF-Token` 头发出去。
3. `GET` 不需要令牌。它够得到的东西什么都不改。

CSRF 是跨站请求伪造：别的站点让你的浏览器带着你的 cookie 发请求。令牌能挡住它，是因为那个
站点读不到你登录的应答，也设不了这个头。

服务器根本不发 `Access-Control-Allow-Origin` 头。那是浏览器施加在页面上的规则，不是这台
服务器做的检查，所以 `curl`、脚本和服务器之间的调用都不受影响；在另一个源上的页面则不行。

下面的例子是一整个会话。它用了一个 cookie jar 文件：`-c` 写入服务器设下的 cookie，`-b` 把
它们发回去。

```bash
BASE=https://127.0.0.1:8888

# 证书是自签的，所以告诉 curl 去哪里找它。Settings 页面按 PEM 显示证书；
# 换成每次调用都加 -k 则是跳过检查。
# 见 [HTTPS 与证书](#https-与证书)。
export CURL_CA_BUNDLE=tm-cert.pem

# 1. 登录。-c 把 tm_session 和 tm_csrf 存进 cookies.txt。
curl -s -c cookies.txt -X POST "$BASE/api/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"operator","password":"<your-password>"}' > login.json

# {"success":true,"data":{"setup_required":false,"csrf_token":"<token>"}}

# 2. 从应答里把令牌取出来。
CSRF=$(python3 -c 'import json; print(json.load(open("login.json"))["data"]["csrf_token"])')

# 3. 读操作只要 cookie，别的都不用。
curl -s -b cookies.txt "$BASE/api/status"

# 4. 写操作还要加上这个头。
curl -s -b cookies.txt -X POST "$BASE/api/host" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"ip":"192.0.2.10","port":22,"user":"ubuntu","password":"<host-password>","description":"example"}'

curl -s -b cookies.txt -X POST "$BASE/api/service-port" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"service_ip":"198.51.100.20","service_port":8080,"local_port":18080}'

# 5. 脚本跑完就退出登录。
curl -s -b cookies.txt -X POST "$BASE/api/logout" -H "X-CSRF-Token: $CSRF"
```

第一次登录时，用空用户名和初始密码登进去，然后先把初始化做完再做别的。`initial-password`
就在数据库文件所在的目录里。

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

会出什么问题，以及它长什么样：

| 应答 | 是什么意思 |
|------|------------|
| `401 Authentication required` | 没发会话 cookie，或者会话已经过期。重新登录。 |
| `403 The request carries no valid X-CSRF-Token header...` | 这个写操作没带令牌，或者带错了。把登录应答里的 `data.csrf_token` 发过来。 |
| `403 The account setup is not finished...` | 账号还没有用户名。先调 `POST /api/setup`。 |
| `401 Invalid username or password` | 登录被拒。它故意不说是两者中的哪一个错了。 |
| `400 The settings are refused: ...` | 某项设置破了上面的规矩。什么都没存下。 |
| `401 The password does not open this account` | 卸载时密码给错了。什么都没停，也什么都没删。 |

每个应答的形状都一样：`{"success":true,"data":...}` 或 `{"success":false,"error":"..."}`。
说不的应答还多两个字段：`error_code` 是这次拒绝的名字，`error_args` 装着填进那句话里的值，
用的就是句子里的名字，没有值时不出现。不管页面是哪种语言，`error` 都是那句英文，所以一直在
读它的脚本照样能跑；页面翻译的是代码，脚本也该认代码而不是认字句。

```json
{
  "success": false,
  "error": "No such service port: 99",
  "error_code": "assignment.service_port.not_found",
  "error_args": { "ids": "99" }
}
```

## API 接口

`/api` 下面的一切都需要会话，只有 `POST /api/login` 例外。不是 `GET` 的一切都需要
`X-CSRF-Token` 头。

### 分页

`GET /api/host`、`GET /api/service-port` 和 `GET /api/status` 一次答一页。一把全给的列表会
跟着安装一起长大：应答本身、构造它用的内存、装下它的页面，都随行数增长，而这些东西没有哪个
是一眼能看完的。

| 参数 | 默认值 | 收什么 |
|------|--------|--------|
| `page` | `1` | 页码，从 1 数起。小于 1 当作 1，超过最后一页的页码答的是**最后一页**，而不是报错 |
| `size` | `10` | 一页放多少行。只能是 `10`、`20`、`30`、`50` 和 `100` 之一；别的值答 `400` |

超过末尾的页码不算错，是因为页面开着的时候行会被删掉：客户端停在的那一页，等它再问一次时
可能已经不在了，那里报错会让本该显示剩余行的页面变成空白。一个什么都没存的列表，就是第 1 页
加一个空的 `items`。

`size` 只从那个清单里取，而不是任意数字，因为可以随便挑大小就等于可以要求把所有行装进一个
应答里，而分页正是为了挡住这件事。

**`/api/host` 和 `/api/service-port` 的形状变了。** `data` 从前是行的数组，现在是一个装着
这一页的对象：

```json
{
  "success": true,
  "data": {
    "items": [ "..." ],
    "total": 25,
    "page": 2,
    "size": 10
  }
}
```

`items` 是这一页，`total` 是一共有多少行，`page` 和 `size` 是实际答出来的页码和页大小，
它们不总等于请求要的。原来读 `data[0]` 的客户端，现在读 `data.items[0]`。

`/api/status` 本来就是个对象。`tunnels` 现在是隧道行的一页，`page` 和 `size` 排在它旁边，
而**三个计数是对所有行算的，不是对这一页算的**：它们说的是这套安装在做什么，不是眼前这一页
上有什么。

```bash
# 二十台一页的 Host 的第二页，以及隧道行的最后一页：超过末尾会答最后一页，
# 所以写一个很大的数就是在要最后一页。
curl -s -b cookies.txt "$BASE/api/host?page=2&size=20"
curl -s -b cookies.txt "$BASE/api/status?page=99999&size=10"
```

### 账号

| 方法 | 路径 | 做什么 |
|------|------|--------|
| `POST` | `/api/login` | 收 `username` 和 `password`，设下会话和 CSRF 两个 cookie，答里带着 `setup_required` 和 `csrf_token` |
| `POST` | `/api/logout` | 丢掉会话，并让两个 cookie 过期 |
| `POST` | `/api/setup` | 给还没有用户名和密码的账号定一次用户名和密码 |
| `GET` | `/api/account` | 这个账号叫什么 |
| `PUT` | `/api/account` | 收 `current_password`，以及 `username`、`new_password` 或两者，改掉它们并让其他会话全部下线 |

### Host

| 方法 | 路径 | 做什么 |
|------|------|--------|
| `POST` | `/api/host` | 创建一台 Host。`enabled` 可以不填，没说的 Host 就是启用的 |
| `GET` | `/api/host` | Host 的一页，旧的在前。收 `page` 和 `size`，见[分页](#分页) |
| `GET` | `/api/host/:id` | 读一台 Host |
| `PUT` | `/api/host/:id` | 更新一台 Host。每个字段都可以不填；`enabled` 为 false 会停掉它的隧道 |
| `DELETE` | `/api/host/:id` | 删掉一台 Host，以及提到它的分配关系 |
| `GET` | `/api/host/:id/service-port` | 服务端口的一页，上面叠着这台 Host 的分配关系，见[一台 Host 承载的服务端口](#一台-host-承载的服务端口) |
| `PUT` | `/api/host/:id/service-port` | 给这台 Host 加上和去掉分配关系 |

创建和更新的请求体收这些字段。

| 字段 | 创建时 | 更新时 |
|------|--------|--------|
| `ip`、`port`、`user` | 必填 | 可不填；没写的保持原样 |
| `private_key` | PEM 私钥文件的文本内容。给了 `password` 就可以不填 | 空的或者没发，保留已存的密钥。发上来的密钥会把已存的密钥连同它的密码一起替换掉 |
| `key_passphrase` | 只有带密码保护的密钥才要填 | 跟着它所属的密钥一起发。单独发而没有 `private_key`，会被拒绝 |
| `password` | 给了 `private_key` 就可以不填 | 空的或者没发，保留已存的密码 |
| `description`、`enabled` | 可不填 | 可不填 |
| `assign_all_service_ports` | 可不填。不填的话，现存的服务端口全部分给这台 Host。发成 false 就注册一台什么都不承载的 Host | 不读。一台 Host 承载什么，改起来走 `PUT /api/host/:id/service-port` |

既没有密钥又没有密码的创建会被拒绝，用不了的密钥也一样。拒绝时会说清是哪一种：这个值不是
PEM、密钥有密码保护但没把密码发来、或者密码打不开这把密钥。这几种情况下什么都不会被存下来。

**任何应答里都不会有 `private_key`、`key_passphrase` 或 `password`**，包括这里的。存下来的
东西对不对，由 Host 连上与否来确认，状态里写着。

```bash
# 一台用密钥登录的 Host。密钥是按文件的文本内容发的，所以里面的换行必须原样保留：
# 这里用 jq 来读文件。
jq -n --arg key "$(cat ~/.ssh/id_ed25519)" \
  '{ip:"192.0.2.10",port:22,user:"ubuntu",private_key:$key,description:"example"}' |
curl -s -b cookies.txt -X POST "$BASE/api/host" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  --data-binary @-
```

### 一台 Host 承载的服务端口

**`GET /api/host/:id/service-port` 答的是服务端口的一页，每一项上带着 `assigned`**，说明
这台 Host 承载不承载它。分页是在服务端口上分的，不是在分配关系上分的，排序和
`GET /api/service-port` 一样按 id 来，所以不管这台 Host 承载与否，同一行在两个列表里都落在
同一页上。它收 `page` 和 `size`，见[分页](#分页)。不存在的 Host 答的是 `404`，而不是把所有
服务端口都列出来、一个都没分配，因为后者正是一台什么都不承载的 Host 的样子。

```json
{
  "success": true,
  "data": {
    "items": [
      { "id": 1, "service_ip": "198.51.100.20", "service_port": 8080,
        "local_port": 18080, "description": "", "assigned": true }
    ],
    "total": 1,
    "page": 1,
    "size": 10
  }
}
```

**`PUT /api/host/:id/service-port` 收的是改动，不是整套。** 列表是一页一页提供的，客户端手里
只有一页，对没读到的那些页上的行一无所知；从那里发出来的“整套”只点得到这一页，而这一页之外的
分配关系，会被一个本意只是勾一个框的请求全部删掉。

```bash
curl -s -b cookies.txt -X PUT "$BASE/api/host/1/service-port" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"add":[1,3],"remove":[2]}'
```

```json
{ "success": true, "data": { "added": 2, "removed": 1 } }
```

`added` 和 `removed` 数的是行，不是请求里的项：已经分配过的服务端口再要一次，不会写新行；
本来就没分配的被移除，也不会有行消失。两个列表都可以不填，什么都不改的请求会被应答，而不是
被拒绝。

| 发来的是什么 | 会怎样 |
|--------------|--------|
| 同一个服务端口既在 `add` 又在 `remove` 里 | `400`，并点名它。谁赢都是在猜这个请求到底想干什么，而它决定的是一条隧道跑不跑 |
| 一个没存过的服务端口 id | `400`，并点名它。什么都不写 |
| 一个没存过的 Host id | `404` |

这次改动要么整个落地，要么一点都不落地；提交之后会唤醒调谐循环，所以隧道随即跟上。

### 服务端口

| 方法 | 路径 | 做什么 |
|------|------|--------|
| `POST` | `/api/service-port` | 创建一个服务端口。`assign_to_all_hosts` 可以不填：不填时，现存的每台 Host 都分到它；发成 false 则注册成没有 Host 承载 |
| `GET` | `/api/service-port` | 服务端口的一页，旧的在前。收 `page` 和 `size`，见[分页](#分页) |
| `GET` | `/api/service-port/:id` | 读一个服务端口 |
| `PUT` | `/api/service-port/:id` | 更新一个服务端口。`service_ip`、`service_port` 和 `local_port` 都是必填的 |
| `DELETE` | `/api/service-port/:id` | 删掉一个服务端口，以及提到它的分配关系 |

### 状态

| 方法 | 路径 | 做什么 |
|------|------|--------|
| `GET` | `/api/status` | 这套安装的几个计数，以及隧道行的一页。收 `page` 和 `size`，见[分页](#分页) |
| `GET` | `/api/status/:hostId` | 这台 Host 和它的隧道。不分页：一台 Host 承载几个服务端口就有几条隧道 |

### 设置与卸载

| 方法 | 路径 | 做什么 |
|------|------|--------|
| `GET` | `/api/settings` | 已存的设置，以及在 `pending_restart` 里这个进程还没跑上的那些 |
| `PUT` | `/api/settings` | 把请求体里的设置盖到已存的设置上，答里说改了什么、要不要重启 |
| `GET` | `/api/certificate` | 正在提供的证书：指纹、主体、签发者、覆盖的名字、有效期和剩余天数 |
| `POST` | `/api/certificate/renew` | 再做一张自签证书，从下一个连接起提供它 |
| `PUT` | `/api/certificate` | 收 `cert_pem` 和 `key_pem`，存下来并从下一个连接起提供它们 |
| `GET` | `/api/restart` | 在这里重启会发生什么：服务多久之后停，以及它会不会自己回来 |
| `POST` | `/api/restart` | 按顺序把服务停掉，在有 exec 的平台上把程序原地再跑一遍 |
| `POST` | `/api/uninstall` | 收 `password`，删掉这套安装并结束进程 |
| `GET` | `/api/logs` | 日志文件的末尾。`lines` 说要多少行，最多 2000 |

`api_https_enabled` 关着的时候，三个证书接口都答 `409`，因为那时没有正在使用的证书。无论是
换证书的应答还是读取的应答，都不带私钥：它是用封住 SSH 密码的那把密钥加密存着的，从不离开
这个进程。`cert_pem` 可以是一条链，服务器证书在最前面，中间证书跟在后面。

换上的证书从下一个连接起生效，已经开着的连接不受影响：TLS 在握手时就把证书定下来了，之后
连接再也不回头看。所以重新生成证书的应答是从旧证书上过来的，浏览器也会一直显示旧指纹，直到
页面重新加载。换证书时，新旧两个指纹都会写进日志。

`/api/logs` 的应答带着这些行，也带着取到它们的经过：`path` 是它读的文件，`requested` 和
`max_lines` 说要的是多少、上限是多少，`capped` 说这次是不是撞上了上限，`size` 和 `read` 是
文件的大小和从末尾读了多少。文件是从末尾读的，所以文件再大 `read` 也还是小的。

每一行回来时都拆成 `level`、`time`、`caller`、`message` 和 `extra`，`raw` 里放着这行原本
写下的样子，`parsed` 说拆得成不成。拆不开的行照样会返回，只是 `parsed` 是 false：看着不对劲
的那一行，恰恰是最值得读的。

`message` 是文件里写的那句英文，`extra` 的字段里有一个 `log_id`，是这一行的名字，形如
`tunnel.connected`：Logs 页面拿它查到句子，用自己的语言显示，值取自同一行的其他字段。还没有
名字的老版本写的行，或者别的什么写进这个文件的行，没有 `log_id`，就照写下的样子显示。

### 导出与导入

| 方法 | 路径 | 做什么 |
|------|------|--------|
| `POST` | `/api/export/tunnels` | 收 `password`，把每台 Host 和每个服务端口封进一个文件答出来 |
| `POST` | `/api/import/tunnels` | 收 `password`、`file` 和 `overwrite`，把文件里的内容写进来 |
| `POST` | `/api/export/settings` | 收 `password`，把已存的设置封进一个文件答出来 |
| `POST` | `/api/import/settings` | 收 `password` 和 `file`，把文件里的设置存下来 |

这四个接口把一套配置从一套安装带到另一套。导出给你一个文件，导入收一个文件，所以文件放在哪里、
放多久由你决定，两套安装之间也不需要互相够得着。

**导出给你的是一行文本。** 开头是 `tmpwenc:v1:` 这个标记，后面全是 base64，所以整个文件都是
ASCII，粘到输入框、消息或者工单里也不会因为换行而损坏。里面装的是派生密钥时用的参数、盐、
随机数，以及加密后的配置。参数和盐不加密，只做防篡改校验，这样才能把密码不对和文件损坏区分开
来告诉你。文件里没有任何一处是人能直接读的。导入界面收的是长长的一行，把文件拖进去和把内容
粘进去是同一件事，原因都在这里。

**文件里写着每台 Host 承载哪些服务端口**，放在 Host 的 `assigned_local_ports` 里，用本地端口
来指认而不是用行的 id：id 只在文件来的那套安装里有意义，而本地端口在一套安装的所有服务端口里
是唯一的，所以两边指的是同一个东西。这个列表是导入之后这台 Host 承载的全部，不是在原有基础上
再加：导入写进来的 Host，承载的就是文件里点的那些，别的都没有；被跳过的 Host 则保留它原有的
分配关系。文件里点到而这套安装没有的本地端口，会作为被跳过的分配关系列在应答里，带着 Host 和
端口，导入的其余部分照常算数。

在分配关系还没被存下来之前写出的文件里没有这个字段，那样的文件里每台 Host 都当作承载文件里的
每一个服务端口。那种文件当初没说出口的意思正是如此，而读成“什么都不承载”的话，它会干干净净地
导入完，然后留下一套一条隧道都没有的安装。什么都不承载的 Host 在文件里写成 `[]`，这是同一件事
的另一面。

**文件里明文装着每台 Host 的 SSH 密码、私钥和密钥密码。** 它就是干这个的：数据库里那些东西是
用存放它们那台机器的加密密钥封住的，照原样带走的文件在别的安装上一个也打不开。所以它们在出去
的路上被解封，再用接收方那套安装的密钥重新封住。这中间护着它们的，只有给整个文件加的那个密码，
别无他物，所以请把导出的文件当作它里面每台 Host 的凭据来对待。

导出用 `POST` 而不是 `GET`，因为密码在请求体里。放在 URL 里，它会被写进这台服务器的访问日志，
也会进发起请求的那个浏览器的历史记录。这个密码同样卡在账号密码那样的 12 到 72 字节，而且哪里
都不存：密码忘了的文件，谁也打不开，这个程序自己也打不开。

导入会把这里没有的加进来，已经有的**跳过**，并在应答里说清跳过了什么、为什么跳过。把同一个
文件再发一次、把 `overwrite` 设成 true，就是拿它去替换那些行；被替换的行保留原来的 id，所以
那台 Host 的隧道是重连而不是重建。文件里的每一行都会在应答里列成 `added`、`replaced` 或
`skipped`，决定要不要覆盖之前看的就是它。整个导入是一个事务：中途被拒绝的文件，留下的数据库
和原来分毫不差。隧道本身不随文件走，因为调谐循环会从 Host、服务端口和它们之间的分配关系里把
隧道建出来。

设置的导入只把设置**存下来**，一项都不加到正在跑的进程上，`api_port` 和 `api_https_enabled`
也不例外。存下来的是下次启动时跑的东西，在那之前 `GET /api/settings` 会把差别报在
`pending_restart` 里，所以一次导入不会把端口从承载着它的那个请求底下抽走。设置过不了 Settings
页面那些规矩的文件会被拒绝，什么都不存。

打不开的文件会说清是四种里的哪一种：密码不对、这不是这个程序写出来的文件、文件损坏了，或者
它装的是另一种内容。

```bash
# 导出，文件请放在你放机密的地方。
curl -s -b cookies.txt -X POST "$BASE/api/export/tunnels" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"password":"<the password that seals the file>"}' |
jq -r '.data.file' > tunnels.tmexport

# 在另一套安装上导入它。
jq -n --arg file "$(cat tunnels.tmexport)" \
  '{password:"<the same password>",file:$file,overwrite:false}' |
curl -s -b cookies.txt -X POST "$BASE/api/import/tunnels" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  --data-binary @-
```

### 界面

| 方法 | 路径 | 做什么 |
|------|------|--------|
| `GET` | `/` | 用 `302` 重定向到 `/ui/` |
| `GET` | `/ui` | 用 `302` 重定向到 `/ui/` |
| `GET` | `/ui/version.json` | 可执行文件的版本号，形如 `{"version":"3.0.0"}` |
| `GET` | `/ui/lang/<code>.json` | 一种语言的目录，从 `en` 到 `th`；见[页面的语言](#页面的语言) |
| `GET` | `/ui/*` | 从可执行文件里把界面提供出去 |

`/ui/version.json` 和 `/ui/` 下面其余的东西一样，不需要会话。登录页面上也显示版本号，何况
在一个公开仓库里，这个号在发布页面上本来就看得到。

Host 的 SSH 密码和账号的密码哈希，在任何应答里都不会出现。

## 读懂隧道状态

```bash
curl -s -b cookies.txt https://127.0.0.1:8888/api/status
```

```json
{
  "success": true,
  "data": {
    "desired_tunnels": 1,
    "total_tunnels": 1,
    "connected_tunnels": 0,
    "page": 1,
    "size": 10,
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
        "remote": "198.51.100.20:8080",
        "server_banner": "",
        "forward_reach": "unknown"
      }
    ]
  }
}
```

`tunnels` 是这些行的一页，先按 Host 排再按服务端口排，`page` 和 `size` 说明这是多大的第几页。
三个计数是对所有行算的：一套有二十五条隧道的安装，在十行一页上报出来的还是二十五，而
`connected_tunnels` 数的是这套安装里连上的隧道，不是恰好落在这一页上的那些。见[分页](#分页)。

三个计数回答的是三个不同的问题，它们之间的差各有各的意思。

| 计数 | 数的是什么 |
|------|------------|
| `desired_tunnels` | **应该**跑着多少条隧道：Host 处于启用状态的那些分配关系，算法和调谐时构造期望状态的一样 |
| `total_tunnels` | 存在多少条隧道**行**，凡是启动过的隧道都各占一行，不管它最后落在什么状态 |
| `connected_tunnels` | 这些行里有多少条写着 `connected` |

| 差 | 是什么意思 |
|----|------------|
| `desired > total` | 一条本该跑着的隧道根本没被启动过。要么是调谐还没跑到，那只是一瞬间的事；要么是调谐启动不了它，比如已存的密码用现在这把加密密钥解不开。原因在日志里。 |
| `total > connected` | 隧道启动了，但没在搬运流量。它那一行的 `status` 和 `last_error` 里写着为什么。 |

`status` 是下面几种之一：

| 值 | 含义 |
|----|------|
| `starting` | 行已经写下，SSH 连接正在建 |
| `connected` | Host 上的监听开着 |
| `reconnecting` | 连接断了，或者一次 keepalive 没人回，正在重建 |
| `error` | 这次尝试失败了。`last_error` 里写着原因 |

### 转发端口是否可达

写着 `connected` 的隧道，说明 SSH 连接是立着的。这不等于转发端口连得上：监听是由 **SSH
服务器**打开的，它绑哪个地址是那台服务器的决定，不是这边的。tunnel-manager 要的是
`0.0.0.0:<本地端口>`，而 OpenSSH 保持默认的 `GatewayPorts no`，或者 Dropbear 没带 `-a`
启动时，服务器只绑回环。那样这个端口只在 Host 自己身上应答，别处都不应答。

每条隧道行上有两个字段，说明我们对这件事知道些什么。

| 字段 | 里面是什么 |
|------|------------|
| `server_banner` | SSH 服务器在握手时自报的名号，比如 `SSH-2.0-OpenSSH_10.5p1 Ubuntu-1ubuntu2`。它告诉你面前是哪一种服务器，而要在上面改什么，各家不一样 |
| `forward_reach` | tunnel-manager 有没有连上那个转发端口，做法是朝 Host 的那个端口开一条 TCP 连接：`reachable`、`unreachable`，还什么都没测过时是 `unknown` |

它在隧道起来时测一次，此后每次重连再测一次，而不是每读一次状态就测：决定这件事的是 SSH
服务器的配置，而它不会在一条立着的连接底下变来变去。

> **`unreachable` 说的是从哪里没连上，不是为什么没连上。** 一台只把端口绑在回环上的服务器，
> 和一道在半路把连接丢掉的防火墙，从这边看起来一模一样，而一条根本没到的连接分不出这两者。
> 改任何一边之前，两边都查一下。

`GET /api/status/:hostId` 答的是同样的计数，只是没有 `desired_tunnels`，另外还带上 Host
本身。它不分页，这台 Host 的每条隧道都在里面。

## 加密密钥

Host 的 SSH 密码在存下来之前，用 AES-256-GCM 加密。密钥从 **Encryption key file** 这项设置
指定的文件里读，默认是 `keys/tunnel-manager.key`。那个文件不存在的话，第一次启动会造一把 32
字节的密钥，权限 `0600`；已经存在的话，照原样读。

> **密钥丢了，已存的密码就再也读不出来。** 除了把每台 Host 重新注册一遍，没有别的办法。请把
> 密钥文件和数据库文件一起备份，否则两者会对不上。

密钥文件如果同组或其他人读得到，启动会拒绝继续。用 `chmod 600` 把权限收窄再启动。

如果这把密钥一个已存的密码都打不开，而其中至少有一个被标记为加密过的，启动就会停下来，而不是
提供一个看着健康、却连不上任何一台 Host 的 API。如果能打开一部分、打不开另一部分，打不开的
那些会在一条警告里被点名，它们的隧道不会建起来，已存的值原封不动：一个打不开的密码，别处再也
没有第二份，被覆盖掉就真没了。请通过 API 把那几个重新设一遍。

这项设置里写相对路径，是相对数据库文件所在的目录来解析的，所以默认值把密钥放在数据库旁边的
`keys/` 里。绝对路径原样使用，这也正是把密钥放到别处、比如放到单独一个卷上的办法。启动时会把
打开的那个文件的绝对路径记进日志，所以日志里写着读的是哪把密钥。

## 以非 root 用户运行

这个进程不用 root 也能启动。它会记一条 `not running as root` 的警告，然后能抬多少抬多少。

- 它会试着把文件描述符上限抬到 65535。没有 root 时，软上限最高只能抬到硬上限，而硬上限更低
  时，它会记一条 `max ulimit is low` 然后继续跑。要跑很多隧道的话，请事先把硬上限调高。
- 1024 以下的 API 端口，非 root 进程绑不上。请用 1024 或更大的端口，或者给可执行文件加上
  `CAP_NET_BIND_SERVICE`。
- 这个进程必须**能写数据库文件所在的那个目录**。初始密码文件在第一次启动时写到那里，写不进去
  的目录会让启动停下来，因为一个谁也读不到密码的账号，就是一个谁也登不进去的 API。

`service_ports.local_port` 小于 1024 的话，Host 上的 sshd 不会去开它。那个监听是 sshd 创建
的，不是 Tunnel Manager 创建的，所以这条限制卡的是为这台 Host 注册的那个 SSH 账号，而不是
Tunnel Manager 自己跑在哪个账号下。正如 ssh(1) 所说，特权端口只为 root 用户转发。Host 上的
SSH 账号不是 root 的话，请把 `local_port` 留在 1024 或更大。

### 文件归属与服务账号

没有第二个目录要安排。密钥和日志默认落在数据库文件所在目录下的 `keys/` 和 `logs/` 里，所以
一个有权写那个目录的账号，要的东西就都齐了。日志文件建不出来不会让启动停下：文件日志关掉，
控制台照样什么都有，原因写在 `logging to file is disabled` 这条警告里。

用 systemd 的话，`_scripts/systemd/tunnel-manager.service` 里带的是 `User=root`。改账号时
别动 `StateDirectory=`：

```ini
[Service]
User=tunnel-manager
Group=tunnel-manager
```

`StateDirectory=tunnel-manager` 会让 `/var/lib/tunnel-manager` 归 `User=`/`Group=` 所有，
本来就在那里、属于 root 的目录也会跟着换主。已有的密钥文件也得让那个账号读得到，所以请改它的
属主，权限还留在 `0600`。

容器是以 root 跑的，因为 `Dockerfile` 最后是 `USER root`。想换个身份跑的话，给
docker-compose.yaml 里的服务加上 `user: "<uid>:<gid>"`，并把宿主机上的 `./_data` 改成归那个
uid 所有。要是它曾经以 root 跑过，那个目录属于 root，得先改过来。文件描述符上限来自
docker-compose.yaml 里的 `ulimits`，和容器里用哪个账号没有关系。

## 许可证

MIT License
