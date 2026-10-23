# Отчёт о тестировании pfkeeper v1

Дата: 2026-10-22…2026-10-23. Объект: `pfkeeper/` (Go 1.24, control plane для pf на FreeBSD 14.5).
Стенд испытаний: CARP-пара lb1/lb2 на FreeBSD 14.5 в libvirt/QEMU, бэкенды Debian 13 + nginx, клиент Debian 13 (см. полный отчёт стенда: `reports/2026-10-22-full-report-phases-1-2.md`).

---

## 1. Уровни тестирования

| Уровень | Объём | Инструменты |
|---|---|---|
| Unit | 57 тестов в 8 пакетах (pfctl-парсеры, git store, health prober/машина состояний/консенсус, reconciler, peer controller, сервер, authz, метрики, watchers) | go test, -race (8/8 пакетов чисто), httptest, bufconn, фейковый pfctl-Runner |
| Интеграционные (живой стенд) | t08–t12 | gRPC-клиент pfk, tcpdump, pfctl на обоих узлах, systemctl nginx, ab |
| Security-ревью | весь код pfkeeper/ | ручной аудит по чеклисту mTLS/RBAC/argv/секреты |

## 2. Интеграционные испытания (t08–t12)

### t08 — health-driven failover бэкенда — PASS

Методика: остановка nginx на rs1; prober'ы обоих узлов (interval 5s, fall 3) переводят member в DOWN; контроль содержимого `pfctl -t webs -T show` на lb1 и lb2; 5 curl на VIP; восстановление nginx (rise 2).

- после fall: таблица lb1 = {192.168.100.22}, таблица lb2 = {192.168.100.22} — rs1 удалён из таблиц **обоих** узлов без ручного pfctl;
- в период отказа: 5/5 запросов к VIP успешны;
- после восстановления: rs1 возвращён в таблицы обоих узлов; `/healthz` — ok.

### t09 — обнаружение и устранение дрейфа — PASS

Методика: ручной `pfctl -t webs -T add 192.168.100.99` на lb2; контроль `RuleService.Diff`; `AdminService.Sync`; контроль таблицы.

- diff: `webs diverged: missing=[] extra=[192.168.100.99], diverged=true` (детект ≤2 c) — либо автосходимость пушем снапшота (≤30 с);
- после Sync: таблица = {192.168.100.21, 192.168.100.22}, .99 отсутствует.

### t10 — mTLS и RBAC — PASS

| Сценарий | Ожидание | Результат |
|---|---|---|
| запрос без сертификата | отказ (tls handshake) | отказ |
| viewer: `tables set` | PermissionDenied | отказ |
| viewer: `tables ls` | разрешено | ок |
| operator: `tables ls` | разрешено | ок |
| node-серт на клиентских RPC | отказ (role node — только PeeringService) | отказ |

### t11 — глубина L7-проверки (jq по телу) — PASS

Методика: в state `tables/webs.yaml` задано `expect.status: [2xx]` + `expect.body.jq: '.status == "ok"'`; тело `/healthz` rs1 заменено на `{"status":"broken"}` (HTTP 200 сохраняется).

- rs1 → DOWN с reason=`jq_false` на обоих узлах (код 200 не спасает);
- `health inspect webs 192.168.100.21`: `status_code: 200`, `body_excerpt: {"status":"broken"}`, reason `jq_false` — видно, **что именно ответил** сервис;
- восстановление payload → rs1 UP.

### t12 — peer-синхронизация пары — PASS

Методика: мутация через BACKUP (lb2) — запрос проксируется на MASTER, коммитится в git (actor из CN клиента), применяется на MASTER и пушится снапшотом обратно.

- таблицы lb1 и lb2 идентичны после записи через lb2;
- `status peer`: `reachable=true, peer_revision=<HEAD>, lag=0s, diverged=false`.

## 3. Испытания сервиса в составе стенда (сквозные)

| Сценарий | Результат |
|---|---|
| Отказ lb1 (t07, `shutdown -p now`) под curl-циклом 120 запросов | 119 ok / 1 fail (простой ≈2 с), VIP перехвачен lb2 (CARP), pfsync-репликация стейтов подтверждена кадрами PFSYNCv5 |
| Здоровые члены под jq-контролем | оба UP, code=200; проверка тела выполняется каждым узлом независимо |
| Peer-канал | lag=0s, diverged=false, репликация health-взглядов ≤1 тика |
| Нагрузка через DSR (ab 3000 req, c=10) | 15 212 rps, 0 failed |

## 4. Unit-покрытие по пакетам

| Пакет | Что покрыто |
|---|---|
| internal/pfctl | Runner (строгий argv), парсеры `-si`/`-T show -v`/`-sr` на реальных фикстурах, коды выхода (`-T test`), argv-гигиена (запрет `-`, traversal) |
| internal/state | схема YAML (обе формы expect: плоский `jq` и вложенный `body.jq`), валидация (имена, IPv4, длительности, gojq.Parse, дубликаты), git-цикл Ensure→Pull→CommitAndPush→Pull через bare-origin |
| internal/health | MatchStatus (коды/диапазоны/2xx), HTTP prober (200/500/timeout/json_invalid/jq_false/jq_error), машина состояний rise/fall, консенсус Combine local/and/or |
| internal/reconcile | apply только при диффе, якоря, ApplyMainConf+dryRun, health-aware дрифт (drift vs desired∩healthy), сериализация мутаций mutex |
| internal/peer | роли по CARP, пуш снапшота при смене ревизии, divergence guard + heal, degraded-режим (троттлинг WARN), RedirectWrite proxy |
| internal/server | mtls reload, authz-матрица ролей, audit-события, RedirectWrite whitelist, все сервисы |
| internal/metrics, watchers | сбор pf-счётчиков, per-table члены, watchers ticker+ctx, health-aware gitWatcher |

## 5. Дефекты, найденные тестированием и устранённые

| № | Симптом | Корень | Фикс |
|---|---|---|---|
| 1 | Обе таблицы опустошались при отказе одного бэкенда | key-mismatch `table/addr` vs `addr` на границе watcher→ApplyEffective | нарезка per-table взглядов (`tableView`), регресс-тест end-to-end |
| 2 | BACKUP возвращал health-удалённого члена | периодический снапшот = desired, health-слой применялся только по переходам | health-пересчёт после каждого снапшота + при смене peer-view |
| 3 | jq-ожидание не работало на живых прых | вложенный `expect.body.jq` терялся YAML-схемой (знала только плоский `jq`) | custom UnmarshalYAML (обе формы), canonical-маршал |
| 4 | diverged=true «залипал» на BACKUP | SyncCheck сравнивал хеш локального git (который BACKUP не тянет) | SyncCheck отправляет applied-snapshot identity; MASTER делает heal |
| 5 | Дубли инстансов демона на узле | daemon(8) `-r` респавнил ребёнка; pidfile перезаписывался | bind слушателей до reconcile (крашлуп не трогает pf), rc.d stop = supervisor+child |
| 6 | Ложный дрифт на MASTER возвращал unhealthy-члена | config-reconciler не знал о health | drift относительно desired∩healthy (HealthyView) |

## 6. Security-ревью (выжимка)

Вердикт: deployable as a lab. Эскалаций ролей и инъекций в argv не выявлено. К устранению до продакшена (P2): доверие actor_cn от пира в RedirectWrite; PushConfig без ValidateState на MASTER; gojq без watchdog; http-prober следует редиректам. Полный список (4×P2, 8×P3): `reports/2026-10-22-security-review-pfkeeper.md`.

## 7. Ограничения испытаний

- Отказ lb1 тестировался штатным `shutdown -p now` (не выдёргиванием питания); pfsync-репликация sloppy-стейтов DSR-потоков проверена на реплицированные записи, но не на живое переключение в момент активной передачи.
- ICMP-prober использует `ping(8)` (ограничение v1: `-W` в миллисекундах на FreeBSD).
- Нагрузочный прогон выполнен на ВМ; для целевого железа цифры rps будут иными (сеть/лимиты pf).
