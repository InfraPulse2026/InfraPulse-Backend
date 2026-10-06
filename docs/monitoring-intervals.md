# Интервалы сбора: 3 секунды — 24 часа

Backend сохраняет ожидаемые интервалы каждого устройства и оценивает свежесть
telemetry по этому расписанию. Добавлены миграции 002/003; миграция 001 и Agent
не изменены. OpenAPI обновлён до версии 0.2.0.

## Сбор, связь и экран — три разных интервала

- Agent измеряет CPU/RAM/interfaces с выбранным периодом.
- Heartbeat подтверждает связь, обычно каждые 10 секунд, даже при редком сборе.
  Backend `AGENT_TIMEOUT` остаётся отдельным параметром, по умолчанию 90 секунд.
- Frontend запрашивает API со своей частотой. Запрос каждые 3 секунды может
  возвращать тот же sample: новые значения backend не придумывает.

При сборе раз в 24 часа нельзя обещать минутную диагностику или точный прогноз
быстрой аварии. Частые метрики полезны для критичных устройств; inventory
обычно можно обновлять реже.

## Возможности текущего Agent

Конфигурация Agent допускает интервалы до 24h для telemetry и per-device
polling. Проверено неизменённым бинарником `validate-config`: 10h и 24h
принимаются, 24h1s отклоняется. Полный суточный runtime-цикл не проверялся.

Agent загружает YAML при запуске. В protocol v1 нет live update-config или
подтверждения применённого расписания. Поэтому изменения backend не выдаются
за автоматически применённые Agent настройки.

## API

```http
GET /api/devices/{id}/monitoring?agent_id=...
PUT /api/devices/{id}/monitoring?agent_id=...
```

PUT принимает полный объект с целыми значениями от 3 до 86400 включительно:

```json
{
  "telemetry_interval_seconds": 3,
  "inventory_interval_seconds": 3600,
  "topology_interval_seconds": 300
}
```

Неполные объекты, неизвестные поля, дробные значения и нарушение диапазона
возвращают 400 `invalid_policy`. Настройки принадлежат паре agent/device;
одинаковый device_id у другого collector не меняется.

Ответ содержит настройки, `origin` (`backend_default`/`device_override`),
`delivery_mode: manual_agent_yaml`, `application_status: unverified`,
`local_telemetry_yaml`, `remote_polling_yaml` и `recommended_topology_ttl_seconds`.
Настройки переживают перезапуск backend.

Чтобы реально изменить сбор, перенеси нужный YAML-фрагмент в конфигурацию
Agent, выполни validate-config и перезапусти Agent. После этого статус
`unverified` остаётся: текущий протокол не сообщает config ACK. HTTP 200
означает сохранение ожиданий backend. Frontend не должен писать «Agent применил».

Фрагмент `remote_polling_yaml` предназначен для блока polling существующего
элемента devices[]. Он не заменяет весь список устройств. medium probes,
logs и SIP имеют отдельные интервалы и этим API не меняются. В частности,
remote medium probes могут продолжаться раз в 30 секунд при редкой telemetry.

При редкой topology увеличь `remote.topology_ttl` Agent до рекомендуемого
значения: default 15m меньше периода 10h. TTL — предположение collector,
а не срок, объявленный самим LLDP neighbor. Параметр общий для этого Agent. Рекомендация ограничена 24h — максимумом Agent. При суточной topology этот TTL не покрывает два пропущенных цикла: записи соседей могут истечь до следующего успешного опроса. Для стабильной карты выбирай более частую topology независимо от периода CPU/RAM.

Политики относятся к локальному development API. Перед внешним размещением
нужны авторизация оператора и аудит изменений.

## Значения по умолчанию backend

| Переменная | Значение |
|---|---|
| `DEFAULT_TELEMETRY_INTERVAL` | `3s` |
| `DEFAULT_INVENTORY_INTERVAL` | `5m` |
| `DEFAULT_TOPOLOGY_INTERVAL` | `5m` |

Принимаются целые секунды 3s..24h: например, 30s, 15m, 10h, 24h. Эти параметры
действуют для устройств без override и тоже не меняют расписание Agent.

## Свежесть данных

Карточка устройства содержит `monitoring` и `telemetry_freshness`. Последняя
telemetry/metrics содержит `monitoring` и `freshness`. Исторические samples
не объявляются плохими просто из-за возраста.

Freshness возвращает status (`fresh`, `stale`, `no_data`, `clock_skew`),
expected_interval_seconds, stale_after_seconds, age_seconds, observed_at,
received_at и expected_next_at. Значения времени UTC; отсутствующие времена null.

Свежесть считается по observed_at последнего sample. Новый heartbeat или
inventory не маскируют старую telemetry. received_at виден отдельно: поздняя
доставка не превращает старое наблюдение в новое измерение. Наблюдение более
чем на 30 секунд в будущем даёт clock_skew. Старые даты Simulator могут дать
stale; для текущей временной шкалы задай актуальный --start.

Stale threshold равен `2 × interval + jitter`: jitter — 10% интервала,
ограниченные 5..30 секундами. Для 3s это 11s; для 10h — 20h30s; для 24h — 48h30s. Это явный
допуск двух периодов, не гарантия доставки. expected_next_at показывает,
когда ожидался очередной sample. online, agent_connected и freshness независимы.

## Проверено

Границы 3s/24h, ошибочные запросы, future timestamps, отсутствие данных;
5h-old sample stale при 3s и fresh при 10h; сохранение overrides в PostgreSQL;
изоляция collectors; heartbeat не обновляет возраст telemetry; повторные
миграции сохраняют настройки; API возвращает manual/unverified.

## Будущий live control

Для смены расписания из интерфейса без перезапуска потребуется согласованное
расширение Agent: typed config update, revision, validation, atomic apply,
config result и объявление фактических intervals. Тогда backend сможет
различать pending/applied/rejected. Сейчас такой механизм не реализован;
произвольный shell не используется.
