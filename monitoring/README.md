# monitoring/ — мониторный стек lb-dsr-lab

Prometheus + Grafana на хосте (контейнеры `lab-prometheus` и `lab-grafana`,
сеть `host`). Экспортеры на узлах стенда развёрнуты отдельно (`exporters/`).

## Запуск / остановка

```bash
./run.sh                                   # запуск/перезапуск (идемпотентно)
docker rm -f lab-prometheus lab-grafana    # остановка
```

## Порты

- **9090** — Prometheus (`http://127.0.0.1:9090/targets` — состояние таргетов)
- **3000** — Grafana, анонимный Viewer + admin/admin
  (если 3000 на хосте занят: `GRAFANA_PORT=3300 ./run.sh`)

## Версии образов (запинены, можно переопределить env)

- `prom/prometheus:v3.15.1` (`PROM_IMAGE`)
- `grafana/grafana:11.6.7` (`GRAFANA_IMAGE`)

Если Docker Hub недоступен, `run.sh` автоматически пробует `mirror.gcr.io/<image>`.

## Файлы

- `prometheus.yml` — 10 таргетов (lb1/lb2: pfkeeper 9100, node 9101, pf 9102;
  rs1/rs2: node 9100, nginx-vts 9913), `cluster=lb-dsr-lab`, `role=lb|backend`
- `grafana/provisioning/` — datasource (Prometheus) и провайдер дашбордов
- `grafana/dashboards/` — 4 дашборда: `FreeBSD nodes`, `pf balancers`,
  `pfkeeper`, `nginx backends (VTS)`
- `run.sh` — запуск

Куда смотреть: `http://127.0.0.1:3000` (папка `lb-dsr-lab`), детали —
`docs/observability.md`.
