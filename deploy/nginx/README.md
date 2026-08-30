# Nginx: MediaMTX на одном HTTPS-порту

Один vhost проксирует:

| URL | Куда |
|---|---|
| `/v3/...` | Control API `127.0.0.1:9997` |
| всё остальное | Compat API `127.0.0.1:8877` (live HLS, DVR, preview) |

HTTP `:80` редиректит на HTTPS.

## Подключение

1. В `mediamtx-http.conf` замени `server_name` и пути к сертификатам.

2. Включи конфиг из `http { }` (так обычно устроен `nginx.conf`):

```nginx
include /path/to/mediamtx/deploy/nginx/mediamtx-http.conf;
```

Или симлинк:

```bash
ln -s /path/to/mediamtx/deploy/nginx/mediamtx-http.conf /etc/nginx/conf.d/mediamtx.conf
nginx -t && systemctl reload nginx
```

## MediaMTX

Бэкенды слушай только на localhost и доверяй nginx:

```yaml
api: true
apiAddress: 127.0.0.1:9997
apiTrustedProxies: [127.0.0.1, ::1]

compatAPI: true
compatAPIAddress: 127.0.0.1:8877
compatAPITrustedProxies: [127.0.0.1, ::1]
```

## Примеры

```
https://dvr.example.com/v3/paths/list
https://dvr.example.com/cam1/video.m3u8
https://dvr.example.com/cam1/info.json
```

## Control API

`/v3/` снаружи открыт. Чтобы закрыть, в `mediamtx-http.conf` раскомментируй `allow` / `deny` в `location ^~ /v3/`.
