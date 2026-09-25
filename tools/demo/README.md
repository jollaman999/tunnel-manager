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
   it answers on `127.0.0.1:80` of the container only.
2. Starts a container of it with `127.0.0.2:2222` published to its SSH port and
   `127.0.0.2:8080`, `127.0.0.2:8081` and `127.0.0.2:8082` to the ports the
   Host opens for the service ports, so that tunnel-manager can check from here
   that each of them answers.
3. Starts the demo service (`service/`) on `127.0.0.1:8000`, `127.0.0.1:8001`
   and `127.0.0.1:8002` at once. Each answers with a page that names its port.
4. Builds tunnel-manager from this repository and starts it with a data
   directory of its own under a new temporary directory. Nothing of an
   installation that is already on the machine is read or changed.
5. Drives the UI in headless Chrome (`recorder/`): the first sign in and the
   setup of the account `admin`, three service ports that the Host opens on
   `8080`, `8081` and `8082` (the first at the pace of the rest, the other two
   the same way but quickly), the Host, the approval of its host key, the
   tunnels coming up, the first service opened through the port the Host opened
   for it, a local forward on `127.0.0.1:18080` that reaches the web server
   inside the Host (the recording does not wait there for its status to say
   connected; it opens the port as soon as the port answers), the status screen
   with the tunnels and the forward in one table, and the SOCKS5 proxy of the
   Host on `127.0.0.1:1080`, which a second Chrome opens that same web server
   through.
6. Turns the frames into a GIF 960 pixels wide with ffmpeg, with fewer colors
   when it comes out larger than 5 MB.

It needs Go, Docker, ffmpeg and Google Chrome, and these addresses free:
`127.0.0.1:8888` (tunnel-manager), `127.0.0.1:8000`, `127.0.0.1:8001`,
`127.0.0.1:8002`, `127.0.0.2:2222`, `127.0.0.2:8080`, `127.0.0.2:8081`,
`127.0.0.2:8082`, `127.0.0.1:18080` and `127.0.0.1:1080`. The script says which one is taken if one is.

| Variable | What it changes |
|----------|-----------------|
| `TM_SRC` | The tree tunnel-manager is built from. The repository this script is in when it is not set |
| `CHROME` | The Chrome executable. `/usr/bin/google-chrome` when it is not set |
| `DEMO_WORK_PARENT` | Where the work directory is made. `$TMPDIR`, or `/tmp`, when it is not set |

The work directory holds the frames, the logs and the data directory of the
demo installation. It is left in place and its path is the last line the script
prints; remove it when you are done with it.

The passwords in these files are for the demo and open nothing else: the
container and the installation they belong to exist only while the script runs.
