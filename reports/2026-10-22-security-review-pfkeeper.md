# Security review: pfkeeper v1 (2026-10-22)

Область: весь код pfkeeper/ (демон, gRPC mTLS, git-стейт, prober'ы, rc.d/deploy).
Вердикт: **deployable as a lab**. Доверенная граница — валидный клиентский серт;
вне её удалённой эксплуатации не найдено.

## P2 (исправить до выхода за пределы лабы)

| # | Место | Проблема | Фикс |
|---|---|---|---|
| 1 | server/peering.go RedirectWrite | actor_cn от пира принимается вербатимом → подделка записей аудита/автора коммитов | логировать реальный CN пира рядом с actor_cn; помечать redirected |
| 2 | peer/controller + server/peering PushConfig | MASTER принимает снапшот от любого node-серта без ValidateState | отвергать PushConfig на MASTER; валидировать tables/anchors снапшота |
| 3 | health/probe.go (gojq) | jq-выражение оператора может заблокировать probe-горутину (repeat/range) — останавливается consensus | ограничить итерации; watchdog с deadline |
| 4 | health/probe.go http | http.Client следует до 10 редиректов — member может перенаправить пробу на произвольный хост (включая /metrics) | CheckRedirect: ErrUseLastResponse |

## P3

- ClientCAs читается один раз при старте (ротация CA требует рестарта) — перезагружать в CertReloader.
- Слушатели на 0.0.0.0: ограничить 8444 пиром, 9100 — mgmt (конфиг + pf).
- tls-блок не обязателен в validate() — при пустом TLS peer-клиент уходит в insecure: сделать обязательным.
- Нет сериализации read-modify-write стора между операторами — mutex вокруг mutateTable.
- actor в git Signature без санитизации (newline/<> из CN) — чистить control-символы.
- jqCache (sync.Map) растёт без границ — LRU.
- tcp без port>0 проходит валидацию — требовать.
- ping -W на FreeBSD — миллисекунды: масштабировать (иначе массовые ложные DOWN).
- Node-серты 365 дней + полный контроль пары: сократить срок, pf-ограничить 8444 адресом пира.

## Что ревью подтвердило

- Матрица RBAC корректна: node — только PeeringService; viewer — только read;
  RedirectWrite не даёт Rollback/ApplyMainConf; эскалации ролей не найдено.
- Argv-гигиена: строгий поэлементный exec, без shell; имена таблиц/адреса валидируются
  (запрет ведущих `-`, traversal); SSRF проб ограничен зарегистрированными мониторами.
- Секреты: ключи 0600; deploy key не попадает в argv/env (in-process SSH); known_hosts
  верификация у go-git включена (fail-closed); аудит — JSON (newline-инъекции нет).
