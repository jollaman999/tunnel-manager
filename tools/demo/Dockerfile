# The Hosts of the demo: an OpenSSH server that lets a client forward ports and
# bind them on every interface, with a small web server inside that only the
# Host itself can reach. WEB_ADDR is where that web server listens, and SSH_FROM,
# when it is set, is the one address the SSH server lets the user log in from.
FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

RUN apk add --no-cache openssh busybox-extras \
 && adduser -D -s /bin/sh demo \
 && echo 'demo:demo-host-password' | chpasswd \
 && mkdir -p /srv/www \
 && printf '%s\n' \
      'PasswordAuthentication yes' \
      'KbdInteractiveAuthentication no' \
      'PermitRootLogin no' \
      'AllowTcpForwarding yes' \
      'GatewayPorts yes' \
      'PerSourcePenalties no' \
      > /etc/ssh/sshd_config.d/demo.conf

COPY host-page.html /srv/www/index.html

ENV WEB_ADDR=127.0.0.1:80 SSH_FROM=

EXPOSE 22 8080

CMD ["sh", "-c", "ssh-keygen -A && sed -i \"s|@HOST@|$(hostname)|; s|@WEB_ADDR@|$WEB_ADDR|\" /srv/www/index.html && if [ -n \"$SSH_FROM\" ]; then echo \"AllowUsers demo@$SSH_FROM\" >> /etc/ssh/sshd_config.d/demo.conf; fi && busybox-extras httpd -p \"$WEB_ADDR\" -h /srv/www && exec /usr/sbin/sshd -D -e"]
