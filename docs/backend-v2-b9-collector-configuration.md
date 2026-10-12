# B9 — управление конфигурацией Collector

Этап реализован только в backend. Код Agent/Collector и frontend не изменён. Совместимость проверяется с финальным Collector `215d33a4a5fbc2a12c57d31e516b0ad33f7ae574`.

## 1. Что теперь можно сделать

Администратор InfraPulse может добавить управляемые SNMP, RouterOS, SIP, Redfish и IPMI targets, включить/выключить их, поменять их интервалы и передать площадку как metadata hint. Collector получает полный набор через уже существующий gRPC stream и применяет его без перезапуска.

Backend хранит отдельно желаемую конфигурацию и последнюю подтверждённую применённую. Поэтому ответ API «принято» не превращается в ложное сообщение «оборудование уже опрашивается». Все изменения имеют revision, digest, автора, причину и историю.

Существующие local targets из YAML остаются у владельца Collector. Пустой managed snapshot удаляет только управляемый набор. Нельзя отправить через этот API пароль, произвольный YAML, путь к ключу, shell command или изменения локальной политики безопасности.

## 2. Как проходит изменение

1. Администратор читает текущий `GET /api/collectors/{id}/config` и его `desired.revision`; если desired ещё нет — использует `0`.
2. Отправляет `PUT` с полным `targets`, текущей `expected_revision`, `confirmed=true`, причиной и уникальным `Idempotency-Key`.
3. Backend повторно проверяет действующую роль и сессию администратора в PostgreSQL. Проверяет закрытую структуру, адреса, протоколы, периоды и ссылки на credentials.
4. Backend применяет ровно fixed v1 defaults Collector, сортирует targets, вычисляет SHA-256 специального canonical representation из контракта Agent. Это не хеш JSON.
5. В одной транзакции создаёт revision, журналирует изменение и переводит desired state в `pending`. Номер новой ревизии больше всех известных ранее номеров, включая отклонённые Collector.
6. Фоновый worker выбирает актуальную ревизию для подключённой сессии. Сначала сохраняет попытку доставки в БД, затем отправляет `DesiredConfigSnapshot` через общий сериализованный sender.
7. Collector проверяет свою local policy и сообщает `received`, `validating`, `applied`, `rejected` или `failed`. Backend сохраняет сообщение и его projection до application ACK.
8. Для положительного результата standalone `ConfigApplyStatus` backend показывает `applied_reported`. После matching runtime state из Hello/Heartbeat показывает `applied`. Timestamp heartbeat дополнительно сравнивается с `started_at` текущего процесса Collector; сохранённый heartbeat прежнего процесса не подтверждает новый runtime. Такая дополнительная проверка нужна, потому что сообщения в durable queue могут пережить перезапуск Collector.
9. `in_sync=true` появляется, когда desired revision/digest совпали с подтверждёнными applied, текущая сессия подключена, и нет более новой ожидающей конфигурации.

При ошибке Collector продолжает свою последнюю рабочую конфигурацию. Backend сохраняет прежние applied revision/digest и отдельно показывает отказ новой. Нельзя исправлять уже отправленный номер ревизии: исправление оформляется новой ревизией.

При переподключении snapshot получает новый session ID, сохраняя revision/digest. Если Hello уже доказывает применение известной backend конфигурации, лишняя отправка не нужна. Старая сессия не получает настройку новой. После потери соединения `in_sync=false`.

## 3. Что включить перед использованием

Обычный запуск backend остаётся совместимым с предыдущим этапом. Управление по умолчанию выключено:

```dotenv
COLLECTOR_CONFIG_ENABLED=false
```

Для включения нужны одновременно:

- `COLLECTOR_CONFIG_ENABLED=true` в backend;
- `AUTH_MODE=required` и действующий администратор;
- серверный gRPC certificate/key и `TLS_CLIENT_CA_FILE` для проверяемого client mTLS;
- URI сертификата Collector `spiffe://infrapulse/agent/<agent_id>` с соответствующим UUID;
- финальный Collector, который сообщает capability `managed_config_v1`;
- local opt-in `remote_management.enabled=true` у Collector;
- разрешённые у Collector протоколы, destinations, credential refs и CA refs.

`HelloAck.managed_config_v1=true` выдаётся только подходящему verified mTLS peer после этих условий. Plaintext, только серверный TLS, старый Collector или выключенная local permission не включают управление. Наличие UUID или сообщения capability само по себе не даёт право доступа.

Backend отказывается запускаться с включённым управлением без REST authentication и настроенного gRPC mTLS. Настройки Collector выполняет его ответственный разработчик/администратор; код Agent править не требуется.

Для контейнерного запуска используется существующий [PKI overlay](stage8-pki.md), подготовленный каталог сертификатов и соответствующие переменные `.env`. После подготовки:

```powershell
docker compose -f docker-compose.yml -f docker-compose.pki.yml up --build -d
```

Не создавайте новую БД и не удаляйте существующий PostgreSQL volume. Backend сам применяет миграцию `031_collector_configuration.sql`, сохраняя таблицы прежних этапов.

Пример фрагмента local policy Collector для обсуждения с его владельцем:

```yaml
remote_management:
  enabled: true
  allowed_protocols: [snmp, routeros, sip, redfish, ipmi]
  allowed_destinations: [192.0.2.0/24]
  allowed_credential_refs: [snmp-read, router-read, bmc-read]
```

Это пример, а не готовый адрес вашего стенда. Local credentials должны уже существовать у Collector. Backend не знает их значения. `ca_ref` тоже является именем из local allowlist, а не путём к файлу. Local max_targets/max_bytes/min_period могут быть строже backend лимитов — в этом случае Collector отклонит snapshot целиком.

## 4. API для разработчика frontend

| Маршрут | Что возвращает |
|---|---|
| `GET /api/collectors/{id}/config` | Capability/opt-in, connectivity/session, desired, last applied, status/error и `in_sync` |
| `PUT /api/collectors/{id}/config` | `202 Accepted`, сохранённую desired revision; применение асинхронно |
| `GET /api/collectors/{id}/config/history?limit=50&offset=0` | Полные immutable revisions, actor/reason/site и время создания |
| `GET /api/collectors/{id}/config/events?limit=50&offset=0` | Попытки доставки и reported statuses с признаком `correlated` |

Все четыре маршрута требуют `access.manage` — текущую роль admin. Cookie-мутации требуют прежний CSRF token и допустимый Origin. Bearer используется в примере ниже. Responses/target options описаны в `/openapi.json`, версия `0.30.0`.

`site` относится к managed configuration Collector: заполняет пустой `target.location.site` и участвует в digest через target metadata. Непустой site конкретного target сохраняется. Это не B6 `physical_locations.id` и не подтверждённое место Asset в стойке: проверенные placement/занятые U остаются отдельным механизмом.

В frontend нужны как минимум две колонки: «Желаемая revision» и «Последняя подтверждённая applied revision». `pending/received/validating/applied_reported` показываются как незавершённое применение. `rejected/failed` требуют просмотра error code и исправления. Не объявляйте результат по одному HTTP 202.

`expected_revision` защищает от потери одновременного изменения другим администратором. `409` — перечитать state и сравнить изменения, а не автоматически перезаписать чужой набор. Повтор того же запроса с тем же ключом возвращает ту же revision и `Idempotent-Replay: true`. Другой запрос с тем же ключом получает `409`.

Backend хранит revisions как положительный bigint; неизвестные uint64 statuses Collector архивируются без переполнения. Если Collector сообщает номер вне доступного диапазона для следующей ревизии, backend возвращает conflict и не оборачивает счётчик в ноль. Для JSON-клиента с очень большими числами требуется точное представление int64; обычные номера демонстрации малы.

## 5. Первый запрос из PowerShell

Сначала запустите backend с mTLS, создайте администратора по [инструкции доступа](access-stage5.md) и подключите Collector с его local opt-in. ID берите из `/api/agents`, это UUID Collector, а не Asset ID.

В своём терминале выполните вход. Реальный пароль в чат или репозиторий отправлять не нужно:

```powershell
$apiBase = 'http://127.0.0.1:8080'
$loginCredential = Get-Credential -UserName 'oleg' -Message 'Локальный администратор InfraPulse'
$loginBody = @{
  username = $loginCredential.UserName
  password = $loginCredential.GetNetworkCredential().Password
} | ConvertTo-Json
$session = Invoke-RestMethod -Method Post -Uri "$apiBase/api/auth/token" -ContentType 'application/json' -Body $loginBody
$headers = @{ Authorization = "Bearer $($session.access_token)" }
$collectorId = 'UUID-ВАШЕГО-COLLECTOR'
$current = Invoke-RestMethod -Uri "$apiBase/api/collectors/$collectorId/config" -Headers $headers
$revision = 0
if ($null -ne $current.desired) { $revision = $current.desired.revision }
```

Пример полного desired набора с одной SIP-точкой:

```powershell
$body = @{
  expected_revision = $revision
  confirmed = $true
  reason = 'Добавляем тестовую АТС согласованного стенда'
  site = 'demo-site'
  targets = @(
    @{
      target_id = 'demo-pbx'
      protocol = 'sip'
      enabled = $true
      endpoint = '192.0.2.10'
      sip = @{ transport = 'udp'; port = 5060 }
      polling = @{ fast_interval = '3s'; timeout = '2s' }
    }
  )
} | ConvertTo-Json -Depth 12
$writeHeaders = @{
  Authorization = $headers.Authorization
  'Idempotency-Key' = [guid]::NewGuid().ToString()
}
$desired = Invoke-RestMethod -Method Put -Uri "$apiBase/api/collectors/$collectorId/config" -Headers $writeHeaders -ContentType 'application/json; charset=utf-8' -Body ([Text.Encoding]::UTF8.GetBytes($body))
$desired
Invoke-RestMethod -Uri "$apiBase/api/collectors/$collectorId/config" -Headers $headers
```

Адрес `192.0.2.10` — документационный. Замените его разрешённым адресом своего стенда. Для SIP не передаётся credential_ref. Для других протоколов добавьте local reference и соответствующий тип адреса. Примеры всех пяти определений: [JSON](examples/managed-collector-targets.json).

**PUT заменяет весь managed набор.** При редактировании одной цели отправляйте остальные, которые хотите оставить. Иначе отсутствующая цель удалится после успешного apply.

Для смены периода измените `polling`, перечитайте current desired revision и отправьте новый полный snapshot с новым ключом. Периоды SNMP/RouterOS/SIP — 3 секунды–24 часа; Redfish/IPMI — 10 секунд–24 часа. Hardware использует `medium_interval`. SIP использует `fast_interval`; его medium/inventory/topology должны отсутствовать или быть нулём. Backend сохраняет protobuf Duration без обрезания.

`enabled=false` останавливает управляемую цель после успешного apply. Отключённые definitions тоже проходят валидацию. Для явной очистки используйте `targets=[]` и `allow_empty_snapshot=true`; подтверждение и reason обязательны. История наблюдений в backend не удаляется. Историческая карточка отключённой цели сама по себе не означает, что Collector продолжает её опрашивать.

## 6. Свежесть и прежние monitoring endpoints

Прежние `PUT /api/devices/{id}/monitoring` и `PUT /api/telephony/targets/{id}/monitoring` остаются настройками ожидаемого периода backend. Они не отправляют managed snapshot и не меняют scheduler автоматически. Для реального изменения опроса используется новый Collector API.

После runtime-confirmed apply backend наследует периоды только для точно принадлежащих managed targets с typed Collector/Target/protocol и `provenance=real`. Имена, IP или vendor не заменяют эту проверку. Для SIP `TelephonyHealth.target_id` остаётся raw ID (`demo-pbx`), тогда как SourceRef target имеет `remote:demo-pbx`; проверяются оба.

В карточках появляется `monitoring.origin=collector_applied`, `delivery_mode=managed_collector_stream`, `application_status=applied`. Если operator/admin ранее явно задал ручную freshness policy, она сохраняется как `device_override/target_override` и имеет приоритет. Desired/rejected/неизвестная revision ожидаемый период не меняет. Факты, которые впервые придут после apply, также получат подтверждённый период.

Дробный период в snapshot сохраняется точно; целочисленный freshness API округляет его вверх. Отключение связи с Collector отдельно влияет на availability, независимо от частоты data polling.

B8 hardware current/history автоматически увеличивает окно свежести до `max(300s, 3 × applied period)` для соответствующего типа наблюдения. Инвентаризация использует inventory period, health/metric — telemetry period. У каждого item есть `freshness_max_age_seconds`. Явный query `max_age_seconds` всегда приоритетен. SIM и чужие binding не получают это исключение.

## 7. Хранение и надёжность

Миграция 031 создаёт `collector_config_state`, `collector_config_revisions`, `collector_config_deliveries`, `collector_config_events`. Прежние monitoring/telephony policies получают origin/managed_revision для разделения ручных и подтверждённых периодов.

Worker проверяет очередь раз в секунду, берёт не больше 16 Collector за проход; повторная попытка допускается через 10 секунд. Send имеет ограничение времени и использует тот же gate, что ACK и команды. Same revision/digest допускает безопасную повторную доставку полного snapshot; команды с побочным эффектом продолжают свой прежний механизм.

Устаревшие/некоррелированные statuses остаются в истории и архиве, не превращаясь в applied state. Повторные одинаковые heartbeat reports не раздувают config events и не переписывают policies. Raw error text Collector не выводится в management REST: используется фиксированный code или безопасный общий code. Snapshot содержит logical references, но не значения credentials.

Функция рассчитана на текущий одиночный Controller. HA/разделение по tenant и развёртывание нескольких backend owners — B16; наличие очереди не является заявлением о готовом кластере.

## 8. Проверки и дальнейшие этапы

[Отчёт B9](backend-v2-b9-validation.md) разделяет проверки кода/БД, реальный процесс неизменённого Collector с протокольными fixtures и ещё необходимую приёмку оборудования команды.

Реализован этап B9 окончательного плана. Следующий — B10 Ansible. B10–B16, новый frontend и production deployment не объявляются завершёнными. Backend до B8 и все его прежние маршруты остаются в этом архиве.
