# Grafana under /grafana/ — Include this INSIDE the existing *:443 <VirtualHost>
# (the one that serves Nagios), e.g. in /etc/apache2/sites-available/default-ssl.conf.
# Requires: a2enmod proxy proxy_http proxy_wstunnel headers

ProxyPreserveHost On
RequestHeader set X-Forwarded-Proto "https"

RedirectMatch 301 ^/grafana$ /grafana/
# upgrade=websocket carries Grafana Live (/grafana/api/live/ws) over the same mapping
ProxyPass        /grafana/ http://127.0.0.1:3000/grafana/ upgrade=websocket
ProxyPassReverse /grafana/ http://127.0.0.1:3000/grafana/
