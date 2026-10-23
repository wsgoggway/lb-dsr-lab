# Мануал: развёртывание DSR + CARP + pfkeeper на реальном железе

Перенос стенда lb-dsr-lab с libvirt/QEMU на физические серверы. Всё, что в лабе
зависело от виртуализации (vtnet, libvirt-сети, снапшоты), заменяется на физические
интерфейсы, коммутацию и обычную установку ОС. Синтаксис pf, параметры CARP/pfsync,
конфигурация pfkeeper — идентичны.

---

## 1. Топология и требования

```
                 internet / корпоративная сеть
                          |
                      роутер/fw  (клиенты приходят в VLAN 100)
                          |
        +--------- VLAN 100 (DSR-сегмент, 192.168.100.0/24) ---------+
        |                 |                  |            |
      lb1 em0           lb2 em0           rs1 eth0      rs2 eth0
   .11 + CARP VIP     .12 + CARP VIP      .21 + VIP/32 lo   .22 + VIP/32 lo
   (VIP .200 отвечает только lb-паре)
        | em1 (VLAN 200 - sync/mgmt, 10.0.100.0/30)
        +------- lb1 <--- pfsync + pfkeeper peer ---> lb2
```

| Позиция | Требования |
|---|---|
| lb1, lb2 | x86-64, 2 NIC (Intel igc/ix/em рекомендуется), 2 ГБ RAM, disk ≥ 20 ГБ, FreeBSD 14.x |
| rs1, rs2 … | любой Linux, 1 NIC, nginx; **обязательно в том же L2/VLAN, что и em0 балансировщиков** (условие DSR через маршрут) |
| Сеть | VLAN 100 — клиентский/DSR-сегмент; VLAN 200 — изолированный sync/mgmt (pfsync ходит открытым текстом); шлюз по умолчанию для бэкендов — не балансировщик (обратный путь мимо LB — суть DSR) |

Адресация (пример из стенда, замените на свою):

| Узел | VLAN 100 | VLAN 200 (sync) |
|---|---|---|
| lb1 | 192.168.100.11/24 + CARP VIP 192.168.100.200/32 (vhid 1, advskew 0) | 10.0.100.1/30 |
| lb2 | 192.168.100.12/24 + тот же CARP VIP (advskew 100) | 10.0.100.2/30 |
| rs1 / rs2 | 192.168.100.21/24, .22/24 + VIP/32 на lo | — |
| client | 192.168.100.10/24 | — |

## 2. Отличия от лабы (чеклист переноса)

| В лабе | На железе |
|---|---|
| драйвер vtnet | фактический: `em`/`igc`/`ix` — заменить во всех pf.conf и rc.conf |
| libvirt-сети | реальные VLAN на коммутаторах |
| CARP-мультикаст внутри virbr | коммутаторы: проверить, что multicast 224.0.0.18 (протокол 112, CARP) не режется igmp-snooping'ом; на некоторых свитчах требуется IGMP querier в VLAN или отключение snooping |
| Сертификаты от лабораторного CA | внутренний/боевой CA, короткоживущие серты, ротация |
| git-origin: bare на client VM | GitLab-проект + deploy key с RW |
| снапшоты/клоны дисков | бэкапы обычными средствами; восстановления — повторный деплой |
| nuageinit (облачная инициализация) | обычная установка/Ansible — симптомы «флейма первого бута» исчезают |
| pfsync внутри изолированного virbr-sync | отдельный физический линк или выделенный VLAN; трафик pfsync **не шифруется** |

## 3. Установка FreeBSD (lb1, lb2 — одинаково, кроме hostname/IP)

1. Установите FreeBSD 14.x (базовая система, без портов; исходники не нужны).
2. Корневая ФС — UFS или ZFS (на выбор; pf и CARP безразличны).
3. Проверьте после загрузки: `pciconf -l | grep -i ether` — имена NIC
   (примеры далее: `em0` — VLAN 100, `em1` — sync).

## 4. /etc/rc.conf (lb1; для lb2 см. отличия в комментариях)

```sh
hostname="lb1"
ifconfig_em0="inet 192.168.100.11/24"
ifconfig_em0_alias0="vhid 1 advskew 0 pass lbdsr 192.168.100.200/32"
ifconfig_em1="inet 10.0.100.1/30"
defaultrouter="192.168.100.1"
gateway_enable="YES"
sshd_enable="YES"
# pf
pf_enable="YES"
pf_rules="/etc/pf.conf"
# CARP + pfsync - модули ядра, не в GENERIC
kld_list="carp pfsync"
cloned_interfaces="pfsync"
ifconfig_pfsync0="syncdev em1 syncpeer 10.0.100.2 up"
# CARP preemption: узел с advskew 0 возвращает MASTER после восстановления
pfkeeper_enable="YES"
```

lb2 отличия: `hostname="lb2"`, `ifconfig_em0="inet 192.168.100.12/24"`,
alias — `advskew 100`, `ifconfig_em1="inet 10.0.100.2/30"`,
`ifconfig_pfsync0="syncdev em1 syncpeer 10.0.100.1 up"`.

`/etc/sysctl.conf` (оба узла):

```sh
net.inet.carp.preempt=1
net.inet.ip.forwarding=1
```

`/etc/pf.conf` — взять `pfkeeper/../freebsd/pf-dsr.conf` из репозитория и заменить
`vtnet0` на `em0` (3 вхождения: правило DSR, carp/pfsync-пассы не привязаны к
интерфейсу, management-пассы). Порты 8443/8444/9100 на em1 (sync/mgmt) и 8443/9100
на em0 (mgmt из клиентской подсети) — уже в файле.

## 5. Linux-бэкенды (rs1, rs2)

1. nginx: конфиг из `linux/nginx-vip.conf` — `listen 80` (wildcard),
   `add_header X-Served-By $hostname`, `/healthz` из файла `/var/www/healthz`
   (JSON `{"status":"ok","host":"rs1"}`).
2. Файл маркера: `echo '<html>...</html>' > /srv/www/index.html` и
   `echo '{"status":"ok","host":"rs1"}' > /var/www/healthz`.
3. sysctl (обязательно, иначе ARP-война за VIP):

```sh
net.ipv4.conf.all.arp_ignore = 1
net.ipv4.conf.lo.arp_ignore = 1
net.ipv4.conf.all.arp_announce = 2
net.ipv4.conf.lo.arp_announce = 2
net.ipv4.conf.all.rp_filter = 0
```

4. VIP на loopback (постоянно, например через /etc/network/interfaces или nm):

```sh
ip addr add 192.168.100.200/32 dev lo
```

5. Шлюз по умолчанию — маршрутизатор VLAN 100, **не** балансировщик.

## 6. Проверка без pfkeeper (голый DSR + CARP)

На lb1 (затем lb2):

```sh
kldload carp pfsync
ifconfig em0 alias vhid 1 advskew 0 pass lbdsr 192.168.100.200/32   # lb2: advskew 100
ifconfig pfsync0 create
ifconfig pfsync0 syncdev em1 syncpeer 10.0.100.2 up    # lb2: syncpeer 10.0.100.1
sysctl net.inet.carp.preempt=1
pfctl -f /etc/pf.conf
```

Контрольные точки:

1. `ifconfig em0 | grep carp:` → lb1 `MASTER`, lb2 `BACKUP`.
2. С клиента: `arping 192.168.100.200` → отвечает **00:00:5e:00:01:01** (CARP VMAC).
3. `curl http://192.168.100.200/healthz` → JSON от rs1/rs2; повторные запросы
   ходят round-robin (один источник — один бэкенд, sticky).
4. Останов nginx на одном бэкенде → уберите его адрес из таблицы
   (`pfctl -t webs -T delete 192.168.100.21`) → трафик идёт на второй.

## 7. pfkeeper

### 7.1 Сертификаты

На административной машине (не на узлах):

```bash
cd pfkeeper && make certs        # лабораторный CA; для прода - ваш CA
```

Для прода: подписьте узловые и клиентские серты внутренним CA (CN=lb1/lb2,
URI `pfkeeper://role/node` + IP SAN адресов mgmt-интерфейса; клиентские —
`pfkeeper://role/admin|operator|viewer`). Рекомендуемый срок жизни — 30–90 дней.

### 7.2 Git-репозиторий состояния

Создайте проект в GitLab (например `infra/pfkeeper-state`), добавьте
**deploy key с write access** (один на пару), секретный ключ положите на узлы в
`/var/db/pfkeeper/deploy.key` (0600, root:root), публичный — в authorized_keys
учётной записи на хосте origin.

### 7.3 Конфиг узла

`/usr/local/etc/pfkeeper/pfkeeper.yaml` (пример lb1):

```yaml
node: lb1
carp_interface: em0
state:
  dir: /var/db/pfkeeper/state
  remote: ssh://git@gitlab.example.com/infra/pfkeeper-state.git
  branch: main
  key: /var/db/pfkeeper/deploy.key
sync:
  mode: and          # member жив, только если здоров на обоих узлах
  auto_heal: true
listen:
  client: 10.0.100.1:8443
  peer: 10.0.100.1:8444
  metrics: 10.0.100.1:9100
tls:
  cert: /usr/local/etc/pfkeeper/lb1.crt
  key: /usr/local/etc/pfkeeper/lb1.key
  ca: /usr/local/etc/pfkeeper/ca.crt
peer:
  endpoint: 10.0.100.2:8444
```

lb2 — зеркально (адрес, серт, `endpoint: 10.0.100.1:8444`). Начальное
содержимое state — `config.yaml` + `tables/webs.yaml` из репозитория
(`pfkeeper/../state`-примеры см. в GitLab-проекте после первого push).

### 7.4 Установка сервиса

На административной машине (параметры окружением — реальные адреса/ключ):

```bash
PFK_NODES="10.0.100.1 10.0.100.2" PFK_KEY=~/.ssh/prod_key \
    bash pfkeeper/deploy/deploy.sh
```

Скрипт собирает статические бинарники (GOOS=freebsd, без cgo), останавливает
сервис, заменяет бинарь и rc.d, запускает. Конфиги/серты/ключ — раскладываются
отдельно (выше). Файл `/etc/pf.conf` на узлах pfkeeper **не трогает** — только
таблицы и якоря; замена основного конфига — `pfk rules apply-main`.

Клон стейта появится сам при первом старте: `/var/db/pfkeeper/state`.

### 7.5 Проверка

```bash
pfk -addr 10.0.100.1:8443 -cert admin.crt -key admin.key -ca ca.crt status carp
pfk ... status peer      # reachable=true, diverged=false
pfk ... tables ls        # webs, члены
pfk ... health ls        # UP по обоим узлам
# отказоустойчивость узла:
lb1# shutdown -p now     # под curl-циклом на VIP: потери ≤ пара запросов
```

## 8. Эксплуатация

- Изменение состава бэкендов: правка `tables/*.yaml` в GitLab (MR) → `pfk admin sync`
  на любом узле (или авто — pull-цикл MASTER'а) → применение на паре.
- Откат: `pfk admin rollback <commit>`.
- Бэнд-эксплуатация узла: `service pfkeeper stop` — узел прекращает health-пробы
  и согласование; CARP-роль остаётся за парой.
- Ротация сертов: `make certs` (или ваш CA) → разложить → сервис перечитает
  сертификаты без рестарта (CertReloader); CA — дополнительно перезапуск.
- Обновление pfkeeper: повторный `deploy/deploy.sh`.

## 9. Типовые проблемы

| Симптом | Причина | Действие |
|---|---|---|
| Оба узла MASTER | коммутатор режет мультикаст CARP | отключить igmp-snooping в VLAN или включить querier; проверить ACL на протокол 112 |
| VIP доступен только с сегмента lb | нет обратного пути для бэкендов / DSR-ответы блокируются fw по пути клиента | DSR-ответы src=VIP из VLAN 100 — маршрутизатор не должен их блокировать; uRPF на пути — strict off для этого сегмента |
| `pfkeeper` перезапускается циклически | второй инстанс держит порт; осиротевший daemon(8) | `killall pfkeeper daemon; rm -f /var/run/pfkeeper*.pid` → `service pfkeeper start` |
| health DOWN у живого бэкенда | jq-ожидание не совпало с телом | `pfk health inspect <table> <addr>` — показывает тело и вывод jq |
| git pull/push reject | разошлись узлы | MASTER `pfk admin sync`; при необходимости rollback на известный коммит |

## 10. Безопасность продакшена (см. reports/2026-10-22-security-review-pfkeeper.md)

- Закрыть P2-находки ревью (актуальный список — в отчёте): доверие actor_cn,
  валидация PushConfig, watchdog gojq, запрет редиректов в prober.
- Слушатели 8443/9100 — только mgmt-адрес или pf-ограничение по адресам
  администраторов; 8444 — только адрес пира.
- Срок жизни node-сортов — 30–90 дней; клиентские серты — по ролям.
- Deploy key — минимальный срок/ротация;审计-логи — в централизованное хранилище.
