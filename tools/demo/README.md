# Demo recording

`run.sh` records `docs/demo.gif`, the animation at the top of the README. It
runs everything on this machine and stops what it started when it is done.

```bash
tools/demo/run.sh                    # writes docs/demo.gif
tools/demo/run.sh /tmp/try.gif       # writes somewhere else
```

What it does, in order:

1. Builds the image in this directory: an OpenSSH server that lets clients
   forward ports and bind them on every interface (`GatewayPorts yes`), with a
   user `demo` whose password is `demo-host-password`. A small web server inside
   it answers on `127.0.0.1:80` of the container only, unless `WEB_ADDR` says
   otherwise, and `SSH_FROM` limits the logins to one client address. Each
   container makes host keys of its own when it starts, and the penalties
   OpenSSH puts on an address that opens connections without logging in
   (`PerSourcePenalties`) are off: every connection to a Host comes from the
   same address here, and the ones tunnel-manager drops at a host key nobody
   has approved yet would shut that address out.
2. Makes three Docker networks in the ranges set aside for documentation, so
   that the addresses on the screens stand for nobody's network:

   | Network | Subnet | Reached from the Tunnel Manager server |
   |---------|--------|---------------------------|
   | `tunnel-manager-demo-site-a` | `198.51.100.0/24` | Yes, as the network the Tunnel Manager server and the clients are on |
   | `tunnel-manager-demo-site-b` | `203.0.113.0/24` | No: internal, with no address or route of the Tunnel Manager server on it |
   | `tunnel-manager-demo-site-b-lan` | `2001:db8:b::/64`, IPv6 alone | No, the same way |

3. Starts six containers of the image on them, named `tunnel-manager-demo-`
   and the name of the Host, with no port published:

   | Host | Addresses | Jump route |
   |------|-----------|------------|
   | `bastion` | `198.51.100.10` on site A, `203.0.113.10` on site B | None |
   | `site-a-app` | `198.51.100.21`, SSH from the bastion alone | `bastion` |
   | `site-a-db` | `198.51.100.22`, SSH from the bastion alone | `bastion` |
   | `site-b-gw` | `203.0.113.20` on site B, `2001:db8:b::20` on its LAN | `bastion` |
   | `site-b-app` | `2001:db8:b::21` | `bastion`, `site-b-gw` |
   | `site-b-db` | `2001:db8:b::22`, its web server on every address | `bastion`, `site-b-gw` |

4. Starts the demo service (`service/`) on `127.0.0.1:8000`, `127.0.0.1:8001`
   and `127.0.0.1:8002` at once. Each answers with a page that names its port.
5. Builds tunnel-manager from this repository and starts it with a data
   directory of its own under a new temporary directory. Nothing of an
   installation that is already on the machine is read or changed.
6. Drives the UI in headless Chrome (`recorder/`): the first sign in and the
   setup of the account `admin`; three service ports that the Hosts open on
   `8080`, `8081` and `8082` (the first at the pace of the rest, the other two
   the same way but quickly); the bastion, added without the service ports;
   `site-a-app` behind it, with its jump route set in the panel of the add
   form, and the other four the same way but quickly, `site-b-gw` also without
   the service ports; the host keys approved as they come in, a hop at a time,
   the first from its row and the others ticked together, and the tunnels
   coming up; the jump route column of the Hosts list reading none, one hop and
   two hops; the three services opened through the ports `site-a-app` opened
   for them on `198.51.100.21`, `8080`, `8081` and `8082` in turn (the first at
   the pace of the rest, the other two quickly), each page naming the port of
   the service it came from; a local forward on `127.0.0.1:18080` that reaches
   the web server inside `site-b-app`, two hops away (the recording does not
   wait there for its status to say connected; it opens the port as soon as the
   port answers); the status screen with the tunnels and the forward in one
   table; `site-b-gw` switched off, the route of the Hosts behind it marked
   with `!` in the list and the status screen saying which hop is off, and
   `site-b-gw` switched on again; and the SOCKS5 proxy of `site-b-app` on
   `127.0.0.1:1080`, which a second Chrome opens the web server of `site-b-db`
   through, at `http://[2001:db8:b::22]/`. That Chrome sends this address alone
   through the proxy, with a PAC script: the names Chrome looks up for itself
   would be looked up by `site-b-app`, which has no name server to ask, and its
   SSH server answers nothing else, the keepalive of the proxy included, while
   it waits.
7. Turns the frames into a GIF 960 pixels wide with ffmpeg, with fewer colors,
   and last without dithering, when it comes out larger than 5 MB.

It needs Go, Docker, ffmpeg and Google Chrome, and these addresses free:
`127.0.0.1:8888` (tunnel-manager), `127.0.0.1:8000`, `127.0.0.1:8001`,
`127.0.0.1:8002`, `127.0.0.1:18080` and `127.0.0.1:1080`. The script says which
one is taken if one is, and stops as well when a container or a network of the
names above is already there, or when this machine has a route inside one of
the three subnets.

| Variable | What it changes |
|----------|-----------------|
| `TM_SRC` | The tree tunnel-manager is built from. The repository this script is in when it is not set |
| `CHROME` | The Chrome executable. `/usr/bin/google-chrome` when it is not set |
| `DEMO_WORK_PARENT` | Where the work directory is made. `$TMPDIR`, or `/tmp`, when it is not set |

The recorder takes `-pace`, how many times as long every frame of the GIF is
held as the recording asks for. It is `1.15` when it is not given, which is
what `run.sh` uses; a larger one makes the whole GIF play slower, a smaller one
quicker.

The work directory holds the frames, the logs and the data directory of the
demo installation. It is left in place and its path is the last line the script
prints; remove it when you are done with it.

The passwords in these files are for the demo and open nothing else: the
containers, the networks and the installation they belong to exist only while
the script runs.
