// Package nginx — config templates (UPDATED for step 6).
//
// New: server blocks render cache locations, defense rules, redirects,
// and header operations from the DomainPolicy attached at generate time.
package nginx

const mainConfTemplate = `# MeshCDN-generated nginx.conf — DO NOT EDIT BY HAND
worker_processes  auto;
worker_rlimit_nofile  65535;

events {
    worker_connections  10240;
    use epoll;
    multi_accept on;
}

http {
    include       /usr/local/openresty/nginx/conf/mime.types;
    default_type  application/octet-stream;

    sendfile        on;
    tcp_nopush      on;
    tcp_nodelay     on;
    keepalive_timeout  65;
    client_max_body_size 100m;
    server_tokens off;

    log_format main '$remote_addr - $remote_user [$time_local] '
                    '"$request" $status $body_bytes_sent '
                    '"$http_referer" "$http_user_agent"';
    access_log  /etc/meshcdn/runtime/logs/access.log main;
    error_log   /etc/meshcdn/runtime/logs/error.log warn;

    ssl_session_cache shared:SSL:10m;
    ssl_session_timeout 10m;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers HIGH:!aNULL:!MD5;
    ssl_prefer_server_ciphers off;

    # Proxy cache zone (used by cache rules)
    proxy_cache_path /etc/meshcdn/runtime/cache levels=1:2 keys_zone=CDN:50m
                     max_size=1g inactive=24h use_temp_path=off;

    # Rate-limit zones (defined per-zone via a single shared declaration;
    # specific zones referenced from server blocks)
    limit_req_zone $binary_remote_addr zone=meshcdn_rl:10m rate=1000r/s;

    include {{ .OutputDir }}/servers/*.conf;
}
`

const serverConfTemplate = `# Generated for {{ .Server.Scope }}
server {
{{- if eq .Server.Proto "https" }}
{{- if eq .Server.Host "-" }}
    listen {{ .Server.Port }} default_server ssl;
    listen [::]:{{ .Server.Port }} default_server ssl;
{{- else }}
    listen {{ .Server.Port }} ssl;
    listen [::]:{{ .Server.Port }} ssl;
{{- end }}
{{- else }}
{{- if eq .Server.Host "-" }}
    listen {{ .Server.Port }} default_server;
    listen [::]:{{ .Server.Port }} default_server;
{{- else }}
    listen {{ .Server.Port }};
    listen [::]:{{ .Server.Port }};
{{- end }}
{{- end }}

{{- if eq .Server.Host "-" }}
    server_name _;
{{- else }}
    server_name {{ .Server.Host }};
{{- end }}

{{- if eq .Server.Proto "https" }}
{{- if .Server.CertCRT }}
    ssl_certificate     {{ .Server.CertCRT }};
    ssl_certificate_key {{ .Server.CertKey }};
{{- end }}
{{- end }}

    # ACME HTTP-01 challenges
    location ^~ /.well-known/acme-challenge/ {
        root {{ .ChallengeRoot }};
        try_files $uri =404;
    }

{{- /* Defense: deny/allow IP rules at server-level */ -}}
{{- range .Policy.Defenses.BlockIPs }}
    deny {{ . }};
{{- end }}
{{- range .Policy.Defenses.AllowIPs }}
    allow {{ . }};
{{- end }}

{{- /* Defense: rate limit + body size */ -}}
{{- if .Policy.Defenses.RateLimit }}
    limit_req zone=meshcdn_rl burst={{ .Policy.Defenses.RateLimit }} nodelay;
{{- end }}
{{- if .Policy.Defenses.BodySize }}
    client_max_body_size {{ .Policy.Defenses.BodySize }};
{{- end }}

{{- /* Redirects: emit "return <status> <to>" rules in precedence order */ -}}
{{- range .Policy.Redirects }}
{{- if .Regex }}
    location ~ {{ .From }} {
        return {{ .Status }} {{ .To }};
    }
{{- else }}
    location {{ .From }} {
        return {{ .Status }} {{ .To }};
    }
{{- end }}
{{- end }}

{{- /* Cache rules: emit nginx location blocks in precedence order */ -}}
{{- range .Policy.CacheRules }}
    location {{ if .NginxModifier }}{{ .NginxModifier }} {{ end }}{{ .NginxMatcher }} {
{{- if not $.OriginIsWelcome }}
        proxy_pass {{ $.OriginRaw }};
        proxy_http_version 1.1;
        proxy_set_header Host                $host;
        proxy_set_header X-Real-IP           $remote_addr;
        proxy_set_header X-Forwarded-For     $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto   $scheme;
{{- end }}
        proxy_cache CDN;
        proxy_cache_valid 200 {{ .TTL }}s;
{{- if .HSTS }}
        add_header Strict-Transport-Security "max-age=31536000" always;
{{- end }}
{{- if .BrowserTTL }}
        expires {{ .BrowserTTL }}s;
{{- end }}
{{- range $k, $v := $.Policy.HeaderOps.ResponseAdd }}
        add_header {{ $k }} "{{ $v }}" always;
{{- end }}
    }
{{- end }}

{{- /* Default location (catch-all) */ -}}
{{- if .OriginIsWelcome }}
    root {{ .WelcomeRoot }};
    index index.html;
    location / {
        try_files $uri $uri/ /index.html;
    }
{{- else }}
    location / {
        proxy_pass {{ .OriginRaw }};
        proxy_http_version 1.1;
        proxy_set_header Host                $host;
        proxy_set_header X-Real-IP           $remote_addr;
        proxy_set_header X-Forwarded-For     $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto   $scheme;
        proxy_set_header X-Forwarded-Host    $host;
        proxy_connect_timeout 30s;
        proxy_send_timeout    60s;
        proxy_read_timeout    60s;
        proxy_buffering on;
{{- if eq .Server.Proto "https" }}
        proxy_ssl_server_name on;
        proxy_ssl_protocols TLSv1.2 TLSv1.3;
{{- end }}
{{- /* Header ops at default location */ -}}
{{- range $k, $v := .Policy.HeaderOps.RequestAdd }}
        proxy_set_header {{ $k }} "{{ $v }}";
{{- end }}
{{- range $k, $v := .Policy.HeaderOps.ResponseAdd }}
        add_header {{ $k }} "{{ $v }}" always;
{{- end }}
{{- range .Policy.HeaderOps.ResponseDel }}
        proxy_hide_header {{ . }};
{{- end }}
    }
{{- end }}
}
`

const defaultConfTemplate = `# Default server for port {{ .Port }}
server {
{{- if eq .Proto "https" }}
    listen {{ .Port }} default_server ssl;
    listen [::]:{{ .Port }} default_server ssl;
{{- else }}
    listen {{ .Port }} default_server;
    listen [::]:{{ .Port }} default_server;
{{- end }}
    server_name _;

{{- if eq .Proto "https" }}
{{- if .CertCRT }}
    ssl_certificate     {{ .CertCRT }};
    ssl_certificate_key {{ .CertKey }};
{{- end }}
{{- end }}

    location ^~ /.well-known/acme-challenge/ {
        root {{ .ChallengeRoot }};
        try_files $uri =404;
    }

    root {{ .WelcomeRoot }};
    index index.html;
    location / {
        try_files $uri $uri/ /index.html;
    }
}
`
