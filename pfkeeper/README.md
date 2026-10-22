# pfkeeper — control plane для pf-балансировщиков (FreeBSD)

Демон на каждом узле CARP-пары (lb1/lb2) управляет pf-таблицами, якорными правилами
и health-чеками бэкендов. Желаемое состояние — в git (GitLab), применяемое — через
`pfctl`. API — gRPC поверх mTLS. Метрики — Prometheus. Родительский проект: [lb-dsr-lab](../README.md).

## Как это работает

1. **Состояние в git**: `tables/*.yaml` (члены + health-спека), `rules/*.conf` (якоря),
   `conf/pf.conf` (основной конфиг, применяется только по явной команде).
2. **Роли по CARP**: MASTER — единственный писатель (тянет git, применяет, пушит
   эффективный снапшот пиру). BACKUP применяет снапшоты и свои health-решения,
   в git не ходит (кроме degraded-режима).
3. **Health-чеки** (ядро): http (код + gojq по телу)/tcp/icmp, rise/fall,
   независимые пробы с обоих узлов, консенсус `and|or|local`. Фактическая таблица
   узла = desired ∩ healthy. Бэкенд, чей payload не проходит jq, удаляется из
   таблицы даже при HTTP 200.
4. **Peer-синхронизация**: PushConfig/PublishHealth/SyncCheck — divergence guard
   (хеши ревизии и эффективных таблиц), degraded-режимы без флапа.
5. **Безопасность**: TLS 1.3 + mTLS (свои CA и серты: admin/operator/viewer/node),
   RBAC по CN в interceptor, validate-before-apply (`pfctl -nf`), аудит
   (git commit + structured log), single-writer.

## Быстрый старт

```bash
make certs          # лабораторный PKI (certs/, в git не попадает)
make build-freebsd  # bin/freebsd/{pfkeeper,pfk}
bash deploy/deploy.sh   # сборка + установка на lb1/lb2 (root SSH)
```

Конфиги узлов: `deploy/configs/lb{1,2}.yaml`. Управление:

```bash
pfk -addr 192.168.100.11:8443 -cert admin.crt -key admin.key -ca ca.crt \
    tables set webs 192.168.100.21 192.168.100.22 -r "resize"
pfk ... health ls | inspect webs 192.168.100.21 | probe
pfk ... status pf|carp|peer   /   pfk ... admin sync
```

## Метрики и логи

`/metrics` (Prometheus): pf-счётчики, состояние членов, длительность/ошибки apply,
lag и diverged пира, роль CARP, системные. Логи — slog JSON в
`/var/log/pfkeeper.log` (newsyslog), audit-события `event=audit`.

## Известные ограничения v1 (security-ревью 2026-10-22, reports/)

- RedirectWrite доверяет actor_cn от пира (P2) — маркировать redirected-записи.
- PushConfig принимается MASTER-ом без валидации снапшота (P2) — валидировать.
- gojq-выражение может заблокировать probe-горутину (P2) — watchdog.
- http-prober следует редиректам (P2) — отключить.
- Node-серты 365 дней — сократить срок при выходе за лабу.
