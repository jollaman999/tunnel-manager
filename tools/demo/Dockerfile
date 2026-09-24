# The Host of the demo: an OpenSSH server that lets a client forward ports and
# bind them on every interface, with a small web server inside that only the
# Host itself can reach.
FROM alpine:3.21.0

RUN apk add --no-cache openssh busybox-extras \
 && ssh-keygen -A \
 && adduser -D -s /bin/sh demo \
 && echo 'demo:demo-host-password' | chpasswd \
 && mkdir -p /srv/www \
 && printf '%s\n' \
      'PasswordAuthentication yes' \
      'KbdInteractiveAuthentication no' \
      'PermitRootLogin no' \
      'AllowTcpForwarding yes' \
      'GatewayPorts yes' \
      > /etc/ssh/sshd_config.d/demo.conf

COPY host-page.html /srv/www/index.html

EXPOSE 22 8080

CMD ["sh", "-c", "busybox-extras httpd -p 127.0.0.1:80 -h /srv/www && exec /usr/sbin/sshd -D -e"]
